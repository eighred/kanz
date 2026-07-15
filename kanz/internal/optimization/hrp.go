package optimization

import "math"

// hrp computes Hierarchical Risk Parity weights (López de Prado, 2016) from the
// covariance matrix alone — no expected returns, and Σ is NEVER inverted (the
// numerical robustness that motivates HRP over mean-variance). Four stages:
// correlation-distance, single-linkage agglomerative clustering, quasi-diagonal
// ordering, and recursive bisection by cluster variance. Returns long-only
// weights (all ≥ 0) that sum to 1, aligned to cov's row order.
//
// Box/group bounds are deliberately NOT honored here (see the HRP ObjectiveType
// comment): HRP is long-only and fully-invested by construction, and projecting
// onto a bound box would distort the risk allocation into something that is no
// longer HRP. Mandate compliance is validated downstream by CheckMandate.
func hrp(cov [][]float64) []float64 {
	n := len(cov)
	switch n {
	case 0:
		return nil
	case 1:
		return []float64{1}
	}
	order := quasiDiagOrder(correlDistance(cov))
	return recursiveBisection(cov, order)
}

// correlDistance maps Σ to the HRP distance matrix d_ij = √(½(1−ρ_ij)), where
// ρ_ij = Σ_ij/√(Σ_ii·Σ_jj). A non-positive Σ_ii ⇒ ρ treated as 0 (distance
// √0.5), never a division by zero. ρ is clamped to [-1,1] against rounding.
func correlDistance(cov [][]float64) [][]float64 {
	n := len(cov)
	d := make([][]float64, n)
	for i := range d {
		d[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			var rho float64
			denom := math.Sqrt(cov[i][i] * cov[j][j])
			if denom > 0 {
				rho = cov[i][j] / denom
			}
			if rho > 1 {
				rho = 1
			} else if rho < -1 {
				rho = -1
			}
			dij := math.Sqrt(0.5 * (1 - rho))
			d[i][j] = dij
			d[j][i] = dij
		}
	}
	return d
}

// quasiDiagOrder performs single-linkage agglomerative clustering on the distance
// matrix and returns the leaf order in which correlated assets are adjacent (the
// quasi-diagonalization). Each merge concatenates the two closest clusters'
// member lists; the final cluster's member list is the order. O(n³) — fine for
// the instrument counts a portfolio optimizer sees.
func quasiDiagOrder(dist [][]float64) []int {
	n := len(dist)
	clusters := make([][]int, n)
	for i := range clusters {
		clusters[i] = []int{i}
	}
	for len(clusters) > 1 {
		bi, bj := 0, 1
		best := math.Inf(1)
		for a := 0; a < len(clusters); a++ {
			for b := a + 1; b < len(clusters); b++ {
				if d := singleLinkage(clusters[a], clusters[b], dist); d < best {
					best = d
					bi, bj = a, b
				}
			}
		}
		merged := make([]int, 0, len(clusters[bi])+len(clusters[bj]))
		merged = append(merged, clusters[bi]...)
		merged = append(merged, clusters[bj]...)
		// Remove bj (higher index) first, then bi, then append the merge.
		clusters = append(clusters[:bj], clusters[bj+1:]...)
		clusters = append(clusters[:bi], clusters[bi+1:]...)
		clusters = append(clusters, merged)
	}
	return clusters[0]
}

// singleLinkage is the minimum pairwise distance between any member of a and any
// member of b — the nearest-point cluster distance.
func singleLinkage(a, b []int, dist [][]float64) float64 {
	m := math.Inf(1)
	for _, i := range a {
		for _, j := range b {
			if dist[i][j] < m {
				m = dist[i][j]
			}
		}
	}
	return m
}

// recursiveBisection allocates weights over the quasi-diagonal order. Every asset
// starts at weight 1; each positional split of a sub-list multiplies its left
// half by α and its right half by 1−α, where α = 1 − V_left/(V_left+V_right) and
// V is the cluster variance (lower-variance side gets more). A leaf's final weight
// is the product of the factors along its path; the products sum to 1 once every
// sub-list is bisected to singletons. Processed with an explicit stack.
func recursiveBisection(cov [][]float64, order []int) []float64 {
	w := make([]float64, len(cov))
	for _, i := range order {
		w[i] = 1
	}
	stack := [][]int{order}
	for len(stack) > 0 {
		items := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if len(items) <= 1 {
			continue
		}
		mid := len(items) / 2
		left := items[:mid]
		right := items[mid:]
		vLeft := clusterVar(cov, left)
		vRight := clusterVar(cov, right)
		alpha := 0.5
		if vLeft+vRight > 0 {
			alpha = 1 - vLeft/(vLeft+vRight)
		}
		for _, i := range left {
			w[i] *= alpha
		}
		for _, i := range right {
			w[i] *= 1 - alpha
		}
		stack = append(stack, left, right)
	}
	return w
}

// clusterVar is the variance of the inverse-variance portfolio over the cluster:
// V = ivpᵀ · Σ_sub · ivp, using the FULL sub-covariance (off-diagonals included),
// not just the diagonal — so intra-cluster correlation shapes the split.
func clusterVar(cov [][]float64, items []int) float64 {
	ivp := inverseVariancePortfolio(cov, items)
	var v float64
	for a, i := range items {
		var row float64
		for b, j := range items {
			row += cov[i][j] * ivp[b]
		}
		v += ivp[a] * row
	}
	return v
}

// inverseVariancePortfolio returns the normalized inverse-variance weights over a
// cluster: ivp_i ∝ 1/Σ_ii. A non-positive Σ_ii contributes 0 (guarded against
// 1/0). If every variance in the cluster is non-positive, it falls back to equal
// weights (a degenerate cluster carries no variance information to tilt on).
func inverseVariancePortfolio(cov [][]float64, items []int) []float64 {
	ivp := make([]float64, len(items))
	var sum float64
	for k, i := range items {
		if cov[i][i] > 0 {
			ivp[k] = 1 / cov[i][i]
		}
		sum += ivp[k]
	}
	if sum > 0 {
		for k := range ivp {
			ivp[k] /= sum
		}
	} else {
		eq := 1 / float64(len(items))
		for k := range ivp {
			ivp[k] = eq
		}
	}
	return ivp
}
