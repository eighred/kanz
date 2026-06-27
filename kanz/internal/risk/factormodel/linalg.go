package factormodel

import "math"

// Dependency-free linear algebra for the factor model — the same "own the small
// numerical core, no gonum" stance the VaR/curve/optimization math takes. Only
// what the fundamental (cross-sectional OLS) and statistical (PCA) estimators
// need: a symmetric solver, a symmetric eigendecomposition, sample
// mean/covariance, cross-sectional z-scoring, and an inverse-normal CDF for
// parametric factor VaR.

// solveLinear solves A·x = b for a square A by Gaussian elimination with partial
// pivoting. ok=false when A is singular (a zero pivot) — the caller adds a ridge
// term and retries rather than dividing by zero.
func solveLinear(a [][]float64, b []float64) ([]float64, bool) {
	n := len(a)
	// Work on copies so the caller's matrix/vector are untouched.
	m := make([][]float64, n)
	for i := range m {
		m[i] = append([]float64(nil), a[i]...)
	}
	x := append([]float64(nil), b...)
	for col := 0; col < n; col++ {
		// Partial pivot: swap in the row with the largest |pivot|.
		piv := col
		max := math.Abs(m[col][col])
		for r := col + 1; r < n; r++ {
			if v := math.Abs(m[r][col]); v > max {
				max, piv = v, r
			}
		}
		if max == 0 {
			return nil, false
		}
		m[col], m[piv] = m[piv], m[col]
		x[col], x[piv] = x[piv], x[col]
		// Eliminate below.
		for r := col + 1; r < n; r++ {
			f := m[r][col] / m[col][col]
			if f == 0 {
				continue
			}
			for c := col; c < n; c++ {
				m[r][c] -= f * m[col][c]
			}
			x[r] -= f * x[col]
		}
	}
	// Back-substitute.
	out := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		s := x[i]
		for c := i + 1; c < n; c++ {
			s -= m[i][c] * out[c]
		}
		out[i] = s / m[i][i]
	}
	return out, true
}

// eigenSym returns the eigenvalues and eigenvectors of a symmetric matrix via the
// cyclic Jacobi rotation method (robust and dependency-free for the small
// covariance matrices the statistical model factors). vectors[i] is the unit
// eigenvector for values[i]; the pairs are unsorted (the caller sorts by
// descending value to pick principal components).
func eigenSym(sym [][]float64) (values []float64, vectors [][]float64) {
	n := len(sym)
	a := make([][]float64, n)
	for i := range a {
		a[i] = append([]float64(nil), sym[i]...)
	}
	// v accumulates the rotations — its columns are the eigenvectors.
	v := identity(n)
	const maxSweeps = 100
	for sweep := 0; sweep < maxSweeps; sweep++ {
		off := offDiagonal(a)
		if off < 1e-18 {
			break
		}
		for p := 0; p < n-1; p++ {
			for q := p + 1; q < n; q++ {
				if math.Abs(a[p][q]) < 1e-300 {
					continue
				}
				// Jacobi rotation angle zeroing a[p][q].
				theta := (a[q][q] - a[p][p]) / (2 * a[p][q])
				t := math.Copysign(1, theta) / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				if theta == 0 {
					t = 1
				}
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				rotate(a, v, p, q, c, s)
			}
		}
	}
	values = make([]float64, n)
	vectors = make([][]float64, n)
	for i := 0; i < n; i++ {
		values[i] = a[i][i]
		col := make([]float64, n)
		for r := 0; r < n; r++ {
			col[r] = v[r][i]
		}
		vectors[i] = col
	}
	return values, vectors
}

func identity(n int) [][]float64 {
	v := make([][]float64, n)
	for i := range v {
		v[i] = make([]float64, n)
		v[i][i] = 1
	}
	return v
}

func offDiagonal(a [][]float64) float64 {
	var s float64
	for i := range a {
		for j := i + 1; j < len(a); j++ {
			s += a[i][j] * a[i][j]
		}
	}
	return s
}

// rotate applies the symmetric Jacobi rotation (c,s) in the (p,q) plane to a and
// accumulates it into the eigenvector matrix v.
func rotate(a, v [][]float64, p, q int, c, s float64) {
	n := len(a)
	for i := 0; i < n; i++ {
		aip, aiq := a[i][p], a[i][q]
		a[i][p] = c*aip - s*aiq
		a[i][q] = s*aip + c*aiq
	}
	for i := 0; i < n; i++ {
		api, aqi := a[p][i], a[q][i]
		a[p][i] = c*api - s*aqi
		a[q][i] = s*api + c*aqi
	}
	for i := 0; i < n; i++ {
		vip, viq := v[i][p], v[i][q]
		v[i][p] = c*vip - s*viq
		v[i][q] = s*vip + c*viq
	}
}

// mean returns the arithmetic mean of xs (0 for empty).
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

// sampleVar returns the Bessel-corrected (1/(n-1)) sample variance of xs.
func sampleVar(xs []float64) float64 {
	n := len(xs)
	if n < 2 {
		return 0
	}
	mu := mean(xs)
	var ss float64
	for _, x := range xs {
		d := x - mu
		ss += d * d
	}
	return ss / float64(n-1)
}

// sampleCov returns the K×K Bessel-corrected covariance of K series (rows[k] is
// the k-th series, all the same length L).
func sampleCov(rows [][]float64) [][]float64 {
	k := len(rows)
	cov := make([][]float64, k)
	for i := range cov {
		cov[i] = make([]float64, k)
	}
	if k == 0 || len(rows[0]) < 2 {
		return cov
	}
	l := len(rows[0])
	means := make([]float64, k)
	for i := range rows {
		means[i] = mean(rows[i])
	}
	denom := float64(l - 1)
	for i := 0; i < k; i++ {
		for j := i; j < k; j++ {
			var s float64
			for t := 0; t < l; t++ {
				s += (rows[i][t] - means[i]) * (rows[j][t] - means[j])
			}
			c := s / denom
			cov[i][j] = c
			cov[j][i] = c
		}
	}
	return cov
}

// zScore standardizes xs cross-sectionally to mean 0 / unit standard deviation —
// the BARRA convention that puts every style factor on one comparable scale. A
// zero-variance input (every value identical) returns all zeros (no exposure).
func zScore(xs []float64) []float64 {
	mu := mean(xs)
	sd := math.Sqrt(sampleVar(xs))
	out := make([]float64, len(xs))
	if sd == 0 {
		return out
	}
	for i, x := range xs {
		out[i] = (x - mu) / sd
	}
	return out
}

// quadForm returns xᵀ M x for a square M.
func quadForm(m [][]float64, x []float64) float64 {
	var s float64
	for i := range x {
		for j := range x {
			s += x[i] * m[i][j] * x[j]
		}
	}
	return s
}

// matVec returns M·x.
func matVec(m [][]float64, x []float64) []float64 {
	out := make([]float64, len(m))
	for i := range m {
		var s float64
		for j := range x {
			s += m[i][j] * x[j]
		}
		out[i] = s
	}
	return out
}

// normalQuantile is the inverse standard-normal CDF (Acklam's rational
// approximation, |error| < 1.2e-9) — z_α for parametric factor VaR. Clamped to
// the open interval; p≤0 ⇒ −∞-ish, p≥1 ⇒ +∞-ish are returned as large finite z.
func normalQuantile(p float64) float64 {
	if p <= 0 {
		return -8
	}
	if p >= 1 {
		return 8
	}
	a := []float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := []float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := []float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := []float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	const plow = 0.02425
	const phigh = 1 - plow
	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p <= phigh:
		q := p - 0.5
		r := q * q
		return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	default:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
}
