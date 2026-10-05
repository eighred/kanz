package factormodel

import (
	"fmt"
	"math"
)

// DescriptorPolicy fixes complete-case eligibility, equal weights, sample SD,
// no winsorization/imputation, and a sorted country reference when both axes exist.
const DescriptorPolicy = "complete_equal_sample_sd_country_reference_v1"

func finiteDescriptor(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func copyStyles(values map[string]float64, names []string) map[string]float64 {
	out := make(map[string]float64, len(names))
	for _, name := range names {
		out[name] = values[name]
	}
	return out
}

// Scale before centering to avoid overflow for finite extreme descriptors.
func descriptorScores(values []float64) ([]float64, error) {
	if len(values) < 2 {
		return nil, fmt.Errorf("factormodel: insufficient descriptor observations")
	}
	scale := 0.0
	for _, v := range values {
		if !finiteDescriptor(v) {
			return nil, fmt.Errorf("factormodel: nonfinite descriptor")
		}
		scale = math.Max(scale, math.Abs(v))
	}
	if scale == 0 {
		return nil, fmt.Errorf("factormodel: constant descriptor")
	}
	scaled := make([]float64, len(values))
	for i, v := range values {
		scaled[i] = v / scale
	}
	sd := math.Sqrt(sampleVar(scaled))
	if sd == 0 || !finiteDescriptor(sd) {
		return nil, fmt.Errorf("factormodel: constant/unresolvable descriptor")
	}
	mu := mean(scaled)
	for i := range scaled {
		scaled[i] = (scaled[i] - mu) / sd
	}
	return scaled, nil
}

// Twice-reorthogonalized modified Gram-Schmidt. Rank is assessed BEFORE adding
// ridge rows: regularization is not evidence that economic factors are identified.
func designQR(a [][]float64) ([][]float64, [][]float64, error) {
	n, k := len(a), len(a[0])
	q := make([][]float64, k)
	r := make([][]float64, k)
	scale := 0.0
	for _, row := range a {
		for _, v := range row {
			scale = math.Hypot(scale, v)
		}
	}
	for j := 0; j < k; j++ {
		r[j] = make([]float64, k)
		v := make([]float64, n)
		for i := range a {
			v[i] = a[i][j]
		}
		for pass := 0; pass < 2; pass++ {
			for h := 0; h < j; h++ {
				dot := 0.0
				for i := range v {
					dot += q[h][i] * v[i]
				}
				r[h][j] += dot
				for i := range v {
					v[i] -= dot * q[h][i]
				}
			}
		}
		norm := 0.0
		for _, x := range v {
			norm = math.Hypot(norm, x)
		}
		if !finiteDescriptor(norm) || norm <= 1e-10*scale {
			return nil, nil, fmt.Errorf("factormodel: deficient design rank at column %d", j)
		}
		r[j][j] = norm
		for i := range v {
			v[i] /= norm
		}
		q[j] = v
	}
	return q, r, nil
}
