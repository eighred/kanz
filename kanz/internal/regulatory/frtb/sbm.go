// Package frtb is the REG-01b FRTB market-risk capital engine — the Fundamental
// Review of the Trading Book sensitivities-based method (SBM). It weights the
// portfolio's delta/vega sensitivities by supervisory risk weights and aggregates
// them within and across buckets under prescribed correlations, taking the worst
// of three correlation scenarios (the FRTB twist that SIMM lacks).
//
// # Boundary (RISK-02): sensitivities are an INPUT
//
// regulatory lives OUTSIDE kanz/internal/risk, so the arch test forbids importing
// the DERIV-01 / FI-01 Greek pricers. As with the COLL-01 SIMM, that is exactly
// how the SBM is meant to work — it is a sensitivity-based charge, so the engine
// takes the delta/vega sensitivities (the CRIF) as data and a deployment computes
// the Greeks via the risk engine and feeds them in. Same "the model output is an
// input" stance OPT-01 / COLL-01 / XVA-01 take.
//
// # The three correlation scenarios
//
// FRTB computes the SBM under HIGH / MEDIUM / LOW correlations and takes the max
// (a bank cannot assume the netting that happens to help it). LOW correlation
// penalizes a book whose sensitivities OFFSET — less correlation means less
// netting benefit, so more capital. This is why the SBM is a max, not a single
// number; the worked-example test exercises exactly that.
package frtb

import "math"

// Sensitivity is one CRIF-style risk sensitivity to a single risk factor, tagged
// with its FRTB risk class and bucket. The raw (unweighted) sensitivity; the risk
// weight is applied from the params.
type Sensitivity struct {
	RiskClass string
	Bucket    string
	Factor    string
	Amount    float64
}

// ClassParams are the supervisory parameters for one risk class: the per-bucket
// risk weights, the intra-bucket correlation ρ (between distinct factors in a
// bucket) and the inter-bucket correlation γ (between distinct buckets).
type ClassParams struct {
	RiskWeight map[string]float64
	IntraCorr  float64
	InterCorr  float64
}

// Params maps a risk class to its supervisory parameters.
type Params map[string]ClassParams

// Charge is the SBM capital for a set of sensitivities: for each risk class the
// worst of the three correlation scenarios, summed across risk classes. The
// per-scenario, per-class breakdown is available via ChargeByScenario.
func Charge(sensitivities []Sensitivity, params Params) float64 {
	var total float64
	for class, sens := range groupByClass(sensitivities) {
		p, ok := params[class]
		if !ok {
			continue
		}
		total += maxScenario(sens, p)
	}
	return total
}

// Scenario is one of the three FRTB correlation scenarios.
type Scenario int

const (
	Low Scenario = iota
	Medium
	High
)

// ChargeForScenario computes the SBM capital for one risk class under one
// correlation scenario — the building block Charge maxes over. Exposed for the
// worked-example reconciliation.
func ChargeForScenario(sensitivities []Sensitivity, p ClassParams, s Scenario) float64 {
	rho := scaleCorr(p.IntraCorr, s)
	gamma := scaleCorr(p.InterCorr, s)

	// Weighted sensitivities by (bucket, factor), netted per factor first.
	wsByBucket := map[string][]float64{}
	for _, ws := range weightedByFactor(sensitivities, p.RiskWeight) {
		wsByBucket[ws.bucket] = append(wsByBucket[ws.bucket], ws.value)
	}

	var sumKsq, sumS, sumSsq float64
	for _, ws := range wsByBucket {
		kb := bucketAgg(ws, rho)
		sb := 0.0
		for _, w := range ws {
			sb += w
		}
		sumKsq += kb * kb
		sumS += sb
		sumSsq += sb * sb
	}
	v := sumKsq + gamma*(sumS*sumS-sumSsq)
	if v < 0 {
		v = 0 // the FRTB fallback when the cross term turns the radicand negative
	}
	return math.Sqrt(v)
}

func maxScenario(sens []Sensitivity, p ClassParams) float64 {
	m := ChargeForScenario(sens, p, Low)
	for _, s := range []Scenario{Medium, High} {
		if c := ChargeForScenario(sens, p, s); c > m {
			m = c
		}
	}
	return m
}

// scaleCorr applies the FRTB correlation scenario to a base correlation: HIGH =
// min(1.25·ρ, 1), LOW = max(2·ρ−1, 0.75·ρ), MEDIUM = ρ.
func scaleCorr(rho float64, s Scenario) float64 {
	switch s {
	case High:
		return math.Min(1.25*rho, 1)
	case Low:
		return math.Max(2*rho-1, 0.75*rho)
	default:
		return rho
	}
}

// bucketAgg is √(max(0, Σ WS² + ρ·((ΣWS)² − Σ WS²))) — the within-bucket
// aggregation with intra-bucket correlation ρ.
func bucketAgg(ws []float64, rho float64) float64 {
	var sum, sumSq float64
	for _, w := range ws {
		sum += w
		sumSq += w * w
	}
	v := sumSq + rho*(sum*sum-sumSq)
	if v < 0 {
		v = 0
	}
	return math.Sqrt(v)
}

type weighted struct {
	bucket string
	value  float64
}

// weightedByFactor nets raw sensitivities per (bucket, factor) and applies the
// bucket risk weight.
func weightedByFactor(sens []Sensitivity, rw map[string]float64) []weighted {
	type fk struct{ bucket, factor string }
	netted := map[fk]float64{}
	order := []fk{}
	for _, s := range sens {
		k := fk{s.Bucket, s.Factor}
		if _, seen := netted[k]; !seen {
			order = append(order, k)
		}
		netted[k] += s.Amount
	}
	out := make([]weighted, 0, len(order))
	for _, k := range order {
		out = append(out, weighted{bucket: k.bucket, value: rw[k.bucket] * netted[k]})
	}
	return out
}

func groupByClass(sens []Sensitivity) map[string][]Sensitivity {
	out := map[string][]Sensitivity{}
	for _, s := range sens {
		out[s.RiskClass] = append(out[s.RiskClass], s)
	}
	return out
}
