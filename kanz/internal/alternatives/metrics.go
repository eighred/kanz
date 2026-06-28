package alternatives

import (
	"errors"
	"math"
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

// ComputeMultiples returns TVPI/DPI/RVPI from a position's paid-in (called),
// distributed, and residual NAV. Paid-in of zero yields zero multiples (nothing
// has been drawn — the multiples are undefined, reported as zero rather than
// NaN/Inf, the same degraded-to-zero discipline the risk measures use).
func ComputeMultiples(p *Position) Multiples {
	paidIn := ratToFloat(p.Called)
	if paidIn <= 0 {
		return Multiples{}
	}
	dist := ratToFloat(p.Distributed)
	nav := ratToFloat(p.NAV)
	return Multiples{
		TVPI: (dist + nav) / paidIn,
		DPI:  dist / paidIn,
		RVPI: nav / paidIn,
	}
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

// errNoIRR is returned when an IRR cannot be solved (no sign change in the
// cashflows, or fewer than two flows).
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

	var pos, neg bool
	for _, f := range ordered {
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

	t0 := ordered[0].Date
	years := func(d time.Time) float64 { return d.Sub(t0).Hours() / 24 / 365 }
	npv := func(r float64) float64 {
		var v float64
		for _, f := range ordered {
			v += f.Amount / math.Pow(1+r, years(f.Date))
		}
		return v
	}

	// Bracket: NPV is monotone decreasing in r over a normal profile. Search for a
	// sign change between -0.9999 and a large upper rate.
	lo, hi := -0.9999, 10.0
	fLo, fHi := npv(lo), npv(hi)
	if fLo*fHi > 0 {
		return 0, errNoIRR // no root in the bracket
	}
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		fMid := npv(mid)
		if math.Abs(fMid) < 1e-9 {
			return mid, nil
		}
		if fLo*fMid < 0 {
			hi, fHi = mid, fMid
		} else {
			lo, fLo = mid, fMid
		}
	}
	return (lo + hi) / 2, nil
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
