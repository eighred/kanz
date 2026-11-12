# Term-structure compatibility kernels

This bounded #1534 family covers continuously compounded zero curves, conversion
from annual/semiannual quotes, sequential par bootstrapping, flat-zero
extrapolation, linear-zero/log-discount interpolation, forward and par queries.
The pure Rust library owns scalar analytics only. Go retains quote resolution,
PIT storage, strip coverage, capture/artifact identity, model approval, canonical
Decimals and financial authority. No writer, new service or activation is added.

The existing default-off bond shadow now uses `Curve::zero_with_bump`; its copied
interpolation was removed. Its canonical request/result schemas, 256-pillar,
100-year and 1,201-payment limits, timestamp validation, schedule identity,
tenant attribution, process supervision and Go-owned selection remain intact.
Pure-library tests do not prove those boundaries; the actual bond supervisor's
separate CI job remains required.

## Methods and failure semantics

Tenors are already resolved years; rates are annual fractions, with continuous
zeros internally. A one-basis-point parallel bump is `0.0001` before interpolation.
Calendar/day-count selection and multi-curve collateral/projection conventions
are not inferred here. Par inputs explicitly use the declared pillar dates as
their common coupon schedule. The separate mixed quote family below preserves
the existing Go deposit/future/swap conventions; Go retains its authority.

- Finite strictly positive ascending tenors are required after sorting. Annual
  rates must exceed -1, semiannual rates -2; continuous negative rates are valid.
  Unknown conventions refuse. A positive discount greater than one is valid at
  negative rates; interpolation alone does not certify arbitrage-free inputs.
- Compounding uses `log1p`. Par bootstrap uses a retained prefix annuity and the
  difference of two `log1p` terms; par repricing uses `expm1`. These preserve tiny
  nonzero rates that `log(1+r)` or `1-DF` erases. See the primary
  [Go math implementation](https://go.dev/src/math/log1p.go), checked 2026-11-12.
- The par recurrence is O(n), replacing repeated O(n^2) prefix sums. Sorting is
  still part of total construction cost. Forward queries analytically integrate
  each interpolation segment rather than subtract nearly equal log-discounts.
  This fixes a controlled adjacent-tenor example returning 6.25% for a 5% forward.
- Go now refuses invalid definitions at construction; scalar unavailable queries
  return NaN at its existing float API. Rust returns `Result` without a candidate.
  Empty/invalid coupon schedules, reversed/nonfinite forward intervals, discount
  overflow/underflow-to-zero and nonfinite arithmetic are unavailable. For valid
  curves, finite nonpositive discount times retain the legacy known value 1.
- Rust permits at most 256 pillars and 4,096 coupon dates. These are candidate
  work bounds, not a new market maturity policy; the Go zero/par APIs are not
  newly capped. Go mixed-swap calibration separately refuses nonfinite quotes
  and annual schedules exceeding 4,096 periods before generating dates.

Before-repair Go tests reproduced refusal of a valid -1% par strip, loss of a
`1e-20` rate, the adjacent-tenor forward error, NaN-query panic and successful
construction with unknown/invalid inputs. The repair also tests that failed
refresh cannot install/capture a new curve or displace its prior PIT version.
That is an in-process store/callback check, not durable publication proof.

## Independent qualification

The negative annual par example has exact discounts `100/99` and `10000/9801`.
`reference/reference.py` independently solves all par equations by dense pivoted
Gaussian elimination, uses barycentric interpolation and high-precision direct
log-discount differences. It does not call production pricing helpers, retain a
prefix annuity, or copy piecewise-forward integration. Ten references at 80 and
110 Decimal digits must agree within `1e-45*(1+abs(value))`; 40 significant digits
are retained. Near-zero cases also have explicit relative-accuracy controls.

The source-bound Go corpus contains **876 unique cases: 832 values, 29 refused
definitions and 15 unavailable queries/results**. Cases cover both constructions,
both interpolations, three compounding conventions, negative/zero/tiny/high rates,
unsorted/irregular dates, 256 pillars, short/long extrapolation, adjacent forward
endpoints, bumps, absent schedules and IEEE invalid values. Every sorted pillar,
zero, discount, bumped zero, forward and par result is checked.
The private method is `go_zero_par_v2`; five negative controls include rejection
of obsolete v1 captures. Interior positive/negative/zero rates and their adjacent
binary64 queries are included in both input orders.

Known pillars return their stored continuous rate exactly, including the existing
pre-interpolation bump. Between pillars, both distances are computed directly and
their endpoint contributions are summed. This preserves a tiny endpoint beside a
larger rate without changing the linear-zero or log-discount model. The public
[linear](https://raw.githubusercontent.com/OpenGamma/Strata/main/modules/market/src/main/java/com/opengamma/strata/market/curve/interpolator/LinearCurveInterpolator.java)
and [log-linear](https://raw.githubusercontent.com/OpenGamma/Strata/main/modules/market/src/main/java/com/opengamma/strata/market/curve/interpolator/LogLinearCurveInterpolator.java)
definitions were checked 2026-11-13; no upstream implementation is included.
`reference/interpolation_reference.py` uses exact Fraction arithmetic for **12
curves / 84 queries**, rounding once at the result. Go independently checks the
same mathematical contract with `math/big.Rat`. The new knot/adjacent-query budget
is **16*EPS*abs(expected)** with no floor; known knot bits must match exactly.
The existing comparison budgets below are unchanged. CI regenerates the rational
fixture, runs its test and retains both hashes and results.

Comparison budgets are fixed before execution: exact tenor bits; discount error
at most `1024*EPS*pillar_count*abs(expected)`; rate-unit error at most the same
factor times the maximum of pillar-zero scale and expected magnitude (including
the known `1e-4` bump when applicable), plus `64*MIN_POSITIVE`. This accounts for
binary64 accumulation/conditioning; it is not money rounding or economic error.
No residual-based tolerance changes are permitted. Missing/relabelled source,
altered zero, duplicate ID and truncated inventory must refuse.

```sh
cd kanz
go run ./services/risk-engine/tools/curve_reference -source "$SOURCE_SHA" > "$CORPUS"
cd ../kanz-rs
python3 -B crates/term_structure/reference/reference.py --check
cargo test --locked -p kanz-term-structure --test numerical
cargo test --locked -p kanz-term-structure --test go_conformance missing_or_malformed_capture_refuses -- --exact
KANZ_CURVE_CORPUS="$CORPUS" KANZ_CURVE_SOURCE="$SOURCE_SHA" \
  cargo test --locked -p kanz-term-structure --test go_conformance fresh_go_oracle_is_required -- --ignored --exact --nocapture
```

An ordinary Cargo invocation ignores the fresh-oracle gate; that is not Go
compatibility evidence. The dedicated `term-structure` CI job explicitly runs it
and retains reference/corpus/lock hashes, source/tree, toolchains and all results.
The existing bond job exercises the changed actual worker through Go supervision.

[Aladdin's public capabilities](https://www.blackrock.com/aladdin/platforms/products/aladdin-risk)
(checked 2026-11-12) describe integrated multi-asset analytics, data quality,
scenario analysis and governance. They define useful capability benchmarks, not
an available specification for proprietary algorithms. This family establishes
specific numerical/compatibility evidence; it does not establish BlackRock
equivalence, superior pricing, production performance or whole-platform parity.
Those claims require representative model/market-data/portfolio and runtime
benchmarks. Existing data/terms/model owners and Go/OpenSSL delivery holds remain.

## Mixed deposit/future/swap calibration

`Curve::calibrate` is a bounded scalar candidate for Go's `curve.Calibrate`.
Inputs are annual simple-rate fractions for deposits/futures and annual par
swap rates. A future tenor is its period start; span zero keeps the existing
0.25-year default. Annual swap dates retain the existing `tenor-1e-9` stub rule.
This does not infer calendars, day counts, collateral or projection curves.

Deposits and futures carry log-discounts through `log1p` instead of rounding a
discount then taking log. If a future starts beyond the last solved pillar,
its start and end discounts are solved together under the declared interpolation.
A separate short-end anchor remains mandatory; no calendar or missing term is
inferred. A positive span that rounds away when added to its start refuses.
`quote_rate`/Go `QuoteRate` reprice simple instruments with `expm1` and forwards.

For last pillar `(a,z_a)`, future start `s>a`, end `e=s+h` and quoted span `h`,
let `F=log1p(r*h)/(e-s)`. The existing interpolation equations give:

- Linear zero: `alpha=s/(e-a)` and
  `z_e=(alpha/(1+alpha))*z_a + (1/(1+alpha))*F`.
- Log discount: `z_e=(a/e)*z_a + ((e-a)/e)*F`.

Normalized weights avoid subtracting nearly equal interpolation coefficients.
This is an O(1) future solve with no extra iteration budget. The separate full
repricing check still refuses an unrepresentable result; tolerances do not change.
The simple-forward discount ratio and linear/log-linear definitions are public
methodology: [Strata forward rates](https://raw.githubusercontent.com/OpenGamma/Strata/main/modules/pricer/src/main/java/com/opengamma/strata/pricer/rate/DiscountIborIndexRates.java)
and [interpolation definitions](https://strata.opengamma.io/apidocs/com/opengamma/strata/market/curve/interpolator/CurveInterpolators.html),
checked 2026-11-13. No Strata code or dependency is included.

The `.25y/3%` deposit followed by a `1y` start / `.25y` span / `3.5%` future
has independent identities `DF(1.25)=(400/403)*(800/807)^4` for log discounts,
and `DF(1.25)^2=(320000/325221)^5` for linear zeros. Both languages check these
direct rational consequences, in addition to quote repricing.

Swap roots retain Go's existing zero-rate domain **[-0.5, 1]**. Quote-scale
bracketing and bisection share at most **256 evaluations per swap**. Success
requires bracket width at most `64*EPS*max(abs(lo),abs(hi))` and quote residual
at most `64*EPS*abs(quote)`. Nonfinite evaluations, exhausted work, stagnant
brackets and roots outside the domain refuse. Exhausted work never promotes an endpoint.
See [Boost's root-finding contract](https://www.boost.org/latest/libs/math/doc/html/math_toolkit/roots_noderiv/bisect.html),
checked 2026-11-12, which distinguishes convergence from iteration exhaustion.
No Boost code or dependency is included.

The complete returned curve must reprice **every** input within
`128*EPS*abs(quote)` in annual rate fractions. There is no absolute tolerance
floor: zero quotes require zero, and erasing a nonzero quote fails. In particular,
a tiny forward on top of a much larger accumulated log-discount may be
unrepresentable in binary64 and must refuse. This is scalar analytic precision,
not permission to use floating-point money or relax canonical Decimal controls.
Go failure precedes recording, PIT installation and shadow capture; prior state
and coverage reporting remain intact. Rust has no publication or runtime route.

`reference/mixed_reference.py` independently solves swap-price and future
discount-ratio equations by damped Newton in log-discount using a high-precision
central derivative. It appends the candidate pillar before evaluating both future
endpoints, rather than copying production gap weights, log1p, ParRate objective
or bisection branches. Thirty references at **160
and 200 digits** must agree within **1e-45 relative error**, including near-zero
results; 40 significant digits are retained. Both oracle precisions separately
reprice their input quotes to that relative budget.

The fresh actual Go mixed corpus has **306 unique cases: 274 values / 32
refusals**. It includes both interpolations, negative/zero/tiny/high rates,
unsorted and irregular mixed strips, chained/irregular pillar gaps, 256 pillars,
domain edges, small interior deposits and invalid inputs. Every case also queries
the two-year knot and its immediately adjacent binary64 tenors. Independent tiny
interior references use exact Decimal images of those queries.
Every sorted tenor, zero, input repricing and queried zero/discount is compared.
Tenors are bit-exact; other fields use `2048*EPS*pillars*abs(expected)` with no
absolute floor. Missing/malformed captures, wrong source, altered zero, duplicate
identity, truncated inventory and obsolete `go_mixed_v1`/`go_mixed_v2` contracts
refuse in six negative controls. The new corpus method is `go_mixed_v3`; old
captures cannot qualify it.
CI explicitly invokes the otherwise
ignored `fresh_mixed_go_oracle_is_required` test and retains both references,
corpora, source/tree/digests and results. The parent zero/par and actual bond
supervisor gates remain required. None of this activates Rust calibration.
