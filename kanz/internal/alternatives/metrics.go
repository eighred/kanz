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
	netted := make([]CashFlow, 0, len(ordered))
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
		amount, _ := total.Float64()
		if math.IsInf(amount, 0) {
			return 0, errNoIRR
		}
		if amount != 0 {
			netted = append(netted, CashFlow{Date: at, Amount: amount})
		}
	}
	if len(netted) < 2 {
		return 0, errNoIRR
	}
	var pos, neg bool
	var scale float64
	for _, f := range netted {
		scale = math.Max(scale, math.Abs(f.Amount))
		switch {
		case f.Amount > 0:
			pos = true
		case f.Amount < 0:
			neg = true
		}
	}
	if !pos || !neg {
		return 0, errNoIRR
	}

	t0 := netted[0].Date
	years := func(d time.Time) float64 {
		// Timestamp ranges exceed time.Duration's approximately 292-year range.
		return (float64(d.Unix()-t0.Unix()) + float64(d.Nanosecond()-t0.Nanosecond())/1e9) / (365 * 24 * 60 * 60)
	}
	type term struct{ years, logAmount, sign float64 }
	terms := make([]term, 0, len(netted))
	for _, f := range netted {
		logAmount := math.Log(math.Abs(f.Amount) / scale)
		if math.IsInf(logAmount, -1) {
			logAmount = math.Log(math.Abs(f.Amount)) - math.Log(scale)
		}
		terms = append(terms, term{years(f.Date), logAmount, math.Copysign(1, f.Amount)})
	}
	npv := func(r float64) float64 {
		// A positive common rescaling preserves the root and its sign. Work in
		// log space so neither tiny money nor long dated discount factors can
		// underflow the convergence test or overflow the bracket calculation.
		maxLog := math.Inf(-1)
		for _, f := range terms {
			maxLog = math.Max(maxLog, f.logAmount-f.years*math.Log1p(r))
		}
		var base, v, correction float64
		for _, f := range terms {
			base += f.sign
			// Keeping exp(x)-1 separate preserves near-cancelling terms at
			// very short intervals, including a genuine zero annual return.
			term := f.sign*math.Expm1(f.logAmount-f.years*math.Log1p(r)-maxLog) - correction
			next := v + term
			correction = (next - v) - term
			v = next
		}
		return base + v
	}

	// Bracket: NPV is monotone decreasing in r over a normal profile. Search for a
	// sign change between -0.9999 and a large upper rate.
	lo, hi := -0.9999, 10.0
	fLo, fHi := npv(lo), npv(hi)
	if fLo == 0 {
		return lo, nil
	}
	if fHi == 0 {
		return hi, nil
	}
	if math.Signbit(fLo) == math.Signbit(fHi) {
		return 0, errNoIRR // no root in the bracket
	}
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		fMid := npv(mid)
		if fMid == 0 || hi-lo <= 1e-12*math.Max(1, math.Abs(mid)) {
			return mid, nil
		}
		if math.Signbit(fLo) != math.Signbit(fMid) {
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
