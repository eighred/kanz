package optimization

import (
	"errors"
	"math"
)

// ErrNotPSD is returned when a covariance matrix is not positive-semidefinite —
// asymmetric or indefinite. A PSD-but-singular matrix (collinear assets, a zero
// eigenvalue) is accepted, because a real covariance may legitimately be singular
// and the solvers tolerate it.
var ErrNotPSD = errors.New("optimization: covariance matrix is not positive-semidefinite")

// checkPSD reports whether cov is symmetric and positive-semidefinite, via an
// LDLᵀ decomposition inspected for a negative pivot. It rejects a genuinely
// INDEFINITE matrix (a negative eigenvalue) but ACCEPTS a singular PSD one (a
// zero eigenvalue). The tolerance is scaled to the matrix magnitude so numerical
// round-trip noise is not rejected. O(n³); never panics on a square matrix.
func checkPSD(cov [][]float64) error {
	n := len(cov)
	if n == 0 {
		return nil
	}
	var maxDiag float64
	for i := 0; i < n; i++ {
		if d := math.Abs(cov[i][i]); d > maxDiag {
			maxDiag = d
		}
	}
	tol := 1e-9 * (1 + maxDiag)

	// A covariance is symmetric by construction.
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if math.Abs(cov[i][j]-cov[j][i]) > tol {
				return ErrNotPSD
			}
		}
	}

	// LDLᵀ: A = L·D·Lᵀ, L unit lower-triangular. A is PSD iff every pivot D_j ≥ 0
	// and every zero pivot has a consistent (≈0) column.
	d := make([]float64, n)
	l := make([][]float64, n)
	for i := range l {
		l[i] = make([]float64, n)
		l[i][i] = 1
	}
	for j := 0; j < n; j++ {
		dj := cov[j][j]
		for k := 0; k < j; k++ {
			dj -= l[j][k] * l[j][k] * d[k]
		}
		if dj < -tol {
			return ErrNotPSD // negative pivot ⇒ indefinite
		}
		d[j] = dj
		for i := j + 1; i < n; i++ {
			num := cov[i][j]
			for k := 0; k < j; k++ {
				num -= l[i][k] * l[j][k] * d[k]
			}
			if math.Abs(dj) <= tol {
				// Zero pivot (singular direction): PSD requires a consistent column.
				if math.Abs(num) > tol {
					return ErrNotPSD
				}
				l[i][j] = 0
			} else {
				l[i][j] = num / dj
			}
		}
	}
	return nil
}
