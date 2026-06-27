// Package performance is the performance measurement & attribution layer
// (PERF-01): time-/money-weighted returns, benchmark-relative active return,
// Brinson attribution, and ex-post risk statistics. It is the "how did we do and
// why" reporting plane the LAKE-01 backtest plane and the MODEL-01b bitemporal
// store were built to support (ROI #28).
//
// # Outside the risk module — own seams, no impl reach-in
//
// performance lives at kanz/internal/performance, OUTSIDE kanz/internal/risk, so
// the RISK-02 arch boundary forbids it from importing the risk impl packages
// (compute/domain/factor). Like COMP-01 it therefore carries its OWN small seams
// — a Classifier for sector bucketing (attribution.go) and value/flow providers
// (valuation.go) — built on the shared wire schemas and the public marketdata
// store, rather than reaching into the risk internals. The price history it
// reads point-in-time IS the shared kanz/internal/marketdata/store (not risk-
// internal), so no boundary is crossed.
//
// # Float math, statistics not money
//
// Returns / weights / attribution effects are dimensionless derived statistics,
// so they are float64 — the EVT-14 rule that permits double for metrics but bans
// it for money/prices/sizes. A performance number is reported to a few decimals
// of a fraction, never summed into a balance.
package performance

import (
	"math"
	"time"
)

// Flow is an external cashflow into (+) or out of (−) the portfolio at Time —
// a contribution or withdrawal, NOT a trade (trades move value between holdings
// without changing total invested capital).
type Flow struct {
	Time   time.Time
	Amount float64
}

// SubPeriod is one valuation interval of a time-weighted return: the market
// value at the start, the net external flow applied at the start of the
// interval, and the market value at the end. The sub-period holding return is
// EndValue / (BeginValue + Flow) − 1, which neutralizes the flow's size (the
// time-weighting property — the manager is not credited or penalized for a
// client's contribution timing).
type SubPeriod struct {
	BeginValue float64
	Flow       float64
	EndValue   float64
}

// Return is the sub-period holding-period return. A non-positive invested base
// (BeginValue+Flow ≤ 0) yields 0 — a degenerate interval contributes nothing
// rather than a divide-by-zero / sign flip.
func (s SubPeriod) Return() float64 {
	base := s.BeginValue + s.Flow
	if base <= 0 {
		return 0
	}
	return s.EndValue/base - 1
}

// TimeWeightedReturn geometrically links the sub-period returns: Π(1+rᵢ) − 1.
// Empty input ⇒ 0. This is the GIPS time-weighted return — the standard measure
// of manager skill, independent of external cashflow timing.
func TimeWeightedReturn(periods []SubPeriod) float64 {
	growth := 1.0
	for _, p := range periods {
		growth *= 1 + p.Return()
	}
	return growth - 1
}

// Period is a measurement window for a money-weighted return: begin/end market
// values and the dated external flows within it.
type Period struct {
	Start, End time.Time
	BeginValue float64
	EndValue   float64
	Flows      []Flow
}

// ModifiedDietz is the flow-weighted money-weighted return approximation:
//
//	R = (V_end − V_begin − ΣF) / (V_begin + Σ wᵢ·Fᵢ)
//
// where wᵢ = (T − tᵢ)/T is the fraction of the window flow i was invested. It
// reflects cashflow timing (the investor's experience), unlike TWR. A
// non-positive average capital yields 0.
func (p Period) ModifiedDietz() float64 {
	total := p.End.Sub(p.Start).Seconds()
	if total <= 0 {
		return 0
	}
	var sumFlows, weighted float64
	for _, f := range p.Flows {
		sumFlows += f.Amount
		w := p.End.Sub(f.Time).Seconds() / total
		weighted += w * f.Amount
	}
	avgCapital := p.BeginValue + weighted
	if avgCapital <= 0 {
		return 0
	}
	gain := p.EndValue - p.BeginValue - sumFlows
	return gain / avgCapital
}

// IRR is the true money-weighted return: the rate r solving
//
//	V_begin·(1+r) + Σ Fᵢ·(1+r)^{(T−tᵢ)/yr} = V_end
//
// equivalently the NPV-zero discount rate of the cashflow stream
// (−V_begin at t=0, −Fᵢ at tᵢ, +V_end at T), expressed as an annualized rate.
// Solved by bisection over [-0.999, 10]; a stream with no sign change (no
// solution in range) returns the Modified Dietz approximation as a graceful
// fallback. Time is measured in years (365-day) from Start.
func (p Period) IRR() float64 {
	const yearSeconds = 365 * 24 * 3600.0
	yrs := func(at time.Time) float64 { return p.End.Sub(at).Seconds() / yearSeconds }
	// fv(r) = future value at End of all flows + begin, minus end value.
	fv := func(r float64) float64 {
		v := p.BeginValue * math.Pow(1+r, yrs(p.Start))
		for _, f := range p.Flows {
			v += f.Amount * math.Pow(1+r, yrs(f.Time))
		}
		return v - p.EndValue
	}
	lo, hi := -0.999, 10.0
	flo, fhi := fv(lo), fv(hi)
	if math.IsNaN(flo) || math.IsNaN(fhi) || flo*fhi > 0 {
		return p.ModifiedDietz() // no bracketed root — fall back
	}
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		fm := fv(mid)
		if flo*fm <= 0 {
			hi = mid
		} else {
			lo, flo = mid, fm
		}
	}
	return (lo + hi) / 2
}

// Annualize scales a fractional return over windowYears to a one-year
// equivalent: (1+r)^(1/windowYears) − 1. windowYears ≤ 0 ⇒ r unchanged. For
// windows under a year this EXTRAPOLATES (compounding a partial-year return to a
// full year), so callers gate annualization on windows ≥ 1y where it is honest.
func Annualize(r, windowYears float64) float64 {
	if windowYears <= 0 {
		return r
	}
	return math.Pow(1+r, 1/windowYears) - 1
}
