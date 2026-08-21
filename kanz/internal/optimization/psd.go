package optimization

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrNotPSD is returned when a covariance matrix is not positive-semidefinite —
// asymmetric or indefinite. A PSD-but-singular matrix (collinear assets, a zero
// eigenvalue) is still ACCEPTED, because a real covariance may legitimately be
// singular and HRP/min-variance tolerate it — but it no longer passes
// anonymously: psdRank reports the rank, and Optimize turns a deficient one into
// a CovarianceQuality the caller can see (#621).
var ErrNotPSD = errors.New("optimization: covariance matrix is not positive-semidefinite")

// CovarianceQuality is WHAT IS KNOWN ABOUT THE COVARIANCE AN ANSWER WAS COMPUTED
// FROM, in five states because a matrix that was accepted is not a matrix that
// was trusted (#621).
//
// # What went wrong without it
//
// psdRank — named checkPSD before this issue, and returning nothing but an
// error — accepted a singular Σ deliberately and said so in a comment. The
// premise was true and the conclusion did not follow: it could not tell
// LEGITIMATELY singular (two collinear assets) from singular BECAUSE THE ESTIMATE
// HAD TOO FEW OBSERVATIONS, and MarketInputs had no field able to carry the
// observation count that separates them. A sample covariance over T periods has
// rank at most min(T−1, n), so an estimate over fewer periods than assets is
// singular by construction — and a singular Σ prices some portfolio at exactly
// zero variance. That reached an API response as an ExpectedRisk of 0, which is
// also what a genuinely riskless book reports. SampleCovariance made it worse: on
// fewer than two observations it returned a matrix of ZEROS, so "we had no data"
// and "nothing in this book moves" were the same bytes.
//
// # Why a status and not a refusal
//
// A refusal would break the two objectives that are CORRECT on a deficient Σ:
// HRP never inverts it (the documented reason HRP exists here) and min-variance
// genuinely wants the null direction. What must not survive is the RISK NUMBER,
// so Optimize leaves Result.ExpectedRisk nil for every state but the two that
// support one. The objectives whose ANSWER is undefined — MaxSharpe and
// RiskParity — refuse outright instead; see ErrTangencyUndefined.
//
// # It is a disclosure, not a gate
//
// Nothing refuses to materialize orders on this field, and that is deliberate: a
// PM may legitimately trade a book whose risk model is degenerate, which is what
// HRP is for. MandateStatus remains the only gate on live capital. What this
// field guarantees is that the degeneracy is IN THE RESPONSE rather than folded
// into a zero.
type CovarianceQuality int

const (
	// CovarianceUnchecked is the ZERO VALUE: no covariance was supplied, or
	// nothing has evaluated one. It is not a soft pass — it carries no risk
	// number, and a proposal cannot acquire a quality merely by being constructed.
	CovarianceUnchecked CovarianceQuality = iota
	// CovarianceRankDeficient means Σ was accepted (symmetric, PSD) but has a zero
	// eigenvalue: there exists a portfolio this model prices at exactly zero risk.
	// That may be true (collinear assets) or an artefact of estimation; no
	// observation count was stated, so this package cannot say which.
	CovarianceRankDeficient
	// CovarianceUnderObserved means the STATED observation count does not exceed
	// the universe size (T ≤ n) — the check internal/alternatives.CalibrateProxy
	// already makes before it will run a regression. It takes PRECEDENCE over
	// CovarianceRankDeficient when both hold, because it names the cause rather
	// than the symptom, and that distinction is what #621 exists to restore.
	CovarianceUnderObserved
	// CovarianceFullRank means Σ has full numerical rank AND NO OBSERVATION COUNT
	// WAS STATED. It says exactly what was verified — the rank — and deliberately
	// does not claim the estimate is adequately observed, because nothing told
	// this package how many periods are behind it. Set MarketInputs.Observations
	// to earn CovarianceObserved instead.
	CovarianceFullRank
	// CovarianceObserved means Σ has full numerical rank and the stated
	// observation count exceeds the universe size. It is the only state that
	// asserts both halves.
	CovarianceObserved
)

// String renders the quality. An out-of-range value renders as itself rather
// than falling back to one of the five: a corrupt quality that printed as
// "UNCHECKED" would be indistinguishable from an honest one.
func (q CovarianceQuality) String() string {
	switch q {
	case CovarianceUnchecked:
		return "UNCHECKED"
	case CovarianceRankDeficient:
		return "RANK_DEFICIENT"
	case CovarianceUnderObserved:
		return "UNDER_OBSERVED"
	case CovarianceFullRank:
		return "FULL_RANK"
	case CovarianceObserved:
		return "OBSERVED"
	}
	return "CovarianceQuality(" + strconv.Itoa(int(q)) + ")"
}

// SupportsRisk reports whether an ex-ante risk number computed from this Σ is
// worth reporting. It is the one predicate callers should branch on; the other
// three states are the diagnosis of why there is no number.
func (q CovarianceQuality) SupportsRisk() bool {
	return q == CovarianceFullRank || q == CovarianceObserved
}

// MarshalJSON renders the quality as its NAME, for the reason MandateStatus does:
// a bare int puts the unchecked state on the wire as 0, which reads as absent,
// false and "fine" in every JSON client there is.
func (q CovarianceQuality) MarshalJSON() ([]byte, error) {
	switch q {
	case CovarianceUnchecked, CovarianceRankDeficient, CovarianceUnderObserved,
		CovarianceFullRank, CovarianceObserved:
		return json.Marshal(q.String())
	}
	return nil, fmt.Errorf("optimization: refusing to serialize an unknown CovarianceQuality %d", int(q))
}

// UnmarshalJSON accepts ONLY the five names, case-insensitively, and an absent or
// empty value as UNCHECKED. A body carrying a quality is decoded in order to be
// seen, never quietly reinterpreted from a number.
func (q *CovarianceQuality) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return fmt.Errorf("optimization: a covariance quality is one of UNCHECKED, RANK_DEFICIENT, "+
			"UNDER_OBSERVED, FULL_RANK or OBSERVED, not %s", string(b))
	}
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "", "UNCHECKED":
		*q = CovarianceUnchecked
	case "RANK_DEFICIENT":
		*q = CovarianceRankDeficient
	case "UNDER_OBSERVED":
		*q = CovarianceUnderObserved
	case "FULL_RANK":
		*q = CovarianceFullRank
	case "OBSERVED":
		*q = CovarianceObserved
	default:
		return fmt.Errorf("optimization: unknown covariance quality %q — it is one of UNCHECKED, "+
			"RANK_DEFICIENT, UNDER_OBSERVED, FULL_RANK or OBSERVED", name)
	}
	return nil
}

// classifyCovariance turns a rank, a universe size and a STATED observation count
// (0 ⇒ not stated) into the quality reported to the caller. UNDER_OBSERVED wins
// over RANK_DEFICIENT when both hold — see the constant's comment.
func classifyCovariance(rank, n, observations int) CovarianceQuality {
	if observations > 0 && observations <= n {
		return CovarianceUnderObserved
	}
	if rank < n {
		return CovarianceRankDeficient
	}
	if observations > n {
		return CovarianceObserved
	}
	return CovarianceFullRank
}

// psdRank reports the NUMERICAL RANK of cov and whether it is symmetric and
// positive-semidefinite, via an LDLᵀ decomposition. It rejects a genuinely
// INDEFINITE matrix (a negative eigenvalue) but ACCEPTS a singular PSD one (a
// zero eigenvalue) — and returns the count of non-zero pivots, which for a PSD
// matrix with consistent zero-pivot columns is its rank. The tolerance is scaled
// to the matrix magnitude so numerical round-trip noise is not rejected. O(n³);
// never panics on a square matrix.
//
// THE RANK IS THE RETURN VALUE THAT MATTERS. This decomposition always knew it —
// the zero pivots ARE the singular directions — and threw it away, which is why a
// Σ that prices some portfolio at zero variance was accepted with nothing said
// (#621).
//
// The tolerance is a floor on what this can see: an asset whose variance is below
// 1e-9·(1+maxDiag) is reported as a singular direction. Annualized variances of
// tradable instruments are orders of magnitude above that, and a variance
// genuinely that small is one a risk number should not be built on either.
func psdRank(cov [][]float64) (int, error) {
	n := len(cov)
	if n == 0 {
		return 0, nil
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
				return 0, ErrNotPSD
			}
		}
	}

	// LDLᵀ: A = L·D·Lᵀ, L unit lower-triangular. A is PSD iff every pivot D_j ≥ 0
	// and every zero pivot has a consistent (≈0) column.
	rank := 0
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
			return 0, ErrNotPSD // negative pivot ⇒ indefinite
		}
		d[j] = dj
		if math.Abs(dj) > tol {
			rank++
		}
		for i := j + 1; i < n; i++ {
			num := cov[i][j]
			for k := 0; k < j; k++ {
				num -= l[i][k] * l[j][k] * d[k]
			}
			if math.Abs(dj) <= tol {
				// Zero pivot (singular direction): PSD requires a consistent column.
				if math.Abs(num) > tol {
					return 0, ErrNotPSD
				}
				l[i][j] = 0
			} else {
				l[i][j] = num / dj
			}
		}
	}
	return rank, nil
}
