package optimization

import "math"

// Numerical core of the optimizer (OPT-01b): a small, dependency-free,
// deterministic solver behind the Solver seam. No gonum / no QP library — the
// objectives reduce to projected gradient over a budget-box feasible region plus
// two closed forms (the tangency portfolio and the risk-parity fixed point),
// which are ~150 lines and cover the OPT-01 objectives without a heavy dep (the
// "avoid dependency bloat" rule). Portfolios are tens of names, so O(n²)·iters
// is trivially fast.
//
// The feasible region is { lo ≤ wᵢ ≤ hi, Σwᵢ = 1 } — a fully-invested,
// box-bounded long/short book. projectBudgetBox is the Euclidean projection onto
// it (water-filling on a single budget multiplier), the workhorse every
// projected-gradient objective calls.

// matVec returns Σ·w.
func matVec(sigma [][]float64, w []float64) []float64 {
	n := len(w)
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		var s float64
		row := sigma[i]
		for j := 0; j < n; j++ {
			s += row[j] * w[j]
		}
		out[i] = s
	}
	return out
}

// quadForm returns wᵀΣw (the portfolio variance).
func quadForm(sigma [][]float64, w []float64) float64 {
	sw := matVec(sigma, w)
	var s float64
	for i := range w {
		s += w[i] * sw[i]
	}
	return s
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// projectBudgetBox returns the point of { lo ≤ wᵢ ≤ hi, Σwᵢ = 1 } nearest to v.
// The solution is wᵢ = clamp(vᵢ − τ, loᵢ, hiᵢ) for the single multiplier τ that
// makes Σwᵢ = 1; since the clamped sum is monotone decreasing in τ, τ is found by
// bisection. When the budget is infeasible for the bounds (Σhi < 1 or Σlo > 1)
// the boundary projection is returned (best effort).
func projectBudgetBox(v, lo, hi []float64) []float64 {
	n := len(v)
	sum := func(tau float64) float64 {
		var s float64
		for i := 0; i < n; i++ {
			x := v[i] - tau
			if x < lo[i] {
				x = lo[i]
			} else if x > hi[i] {
				x = hi[i]
			}
			s += x
		}
		return s
	}
	// Bracket τ: at τ=min(vᵢ−hiᵢ) the sum is Σhi (max); at τ=max(vᵢ−loᵢ) it is
	// Σlo (min).
	a, b := math.Inf(1), math.Inf(-1)
	for i := 0; i < n; i++ {
		if lb := v[i] - hi[i]; lb < a {
			a = lb
		}
		if ub := v[i] - lo[i]; ub > b {
			b = ub
		}
	}
	for i := 0; i < 100; i++ {
		m := (a + b) / 2
		if sum(m) > 1 {
			a = m
		} else {
			b = m
		}
	}
	tau := (a + b) / 2
	w := make([]float64, n)
	for i := 0; i < n; i++ {
		x := v[i] - tau
		if x < lo[i] {
			x = lo[i]
		} else if x > hi[i] {
			x = hi[i]
		}
		w[i] = x
	}
	return w
}

// stepSize is 1/L where L bounds the largest eigenvalue of 2Σ (Gershgorin: 2×
// the max absolute row sum) — the constant step that makes projected gradient on
// the convex quadratic converge.
func stepSize(sigma [][]float64) float64 {
	var maxRow float64
	for i := range sigma {
		var s float64
		for j := range sigma[i] {
			s += math.Abs(sigma[i][j])
		}
		if s > maxRow {
			maxRow = s
		}
	}
	l := 2 * maxRow
	if l <= 0 {
		return 1
	}
	return 1 / l
}

const (
	solverIters = 5000
	solverTol   = 1e-12
)

// minimizeQuadratic minimizes λ·wᵀΣw − μᵀw over the budget-box region by
// projected gradient descent (gradient 2λΣw − μ). λ=1,μ=nil is pure
// min-variance; λ>0 with μ is the mean-variance utility (a risk-averse
// max-return). Seeded at the projected centroid; converges to the global
// constrained optimum (convex objective, convex feasible set).
func minimizeQuadratic(sigma [][]float64, mu []float64, lambda float64, lo, hi []float64) []float64 {
	n := len(lo)
	w := projectBudgetBox(equalSeed(n), lo, hi)
	eta := stepSize(sigma) / math.Max(lambda, 1e-9)
	for it := 0; it < solverIters; it++ {
		grad := matVec(sigma, w)
		next := make([]float64, n)
		for i := 0; i < n; i++ {
			g := 2 * lambda * grad[i]
			if mu != nil {
				g -= mu[i]
			}
			next[i] = w[i] - eta*g
		}
		next = projectBudgetBox(next, lo, hi)
		if maxAbsDiff(next, w) < solverTol {
			return next
		}
		w = next
	}
	return w
}

// maximizeLinear maximizes μᵀw over the budget-box region — projecting a large
// multiple of μ onto the region yields the LP vertex (water-filling: highest-μ
// names to their caps until the budget is spent).
func maximizeLinear(mu []float64, lo, hi []float64) []float64 {
	v := make([]float64, len(mu))
	for i := range mu {
		v[i] = mu[i] * 1e9
	}
	return projectBudgetBox(v, lo, hi)
}

// maxSharpe returns the tangency portfolio — w ∝ Σ⁻¹(μ − rf) normalized to the
// budget — then projects onto the box and refines with a few projected-gradient-
// ascent steps on the Sharpe ratio so box constraints are respected. The
// unconstrained tangency is exact (matches the closed form); the projection +
// refinement handles binding bounds.
func maxSharpe(sigma [][]float64, mu []float64, rf float64, lo, hi []float64) []float64 {
	n := len(mu)
	excess := make([]float64, n)
	for i := range mu {
		excess[i] = mu[i] - rf
	}
	z, ok := solveLinear(sigma, excess)
	var w []float64
	if ok {
		s := 0.0
		for _, zi := range z {
			s += zi
		}
		if math.Abs(s) > 1e-15 {
			w = make([]float64, n)
			for i := range z {
				w[i] = z[i] / s
			}
		}
	}
	if w == nil {
		w = equalSeed(n)
	}
	w = projectBudgetBox(w, lo, hi)
	// Projected-gradient-ascent refinement on S(w) for the constrained case.
	for it := 0; it < 500; it++ {
		v := quadForm(sigma, w)
		sigmaSd := math.Sqrt(v)
		if sigmaSd <= 0 {
			break
		}
		e := dot(mu, w) - rf
		sw := matVec(sigma, w)
		grad := make([]float64, n)
		for i := 0; i < n; i++ {
			grad[i] = (mu[i] - (e/v)*sw[i]) / sigmaSd
		}
		next := make([]float64, n)
		for i := 0; i < n; i++ {
			next[i] = w[i] + 0.05*grad[i]
		}
		next = projectBudgetBox(next, lo, hi)
		if maxAbsDiff(next, w) < 1e-10 {
			break
		}
		w = next
	}
	return w
}

// riskParity returns the equal-risk-contribution portfolio via the damped
// multiplicative update wᵢ ← wᵢ·√(bᵢ/rcᵢ) (normalized), where rcᵢ = wᵢ(Σw)ᵢ/(wᵀΣw)
// is the normalized risk contribution and bᵢ = 1/n the equal budget. The
// square-root damping is what makes it CONVERGE — the un-damped 1/(Σw)ᵢ fixed
// point oscillates; the fixed point has every rcᵢ = 1/n (equal risk
// contributions). Long-only and fully-invested by construction; box bounds are a
// final projection.
func riskParity(sigma [][]float64, lo, hi []float64) []float64 {
	n := len(sigma)
	budget := 1.0 / float64(n)
	w := equalSeed(n)
	for it := 0; it < solverIters; it++ {
		sw := matVec(sigma, w)
		total := dot(w, sw)
		if total <= 0 {
			break
		}
		next := make([]float64, n)
		var s float64
		for i := 0; i < n; i++ {
			rc := w[i] * sw[i] / total
			factor := 1.0
			if rc > 1e-15 {
				factor = math.Sqrt(budget / rc)
			}
			next[i] = w[i] * factor
			if next[i] < 0 {
				next[i] = 0
			}
			s += next[i]
		}
		if s <= 0 {
			break
		}
		for i := 0; i < n; i++ {
			next[i] /= s
		}
		done := maxAbsDiff(next, w) < solverTol
		w = next
		if done {
			break
		}
	}
	return projectBudgetBox(w, lo, hi)
}

// solveLinear solves A·x = b by Gaussian elimination with partial pivoting.
// ok=false when A is singular. A is copied (not mutated).
func solveLinear(a [][]float64, b []float64) ([]float64, bool) {
	n := len(b)
	m := make([][]float64, n)
	for i := range a {
		m[i] = append([]float64(nil), a[i]...)
	}
	x := append([]float64(nil), b...)
	for col := 0; col < n; col++ {
		piv := col
		for r := col + 1; r < n; r++ {
			if math.Abs(m[r][col]) > math.Abs(m[piv][col]) {
				piv = r
			}
		}
		if math.Abs(m[piv][col]) < 1e-15 {
			return nil, false
		}
		m[col], m[piv] = m[piv], m[col]
		x[col], x[piv] = x[piv], x[col]
		for r := 0; r < n; r++ {
			if r == col {
				continue
			}
			f := m[r][col] / m[col][col]
			for c := col; c < n; c++ {
				m[r][c] -= f * m[col][c]
			}
			x[r] -= f * x[col]
		}
	}
	for i := 0; i < n; i++ {
		x[i] /= m[i][i]
	}
	return x, true
}

func equalSeed(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 1.0 / float64(n)
	}
	return w
}

func maxAbsDiff(a, b []float64) float64 {
	var m float64
	for i := range a {
		if d := math.Abs(a[i] - b[i]); d > m {
			m = d
		}
	}
	return m
}
