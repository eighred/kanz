package alternatives

import (
	"errors"
	"math"
	"math/big"
	"sort"
	"time"
)

// CashFlow is one dated external cashflow from the investor's perspective:
// negative = capital paid in (a call), positive = capital returned (a
// distribution). Amounts are float64 — IRR/PME are derived statistics, the EVT-14
// rule that permits double for returns/ratios (money stays exact in the Position
// accounting; it lands as float at the metric boundary).
type CashFlow struct {
	Date   time.Time
	Amount float64
}

// Multiples are the private-asset capital multiples (ALT-01c). TVPI = DPI + RVPI
// by construction (total value = realized + residual, over paid-in capital).
type Multiples struct {
	// TVPI = (distributed + NAV) / paid-in — total value to paid-in.
	TVPI float64
	// DPI = distributed / paid-in — realized (cash-on-cash) multiple.
	DPI float64
	// RVPI = NAV / paid-in — residual (unrealized) multiple.
	RVPI float64
}

// ErrNoMultiples is returned when nothing has been drawn, so every multiple is a
// division by zero.
//
// IT IS AN ERROR AND NOT A ZERO, and the two states it separates are not close
// together (#623): a commitment with nothing called has no multiple at all, while
// a fund that drew capital and returned none has TVPI 0.0 as a MEASURED FACT. One
// is a fund that has not started; the other is a total loss. As zeros they were
// the same three bytes on the wire.
//
// THE SIBLING METRIC IN THE SAME RESPONSE BODY ALREADY DID THIS. IRR returns an
// error for its undefined case and handlePosition omits the key; the multiples
// five lines above it emitted 0. This makes the two agree, which is the whole
// repair — the doc here used to call the zero "the same degraded-to-zero
// discipline the risk measures use", and those measures have since moved the
// other way too (#509, #527: a measure computed over nothing must say so).
var ErrNoMultiples = errors.New("alternatives: no capital has been called, so the multiples are undefined")

// ComputeMultiples returns TVPI/DPI/RVPI from a position's paid-in (called),
// distributed, and residual NAV, or ErrNoMultiples when nothing has been called.
//
// A ZERO RETURNED WITH A NIL ERROR IS A REAL ZERO and callers may report it as
// one: capital drawn and nothing returned is a total loss, which is exactly the
// figure an investor needs to see.
func ComputeMultiples(p *Position) (Multiples, error) {
	paidIn := ratToFloat(p.Called)
	if paidIn <= 0 {
		return Multiples{}, ErrNoMultiples
	}
	dist := ratToFloat(p.Distributed)
	nav := ratToFloat(p.NAV)
	return Multiples{
		TVPI: (dist + nav) / paidIn,
		DPI:  dist / paidIn,
		RVPI: nav / paidIn,
	}, nil
}

// IRRFlows builds the IRR cashflow stream for a position as of the NAV date: the
// dated calls (negative) and distributions (positive), plus the residual NAV as a
// terminal positive flow at the valuation date — the convention that an open
// fund's IRR treats its current NAV as if it were distributed today.
func IRRFlows(p *Position) []CashFlow {
	flows := append([]CashFlow(nil), p.Flows...)
	if p.NAV.Sign() != 0 {
		navDate := p.NAVDate
		if navDate.IsZero() && len(flows) > 0 {
			navDate = flows[len(flows)-1].Date
		}
		flows = append(flows, CashFlow{Date: navDate, Amount: ratToFloat(p.NAV)})
	}
	return flows
}

// errNoIRR distinguishes an unavailable return from a measured zero return.
var errNoIRR = errors.New("alternatives: IRR undefined (need flows of both signs)")

// IRR is the annualized money-weighted return that sets the net present value of
// the cashflows to zero: Σ CF_i / (1+r)^{t_i} = 0, with t_i in years from the
// first flow (actual/365). It requires flows of both signs (a paid-in and a
// returned/residual leg). Solved by bisection on a wide bracket — robust for the
// single sign change a normal fund profile has, where Newton can overshoot.
func IRR(flows []CashFlow) (float64, error) {
	if len(flows) < 2 {
		return 0, errNoIRR
	}
	ordered := append([]CashFlow(nil), flows...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Date.Before(ordered[j].Date) })

	// Flows at one instant have the same discount factor. Net them exactly in
	// the existing float input domain before testing signs: otherwise a journal
	// whose discounted value is identically zero appears to have every IRR.
	type datedAmount struct {
		Date   time.Time
		Amount *big.Rat
	}
	netted := make([]datedAmount, 0, len(ordered))
	for i := 0; i < len(ordered); {
		at := ordered[i].Date
		total := new(big.Rat)
		for i < len(ordered) && ordered[i].Date.Equal(at) {
			if math.IsNaN(ordered[i].Amount) || math.IsInf(ordered[i].Amount, 0) {
				return 0, errNoIRR
			}
			total.Add(total, new(big.Rat).SetFloat64(ordered[i].Amount))
			i++
		}
		if total.Sign() != 0 {
			netted = append(netted, datedAmount{Date: at, Amount: total})
		}
	}
	if len(netted) < 2 {
		return 0, errNoIRR
	}
	var pos, neg bool
	scale := new(big.Rat)
	reference := netted[0].Date
	for _, f := range netted {
		magnitude := new(big.Rat).Abs(f.Amount)
		if magnitude.Cmp(scale) > 0 {
			scale.Set(magnitude)
			reference = f.Date
		}
		switch {
		case f.Amount.Sign() > 0:
			pos = true
		case f.Amount.Sign() < 0:
			neg = true
		}
	}
	if !pos || !neg {
		return 0, errNoIRR
	}

	years := func(d, from time.Time) float64 {
		// Timestamp ranges exceed time.Duration's approximately 292-year range.
		// Borrow before converting so a gap across a second boundary does not
		// subtract nearly equal floats and lose the short interval.
		seconds, nanos := d.Unix()-from.Unix(), d.Nanosecond()-from.Nanosecond()
		if seconds > 0 && nanos < 0 {
			seconds--
			nanos += 1e9
		} else if seconds < 0 && nanos > 0 {
			seconds++
			nanos -= 1e9
		}
		return (float64(seconds) + float64(nanos)/1e9) / (365 * 24 * 60 * 60)
	}
	type term struct {
		date      time.Time
		logAmount float64
		money     *big.Rat
	}
	logMagnitude := func(amount *big.Rat) float64 {
		// Approximation only selects a pivot. Keep grouped money rational;
		// converting an exact sum back to float would discard same-time legs.
		value := new(big.Float).SetPrec(64).SetRat(amount)
		mantissa := new(big.Float)
		exponent := value.MantExp(mantissa)
		m, _ := mantissa.Float64()
		return math.Log(m) + float64(exponent)*math.Ln2
	}
	logScale := logMagnitude(scale)
	terms := make([]term, 0, len(netted))
	for _, f := range netted {
		logAmount := logMagnitude(new(big.Rat).Abs(f.Amount)) - logScale
		terms = append(terms, term{f.Date, logAmount, f.Amount})
	}
	expRat := func(x float64) (*big.Rat, bool) {
		// Represent exp(x) as a finite mantissa times an exact binary power.
		// Range-reduce before even a nonzero subnormal loses relative accuracy.
		// This bound covers the complete canonical Timestamp range throughout
		// the fixed rate bracket, while arbitrary Go dates cannot allocate an
		// unbounded integer. It is a numerical refusal, never a default rate.
		k := math.Floor(x / math.Ln2)
		if math.IsNaN(k) || math.Abs(k) > 262144 {
			return nil, false
		}
		result := new(big.Rat).SetFloat64(math.Exp(x - k*math.Ln2))
		power := new(big.Int).Lsh(big.NewInt(1), uint(math.Abs(k)))
		if k < 0 {
			result.Quo(result, new(big.Rat).SetInt(power))
		} else {
			result.Mul(result, new(big.Rat).SetInt(power))
		}
		return result, true
	}
	npvSign := func(r float64) (int, bool) {
		logRate, largest, pivot := math.Log1p(r), math.Inf(-1), 0
		for i, f := range terms {
			if value := f.logAmount - years(f.date, reference)*logRate; value > largest {
				largest, pivot = value, i
			}
		}
		// A common discount reference preserves the root. Keep short intervals
		// near the dominant cashflow, including small future flows amplified by
		// a negative rate. Logs only choose that reference; they never decide NPV.
		total := new(big.Rat)
		for _, f := range terms {
			x := -years(f.date, terms[pivot].date) * logRate
			if math.Abs(x) < .5 {
				// Resolve principal cancellation exactly in the existing float
				// input domain BEFORE adding the small discount increment. This
				// preserves both tiny residuals and returns split into many legs.
				total.Add(total, f.money)
				total.Add(total, new(big.Rat).Mul(f.money, new(big.Rat).SetFloat64(math.Expm1(x))))
			} else {
				factor, ok := expRat(x)
				if !ok {
					return 0, false
				}
				total.Add(total, new(big.Rat).Mul(f.money, factor))
			}
		}
		return total.Sign(), true
	}

	// Bracket: NPV is monotone decreasing in r over a normal profile. Search for a
	// sign change between -0.9999 and a large upper rate.
	lo, hi := -0.9999, 10.0
	fLo, loOK := npvSign(lo)
	fHi, hiOK := npvSign(hi)
	if !loOK || !hiOK {
		return 0, errNoIRR
	}
	if fLo == 0 {
		return lo, nil
	}
	if fHi == 0 {
		return hi, nil
	}
	if fLo == fHi {
		return 0, errNoIRR // no root in the bracket
	}
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		fMid, ok := npvSign(mid)
		if !ok {
			return 0, errNoIRR
		}
		if fMid == 0 || hi-lo <= 1e-12*math.Max(1, math.Abs(mid)) {
			return mid, nil
		}
		if fLo != fMid {
			hi = mid // fHi is never read again: the loop brackets on fLo's sign
		} else {
			lo, fLo = mid, fMid
		}
	}
	return 0, errNoIRR
}

// PME is the Kaplan-Schoar public-market-equivalent: it scales every cashflow by
// the public benchmark's total return from the flow date to the valuation date
// (level_end / level_at_flow), then takes the ratio of future-valued distributions
// (plus the residual NAV, undiscounted at the valuation date) to future-valued
// contributions. PME > 1 means the private fund beat the public benchmark on a
// capital-weighted basis; PME < 1 means an index investment would have done
// better.
//
// benchmarkLevel returns the public benchmark index level at a date (the PERF-01c
// benchmark, an input — alternatives does not reach into the performance/risk
// modules). It must be positive at every flow date and the valuation date.
func PME(p *Position, valuationDate time.Time, benchmarkLevel func(time.Time) float64) (float64, error) {
	endLevel := benchmarkLevel(valuationDate)
	if endLevel <= 0 {
		return 0, errors.New("alternatives: PME needs a positive benchmark level at the valuation date")
	}
	var fvContrib, fvDist float64
	for _, f := range p.Flows {
		lvl := benchmarkLevel(f.Date)
		if lvl <= 0 {
			return 0, errors.New("alternatives: PME needs a positive benchmark level at every flow date")
		}
		scale := endLevel / lvl
		if f.Amount < 0 {
			fvContrib += -f.Amount * scale // contributions: future-valued at the benchmark
		} else {
			fvDist += f.Amount * scale // distributions: future-valued at the benchmark
		}
	}
	// The residual NAV is already at the valuation date — no scaling.
	fvDist += ratToFloat(p.NAV)
	if fvContrib <= 0 {
		return 0, errors.New("alternatives: PME undefined with no contributions")
	}
	return fvDist / fvContrib, nil
}
