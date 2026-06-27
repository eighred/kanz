package xva

import "math"

// CreditCurve is a counterparty's credit term structure: piecewise-constant
// forward hazard rates and a recovery rate, the CDS-implied default model the
// valuation adjustments integrate against. Survival to t is Q(t)=exp(−∫₀ᵗ h),
// the probability the name has not defaulted by t.
type CreditCurve struct {
	// Tenors are ascending segment ends (years); Hazards[k] is the constant
	// forward hazard on (Tenors[k-1], Tenors[k]] (Tenors[-1]=0). A single
	// (tenor, hazard) pair is a flat curve.
	Tenors  []float64
	Hazards []float64
	// Recovery is R ∈ [0,1); LGD = 1−R is the loss given default.
	Recovery float64
}

// FlatHazard builds a flat-hazard curve out to a long horizon.
func FlatHazard(hazard, recovery float64) CreditCurve {
	return CreditCurve{Tenors: []float64{100}, Hazards: []float64{hazard}, Recovery: recovery}
}

// FromCDS builds a flat curve from a CDS par spread (decimal, e.g. 0.01 for
// 100bp) via the credit-triangle approximation hazard ≈ spread/(1−R).
func FromCDS(spread, recovery float64) CreditCurve {
	lgd := 1 - recovery
	h := 0.0
	if lgd > 0 {
		h = spread / lgd
	}
	return FlatHazard(h, recovery)
}

// LGD is the loss given default, 1−Recovery.
func (c CreditCurve) LGD() float64 { return 1 - c.Recovery }

// Survival returns Q(t) = exp(−∫₀ᵗ h(s) ds) by accumulating the piecewise-
// constant hazard. t≤0 ⇒ 1. Beyond the last tenor the final hazard extrapolates
// flat.
func (c CreditCurve) Survival(t float64) float64 {
	if t <= 0 || len(c.Hazards) == 0 {
		return 1
	}
	var integral, prev float64
	for k, end := range c.Tenors {
		h := c.Hazards[k]
		if t <= end {
			integral += h * (t - prev)
			return math.Exp(-integral)
		}
		integral += h * (end - prev)
		prev = end
	}
	// Past the last pillar: extrapolate the final hazard.
	integral += c.Hazards[len(c.Hazards)-1] * (t - prev)
	return math.Exp(-integral)
}

// DefaultProbability is the marginal default probability over (a, b]: Q(a)−Q(b).
func (c CreditCurve) DefaultProbability(a, b float64) float64 {
	return c.Survival(a) - c.Survival(b)
}
