package frtb

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// P&L attribution test (PARITY-03i, the MAR32 desk-eligibility gate). A desk
// may use the internal-models approach only while its risk-theoretical P&L
// (RTPL — what the risk model's factors explain) tracks the hypothetical P&L
// (HPL — the front-office revaluation). Two statistics decide it: the
// Spearman rank correlation between the two series and the Kolmogorov–Smirnov
// distance between their distributions, each zoned per the published
// thresholds; the desk's zone is the worse of the two. RED sends the desk to
// the standardized approach; AMBER adds a capital surcharge.

// PLAZone is a desk-eligibility traffic-light zone.
type PLAZone string

const (
	PLAGreen PLAZone = "GREEN"
	PLAAmber PLAZone = "AMBER"
	PLARed   PLAZone = "RED"
)

// MAR32.39 zone thresholds.
const (
	plaCorrGreen = 0.80 // Spearman above ⇒ green
	plaCorrRed   = 0.70 // below ⇒ red
	plaKSGreen   = 0.09 // KS below ⇒ green
	plaKSRed     = 0.12 // above ⇒ red
)

// PLAResult is one desk's attribution test.
type PLAResult struct {
	Spearman float64
	KS       float64
	Zone     PLAZone
}

// ErrPLA is returned for unusable attribution-test inputs.
var ErrPLA = errors.New("frtb: invalid P&L attribution inputs")

// PLATest runs the attribution test on aligned daily RTPL and HPL series.
func PLATest(rtpl, hpl []float64) (PLAResult, error) {
	n := len(rtpl)
	if n < 2 || len(hpl) != n {
		return PLAResult{}, fmt.Errorf("%w: need aligned series of ≥2 observations", ErrPLA)
	}
	r := PLAResult{
		Spearman: spearman(rtpl, hpl),
		KS:       ksDistance(rtpl, hpl),
	}
	corrZone := zoneAbove(r.Spearman, plaCorrGreen, plaCorrRed)
	ksZone := zoneBelow(r.KS, plaKSGreen, plaKSRed)
	r.Zone = worseZone(corrZone, ksZone)
	return r, nil
}

// zoneAbove zones a higher-is-better statistic; zoneBelow a lower-is-better.
func zoneAbove(v, green, red float64) PLAZone {
	switch {
	case v > green:
		return PLAGreen
	case v < red:
		return PLARed
	default:
		return PLAAmber
	}
}

func zoneBelow(v, green, red float64) PLAZone {
	switch {
	case v < green:
		return PLAGreen
	case v > red:
		return PLARed
	default:
		return PLAAmber
	}
}

func worseZone(a, b PLAZone) PLAZone {
	rank := map[PLAZone]int{PLAGreen: 0, PLAAmber: 1, PLARed: 2}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

// spearman is the rank correlation: Pearson correlation of the two series'
// average ranks (ties share the mean rank).
func spearman(x, y []float64) float64 {
	rx, ry := ranks(x), ranks(y)
	n := float64(len(x))
	var mx, my float64
	for i := range rx {
		mx += rx[i]
		my += ry[i]
	}
	mx /= n
	my /= n
	var sxy, sxx, syy float64
	for i := range rx {
		dx, dy := rx[i]-mx, ry[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return 0 // a constant series carries no rank information
	}
	return sxy / math.Sqrt(sxx*syy)
}

// ranks assigns 1-based average ranks with ties sharing the mean rank.
func ranks(xs []float64) []float64 {
	idx := make([]int, len(xs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return xs[idx[a]] < xs[idx[b]] })
	out := make([]float64, len(xs))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && xs[idx[j+1]] == xs[idx[i]] {
			j++
		}
		avg := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[idx[k]] = avg
		}
		i = j + 1
	}
	return out
}

// ksDistance is the two-sample Kolmogorov–Smirnov statistic: the largest gap
// between the two empirical CDFs over the pooled sample points.
func ksDistance(x, y []float64) float64 {
	xs := append([]float64(nil), x...)
	ys := append([]float64(nil), y...)
	sort.Float64s(xs)
	sort.Float64s(ys)
	cdf := func(sorted []float64, v float64) float64 {
		return float64(sort.SearchFloat64s(sorted, math.Nextafter(v, math.Inf(1)))) / float64(len(sorted))
	}
	var d float64
	for _, v := range append(append([]float64(nil), xs...), ys...) {
		if g := math.Abs(cdf(xs, v) - cdf(ys, v)); g > d {
			d = g
		}
	}
	return d
}
