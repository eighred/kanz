package frtb

import "math"

// Curvature risk charge (PARITY-03g, the MAR21.101 aggregation). The SBM delta
// charge misses the non-linear P&L of options; curvature captures it from two
// shocked revaluations per risk factor. As with delta, the INPUTS are data (the
// "sensitivities are an INPUT" stance): CVR⁺/CVR⁻ per factor are the shocked-
// minus-delta P&L numbers the risk engine computes upstream (V(up)−V−RW·s and
// V(down)−V+RW·s, sign-flipped per the standard so a LOSS is positive).
//
// Aggregation (per risk class): for each direction the bucket charge keeps
// only positive CVRs on the diagonal and zeroes cross terms between two
// negative CVRs (the ψ function); correlations are the SQUARED delta
// correlations. The class charge is the worse direction, and the whole thing
// runs under the three correlation scenarios, worst taken — like delta.

// CurvatureSensitivity is one factor's curvature exposure: the CVR⁺ (up-shock)
// and CVR⁻ (down-shock) loss numbers, tagged like a delta sensitivity.
type CurvatureSensitivity struct {
	RiskClass string
	Bucket    string
	Factor    string
	Up, Down  float64
}

// CurvatureCharge is the curvature capital: per risk class the worst of the
// three correlation scenarios of the worse shock direction, summed across
// classes. Correlations come from the same Params as delta (squared per the
// standard).
func CurvatureCharge(sens []CurvatureSensitivity, params Params) float64 {
	byClass := map[string][]CurvatureSensitivity{}
	for _, s := range sens {
		byClass[s.RiskClass] = append(byClass[s.RiskClass], s)
	}
	var total float64
	for class, cs := range byClass {
		p, ok := params[class]
		if !ok {
			continue
		}
		m := curvatureScenario(cs, p, Low)
		for _, sc := range []Scenario{Medium, High} {
			if c := curvatureScenario(cs, p, sc); c > m {
				m = c
			}
		}
		total += m
	}
	return total
}

// curvatureScenario is the class charge under one correlation scenario: the
// worse of the up and down aggregates (one consistent direction across the
// whole class, per MAR21.101).
func curvatureScenario(cs []CurvatureSensitivity, p ClassParams, s Scenario) float64 {
	rho := scaleCorr(p.IntraCorr, s)
	gamma := scaleCorr(p.InterCorr, s)
	up := curvatureAgg(cs, rho*rho, gamma*gamma, func(c CurvatureSensitivity) float64 { return c.Up })
	down := curvatureAgg(cs, rho*rho, gamma*gamma, func(c CurvatureSensitivity) float64 { return c.Down })
	return math.Max(up, down)
}

// curvatureAgg aggregates one shock direction: per bucket
// K_b = √max(0, Σ max(CVR,0)² + Σ_{k≠l} ρ²·ψ·CVRₖ·CVRₗ), then across buckets
// with γ² and ψ on the clamped bucket sums.
func curvatureAgg(cs []CurvatureSensitivity, rho2, gamma2 float64, pick func(CurvatureSensitivity) float64) float64 {
	// Net per (bucket, factor) first, delta-style.
	type fk struct{ bucket, factor string }
	netted := map[fk]float64{}
	for _, c := range cs {
		netted[fk{c.Bucket, c.Factor}] += pick(c)
	}
	byBucket := map[string][]float64{}
	for k, v := range netted {
		byBucket[k.bucket] = append(byBucket[k.bucket], v)
	}

	var sumKsq float64
	var sbs []float64
	for _, cvrs := range byBucket {
		var diag, cross, sum float64
		for i, a := range cvrs {
			if a > 0 {
				diag += a * a
			}
			sum += a
			for j, b := range cvrs {
				if i != j {
					cross += rho2 * psi(a, b) * a * b
				}
			}
		}
		kb := math.Sqrt(math.Max(0, diag+cross))
		sumKsq += kb * kb
		sbs = append(sbs, clampF(sum, -kb, kb))
	}
	v := sumKsq
	for i, a := range sbs {
		for j, b := range sbs {
			if i != j {
				v += gamma2 * psi(a, b) * a * b
			}
		}
	}
	return math.Sqrt(math.Max(0, v))
}

// psi zeroes the cross term when both CVRs are negative (MAR21.101's ψ) — two
// gains never net into a capital reduction.
func psi(a, b float64) float64 {
	if a < 0 && b < 0 {
		return 0
	}
	return 1
}

func clampF(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}
