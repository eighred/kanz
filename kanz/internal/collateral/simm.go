package collateral

import (
	"errors"
	"math"
)

var ErrSIMMInput = errors.New("collateral: SIMM sensitivity is nonfinite or has no calibrated bucket")
var ErrSIMMNumerical = errors.New("collateral: SIMM arithmetic is nonfinite")

// COLL-01b — initial margin via ISDA SIMM (the sensitivity-based methodology).
// SIMM is the standard model for uncleared-derivatives IM: it takes the
// portfolio's risk sensitivities (the CRIF — delta/vega/curvature per risk
// factor), weights them by supervisory risk weights, and aggregates them within
// and across buckets with prescribed correlations. This implements the delta-
// margin core (the dominant component) generically, parameterized by the ISDA
// risk weights and correlations so the engine reproduces the published unit
// numbers; vega/curvature/base-correlation add-ons compose on the same shape.
//
// The sensitivities are an INPUT (the boundary stance — see the package doc): a
// deployment computes them from the DERIV-01 Greeks via the risk engine and
// feeds the CRIF here.

// Sensitivity is one CRIF-style risk sensitivity: the delta of the portfolio to a
// single risk factor, tagged with its risk class and bucket.
type Sensitivity struct {
	// RiskClass is the SIMM risk class ("IR", "FX", "Equity", "Commodity",
	// "Credit") — margins are summed across classes.
	RiskClass string
	// Bucket is the SIMM bucket within the class (e.g. an equity sector, a credit
	// rating). Sensitivities aggregate within a bucket, buckets across the class.
	Bucket string
	// RiskFactor is the specific factor (an underlying, a tenor) — sensitivities
	// to the same factor are summed before weighting.
	RiskFactor string
	// Amount is the sensitivity value (dollar delta per unit factor move).
	Amount float64
}

// SIMMParams are the supervisory parameters for one risk class — the ISDA risk
// weights and correlations. RiskWeight is keyed by bucket; IntraBucketCorr (ρ) is
// the correlation between distinct risk factors within a bucket; InterBucketCorr
// (γ) is the correlation between distinct buckets.
type SIMMParams struct {
	RiskWeight      map[string]float64
	IntraBucketCorr float64
	InterBucketCorr float64
}

// SIMM computes the initial margin for one risk class from its sensitivities and
// supervisory parameters, following the ISDA SIMM delta-margin aggregation:
//
//	WSₖ = RW_bucket · sₖ                               (weighted sensitivity)
//	K_b = √( Σ WSₖ² + Σ_{k≠l} ρ·WSₖ·WSₗ )              (bucket margin)
//	S_b = clamp( Σ WSₖ, −K_b, K_b )                    (bucket sum, capped)
//	IM  = √( Σ K_b² + Σ_{b≠c} γ·S_b·S_c )              (across buckets)
//
// Invalid inputs or nonfinite arithmetic return no measurement. In particular,
// an absent bucket cannot become a zero weight that erases its margin. These
// checks do not qualify the representative calibration as an approved IM model.
func SIMM(sensitivities []Sensitivity, p SIMMParams) (float64, error) {
	if err := validateSIMMParams(p); err != nil {
		return 0, err
	}
	// Aggregate raw sensitivities by (bucket, risk factor) first.
	type fk struct{ bucket, factor string }
	byFactor := make(map[fk]float64)
	bucketOrder := []string{}
	seenBucket := map[string]bool{}
	for _, s := range sensitivities {
		if _, ok := p.RiskWeight[s.Bucket]; !ok || !finiteSIMM(s.Amount) {
			return 0, ErrSIMMInput
		}
		key := fk{s.Bucket, s.RiskFactor}
		byFactor[key] += s.Amount
		if !finiteSIMM(byFactor[key]) {
			return 0, ErrSIMMNumerical
		}
		if !seenBucket[s.Bucket] {
			seenBucket[s.Bucket] = true
			bucketOrder = append(bucketOrder, s.Bucket)
		}
	}

	// Weighted sensitivities grouped by bucket.
	wsByBucket := make(map[string][]float64)
	for k, amt := range byFactor {
		rw := p.RiskWeight[k.bucket]
		weighted := rw * amt
		if !finiteSIMM(weighted) {
			return 0, ErrSIMMNumerical
		}
		wsByBucket[k.bucket] = append(wsByBucket[k.bucket], weighted)
	}

	kb := make(map[string]float64)
	sb := make(map[string]float64)
	for bucket, ws := range wsByBucket {
		margin, err := bucketMargin(ws, p.IntraBucketCorr)
		if err != nil {
			return 0, err
		}
		kb[bucket] = margin
		sum := 0.0
		for _, w := range ws {
			sum += w
		}
		sb[bucket] = clamp(sum, -kb[bucket], kb[bucket])
	}

	// Across buckets: Σ K_b² + γ·Σ_{b≠c} S_b·S_c.
	var sumKsq, sumS, sumSsq float64
	for _, bucket := range bucketOrder {
		sumKsq += kb[bucket] * kb[bucket]
		sumS += sb[bucket]
		sumSsq += sb[bucket] * sb[bucket]
		if !finiteSIMM(sumKsq) || !finiteSIMM(sumS) || !finiteSIMM(sumSsq) {
			return 0, ErrSIMMNumerical
		}
	}
	cross := p.InterBucketCorr * (sumS*sumS - sumSsq)
	total := sumKsq + cross
	if !finiteSIMM(cross) || !finiteSIMM(total) {
		return 0, ErrSIMMNumerical
	}
	if total < 0 {
		total = 0 // correlation round-off guard
	}
	return math.Sqrt(total), nil
}

// bucketMargin is √( Σ WSₖ² + ρ·((ΣWS)² − Σ WSₖ²) ) — the within-bucket
// aggregation with a single intra-bucket correlation ρ.
func bucketMargin(ws []float64, rho float64) (float64, error) {
	var sum, sumSq float64
	for _, w := range ws {
		sum += w
		sumSq += w * w
		if !finiteSIMM(sum) || !finiteSIMM(sumSq) {
			return 0, ErrSIMMNumerical
		}
	}
	v := sumSq + rho*(sum*sum-sumSq)
	if !finiteSIMM(v) {
		return 0, ErrSIMMNumerical
	}
	if v < 0 {
		v = 0
	}
	return math.Sqrt(v), nil
}

func finiteSIMM(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}
