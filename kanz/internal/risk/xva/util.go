package xva

import "math"

// Small dependency-free numerics for the exposure simulation — the same "own the
// core" stance as the VaR/factor math.

// choleskyOrIdentity returns the lower-triangular Cholesky factor of corr, or the
// identity when corr is nil/empty/wrong-sized (independent factors). A non-
// positive pivot (a degenerate correlation) takes a tiny jitter so the factor
// stays finite.
func choleskyOrIdentity(corr [][]float64, n int) [][]float64 {
	l := make([][]float64, n)
	for i := range l {
		l[i] = make([]float64, n)
	}
	if len(corr) != n {
		for i := 0; i < n; i++ {
			l[i][i] = 1
		}
		return l
	}
	const jitter = 1e-12
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			sum := corr[i][j]
			for k := 0; k < j; k++ {
				sum -= l[i][k] * l[j][k]
			}
			if i == j {
				if sum <= 0 {
					sum = jitter
				}
				l[i][i] = math.Sqrt(sum)
			} else {
				l[i][j] = sum / l[j][j]
			}
		}
	}
	return l
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// quantile is the empirical q-quantile of an ascending-sorted sample (R-7 linear
// interpolation, matching the VaR models).
func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	switch {
	case n == 0:
		return 0
	case n == 1:
		return sorted[0]
	case q <= 0:
		return sorted[0]
	case q >= 1:
		return sorted[n-1]
	}
	h := float64(n-1) * q
	lo := int(math.Floor(h))
	if lo+1 >= n {
		return sorted[n-1]
	}
	return sorted[lo] + (h-float64(lo))*(sorted[lo+1]-sorted[lo])
}

// timeAverage integrates a per-grid-date series over the grid by the trapezoidal
// rule and divides by the horizon — the time-weighted average EE the CVA/EPE
// definitions use (so uneven grid spacing is handled correctly).
func timeAverage(times, values []float64) float64 {
	n := len(times)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return values[0]
	}
	var area, span float64
	prevT := 0.0
	for k := 0; k < n; k++ {
		dt := times[k] - prevT
		var prevV float64
		if k > 0 {
			prevV = values[k-1]
		} else {
			prevV = values[0]
		}
		area += 0.5 * (prevV + values[k]) * dt
		span += dt
		prevT = times[k]
	}
	if span == 0 {
		return 0
	}
	return area / span
}
