// Package optimization is the portfolio construction & optimization layer
// (OPT-01): mean-variance / risk-parity construction under the SAME mandate
// constraints COMP-01 enforces, producing a human-in-the-loop RebalanceProposal
// that the OPT-01e bridge materializes into OMS-01 orders. The portfolio-
// manager's forward workflow (ROI #29).
//
// # Outside the risk module — covariance is an input, not a reach-in
//
// optimization lives at kanz/internal/optimization, OUTSIDE kanz/internal/risk,
// so the RISK-02 arch boundary forbids importing the risk impl packages
// (the MODEL-01e covariance estimator among them). The optimizer therefore takes
// the covariance + expected returns as INPUTS (MarketInputs) and ships its own
// small SampleCovariance estimator — the same "reuse the shared substrate,
// re-declare the risk-internal concept as your own" stance performance and
// COMP-01 take. A deployment feeds the MODEL-01e covariance in at the
// composition root.
//
// It DOES reuse COMP-01 directly (no boundary there): the constraint layer
// (constraints.go) projects a target book and runs the compliance engine, so a
// book you can't hold you can't optimize into — one source of truth for the
// rules.
package optimization

import (
	"errors"
	"math"
)

// ObjectiveType selects what the optimizer optimizes. Mirrors
// optimization.v1.ObjectiveType.
type ObjectiveType int

const (
	// MaxReturn maximizes expected return μᵀw (an LP over the constraints).
	MaxReturn ObjectiveType = iota
	// MinVariance minimizes portfolio variance wᵀΣw.
	MinVariance
	// MaxSharpe maximizes the Sharpe ratio (the tangency portfolio).
	MaxSharpe
	// RiskParity equalizes each asset's risk contribution.
	RiskParity
	// HRP is hierarchical risk parity: a covariance-only, long-only allocation
	// from asset clustering (no expected returns), robust to an ill-conditioned Σ
	// because it never inverts it. Unlike every other objective, HRP does NOT
	// honor per-asset box/group bounds — it returns its natural allocation, and
	// mandate compliance is validated downstream by CheckMandate. Mirrors
	// optimization.v1.OBJECTIVE_TYPE_HRP.
	HRP
)

// Objective parameterizes the optimization.
type Objective struct {
	Type ObjectiveType
	// RiskAversion λ blends return against variance for MaxReturn (maximize
	// μᵀw − λ·wᵀΣw). 0 ⇒ the pure linear objective.
	RiskAversion float64
	// RiskFreeRate is the per-period risk-free rate for MaxSharpe.
	RiskFreeRate float64
}

// MarketInputs are the optimizer's market data: the instrument universe, their
// expected returns (μ, needed by MaxReturn/MaxSharpe), and the covariance matrix
// (Σ, needed by every objective but MaxReturn). All aligned to Instruments.
type MarketInputs struct {
	Instruments     []string
	ExpectedReturns []float64
	Covariance      [][]float64
}

// Result is the optimized portfolio: target weights per instrument and the
// ex-ante return/risk of the solution.
type Result struct {
	Weights        map[string]float64
	ExpectedReturn float64 // μᵀw (0 when μ is absent)
	ExpectedRisk   float64 // √(wᵀΣw)
}

var (
	// ErrNoUniverse is returned when MarketInputs has no instruments.
	ErrNoUniverse = errors.New("optimization: empty instrument universe")
	// ErrInputsMismatch is returned when μ/Σ dimensions disagree with the universe.
	ErrInputsMismatch = errors.New("optimization: input dimensions do not match the universe")
	// ErrNeedReturns is returned when an objective needs μ but none was supplied.
	ErrNeedReturns = errors.New("optimization: objective requires expected returns")
	// ErrNeedCovariance is returned when an objective needs Σ but none was supplied.
	ErrNeedCovariance = errors.New("optimization: objective requires a covariance matrix")
)

// Optimize solves for the target weights of obj over in, restricted to cons
// (nil ⇒ the default long-only, fully-invested, unbounded-above region). The
// result weights are keyed by instrument id and sum to 1.
func Optimize(in MarketInputs, obj Objective, cons *ConstraintSet) (Result, error) {
	n := len(in.Instruments)
	if n == 0 {
		return Result{}, ErrNoUniverse
	}
	if in.ExpectedReturns != nil && len(in.ExpectedReturns) != n {
		return Result{}, ErrInputsMismatch
	}
	if in.Covariance != nil && (len(in.Covariance) != n || !square(in.Covariance, n)) {
		return Result{}, ErrInputsMismatch
	}
	needsReturns := obj.Type == MaxReturn || obj.Type == MaxSharpe
	needsCov := obj.Type != MaxReturn || obj.RiskAversion > 0
	if needsReturns && in.ExpectedReturns == nil {
		return Result{}, ErrNeedReturns
	}
	if needsCov && in.Covariance == nil {
		return Result{}, ErrNeedCovariance
	}

	lo, hi := bounds(in.Instruments, cons)

	var w []float64
	switch obj.Type {
	case MaxReturn:
		if obj.RiskAversion > 0 {
			w = minimizeQuadratic(in.Covariance, in.ExpectedReturns, obj.RiskAversion, lo, hi)
		} else {
			w = maximizeLinear(in.ExpectedReturns, lo, hi)
		}
	case MinVariance:
		w = minimizeQuadratic(in.Covariance, nil, 1, lo, hi)
	case MaxSharpe:
		w = maxSharpe(in.Covariance, in.ExpectedReturns, obj.RiskFreeRate, lo, hi)
	case RiskParity:
		w = riskParity(in.Covariance, lo, hi)
	case HRP:
		// Covariance-only; bounds deliberately not applied (see the HRP comment).
		w = hrp(in.Covariance)
	default:
		return Result{}, errors.New("optimization: unknown objective type")
	}

	res := Result{Weights: make(map[string]float64, n)}
	for i, id := range in.Instruments {
		res.Weights[id] = w[i]
	}
	if in.ExpectedReturns != nil {
		res.ExpectedReturn = dot(in.ExpectedReturns, w)
	}
	if in.Covariance != nil {
		res.ExpectedRisk = math.Sqrt(math.Max(quadForm(in.Covariance, w), 0))
	}
	return res, nil
}

// SampleCovariance estimates the Bessel-corrected sample covariance matrix from
// per-instrument return series (returns[i] is instrument i's series, all the
// same length). This is the optimizer's own estimator — the boundary-clean
// stand-in for the MODEL-01e covariance a deployment may feed in instead. Fewer
// than two observations ⇒ a zero matrix.
func SampleCovariance(returns [][]float64) [][]float64 {
	n := len(returns)
	cov := make([][]float64, n)
	for i := range cov {
		cov[i] = make([]float64, n)
	}
	if n == 0 {
		return cov
	}
	t := len(returns[0])
	if t < 2 {
		return cov
	}
	means := make([]float64, n)
	for i := range returns {
		var s float64
		for _, x := range returns[i] {
			s += x
		}
		means[i] = s / float64(t)
	}
	for i := 0; i < n; i++ {
		for j := i; j < n; j++ {
			var s float64
			for k := 0; k < t; k++ {
				s += (returns[i][k] - means[i]) * (returns[j][k] - means[j])
			}
			c := s / float64(t-1)
			cov[i][j] = c
			cov[j][i] = c
		}
	}
	return cov
}

// RiskContributions returns each instrument's fractional contribution to total
// portfolio risk: RCᵢ = wᵢ(Σw)ᵢ / (wᵀΣw). For a risk-parity solution these are
// (approximately) equal — the OPT-01f equal-risk-contribution check.
func RiskContributions(weights map[string]float64, in MarketInputs) map[string]float64 {
	n := len(in.Instruments)
	w := make([]float64, n)
	for i, id := range in.Instruments {
		w[i] = weights[id]
	}
	total := quadForm(in.Covariance, w)
	out := make(map[string]float64, n)
	if total <= 0 {
		return out
	}
	sw := matVec(in.Covariance, w)
	for i, id := range in.Instruments {
		out[id] = w[i] * sw[i] / total
	}
	return out
}

func square(m [][]float64, n int) bool {
	for i := range m {
		if len(m[i]) != n {
			return false
		}
	}
	return true
}
