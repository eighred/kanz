module CurveKernel

export calibrate, discount, InvalidInput, NumericalFailure
struct InvalidInput <: Exception end
struct NumericalFailure <: Exception end

# Inputs are captured analytic rates (fractions, not percentages) and year
# fractions. This kernel never converts money or changes canonical Decimal.
function discount(tenors, zeros, t)
    t <= 0 && return 1.0
    isempty(tenors) && throw(InvalidInput())
    i = searchsortedfirst(tenors, t)
    z = if i == 1
        zeros[1]
    elseif i > length(tenors)
        zeros[end]
    else
        fraction = (t - tenors[i-1]) / (tenors[i] - tenors[i-1])
        (1 - fraction) * zeros[i-1] + fraction * zeros[i]
    end
    exp(-t*z)
end

function checked_df(df)
    isfinite(df) && df > 0 || throw(NumericalFailure())
    df
end

function maturity(q)
    q.kind == 2 ? q.tenor + (q.span > 0 ? q.span : 0.25) : q.tenor
end

function calibrate(quotes)
    1 <= length(quotes) <= 256 || throw(InvalidInput())
    for q in quotes
        q.kind in (1, 2, 3) || throw(InvalidInput())
        all(isfinite, (q.tenor, q.span, q.rate)) || throw(InvalidInput())
        # Match existing nonpositive-span => 3M convention, but bound its
        # serialized value as well. Negative starting tenors are unsupported.
        0 <= q.tenor <= 100 && abs(q.span) <= 100 || throw(InvalidInput())
        0 < maturity(q) <= 100 || throw(InvalidInput())
    end
    ordered = sort(collect(quotes); by=maturity)
    tenors, zeros = Float64[], Float64[]
    for q in ordered
        t = maturity(q)
        isempty(tenors) || t > tenors[end] || throw(InvalidInput())
        df = if q.kind == 1
            checked_df(inv(1 + q.rate*q.tenor))
        elseif q.kind == 2
            isempty(tenors) && throw(InvalidInput())
            checked_df(discount(tenors, zeros, q.tenor) / (1 + q.rate*(q.span > 0 ? q.span : 0.25)))
        else
            # Solve the fixed-leg PV equality directly, rather than dividing
            # by the annuity to form Go's par-rate objective. Annual dates and
            # the final stub deliberately retain the existing convention.
            dates = Float64[]
            date = 1.0
            while date < t - 1e-9
                push!(dates, date)
                date += 1
            end
            push!(dates, t)
            trial_t = [tenors; t]
            trial_z = [zeros; 0.0]
            function pv_error(z)
                trial_z[end] = z
                previous, annuity = 0.0, 0.0
                for date in dates
                    annuity += (date - previous) * discount(trial_t, trial_z, date)
                    previous = date
                end
                value = 1 - exp(-z*t) - q.rate*annuity
                isfinite(value) || throw(NumericalFailure())
                value
            end
            lower, upper = -0.5, 1.0
            pv_error(lower) <= 0 <= pv_error(upper) || throw(NumericalFailure())
            for _ in 1:60
                middle = lower + (upper-lower)/2
                if pv_error(middle) < 0
                    lower = middle
                else
                    upper = middle
                end
            end
            checked_df(exp(-(lower + (upper-lower)/2)*t))
        end
        z = -log(df)/t
        isfinite(z) || throw(NumericalFailure())
        push!(tenors,t)
        push!(zeros,z)
    end
    tenors, zeros
end
end
