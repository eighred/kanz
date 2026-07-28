// Package volsurface builds an implied-volatility surface (strike × expiry) from
// market option quotes (DERIV-01c). Each quote's implied vol is backed out of
// the Black-Scholes price by a robust bisection solve; the resulting points form
// a grid that interpolates bilinearly, clamping at the edges (a flat
// extrapolation, the conservative default — the surface never invents curvature
// it has no quotes for).
//
// # Point-in-time
//
// A QuoteProvider supplies the option quotes + underlying spot in effect as of a
// knowledge horizon — the reference mirror of compute.ReturnsProvider, so a
// historical recompute reads the surface that was observable then (the MODEL-01b
// bitemporal store backs the production provider; tests supply quotes directly).
package volsurface

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing"
)

// OptionQuote is one market option price to imply a vol from. Expiry is in years
// to expiry; Price is the observed premium.
type OptionQuote struct {
	Type   pricing.OptionType
	Strike float64
	Expiry float64
	Price  float64
}

// Point is one (strike, expiry) → implied-vol node on the surface.
type Point struct {
	Strike float64
	Expiry float64
	Vol    float64
}

// QuoteProvider supplies the option quotes and underlying spot for an underlying
// as of a point in time — the seam the MODEL-01b market-data store plugs into.
type QuoteProvider interface {
	Quotes(ctx context.Context, underlyingID string, asOf time.Time) (quotes []OptionQuote, spot float64, err error)
}

// ImpliedVol backs the Black-Scholes implied volatility out of a European option
// price by bisection. It returns ok=false when the price is outside the no-
// arbitrage band [intrinsic, spot] (no finite vol reproduces it). Bisection (not
// Newton) is used for unconditional robustness — vega collapses deep ITM/OTM
// where Newton diverges.
func ImpliedVol(otype pricing.OptionType, price, S, K, t, r, q float64) (float64, bool) {
	if price <= 0 || S <= 0 || K <= 0 || t <= 0 {
		return 0, false
	}
	const (
		lo   = 1e-6
		hi   = 5.0 // 500% vol upper bracket
		iter = 100
		tol  = 1e-8
	)
	f := func(sigma float64) float64 {
		return pricing.BlackScholesPrice(otype, S, K, t, r, q, sigma) - price
	}
	flo, fhi := f(lo), f(hi)
	if flo > 0 || fhi < 0 {
		return 0, false // price not bracketed ⇒ outside the arbitrage band
	}
	a, b := lo, hi
	for i := 0; i < iter; i++ {
		mid := 0.5 * (a + b)
		fm := f(mid)
		if fm > -tol && fm < tol {
			return mid, true
		}
		if fm < 0 {
			a = mid
		} else {
			b = mid
		}
	}
	return 0.5 * (a + b), true
}

// Surface is an implied-vol grid over sorted strikes and expiries. Vol queries
// interpolate bilinearly and clamp at the edges.
type Surface struct {
	strikes  []float64
	expiries []float64
	// vol[i][j] is the vol at expiries[i], strikes[j].
	vol [][]float64
}

// Build assembles a Surface from grid points. The points must form a complete
// rectangular grid (every expiry × every strike present exactly once); a missing
// or duplicated cell is an error, so the surface is never silently full of holes.
func Build(points []Point) (*Surface, error) {
	if len(points) == 0 {
		return nil, errors.New("volsurface: no points")
	}
	strikeSet := map[float64]bool{}
	expirySet := map[float64]bool{}
	for _, p := range points {
		strikeSet[p.Strike] = true
		expirySet[p.Expiry] = true
	}
	strikes := sortedKeys(strikeSet)
	expiries := sortedKeys(expirySet)

	vol := make([][]float64, len(expiries))
	filled := make([][]bool, len(expiries))
	for i := range vol {
		vol[i] = make([]float64, len(strikes))
		filled[i] = make([]bool, len(strikes))
	}
	ei := indexMap(expiries)
	si := indexMap(strikes)
	for _, p := range points {
		i, j := ei[p.Expiry], si[p.Strike]
		if filled[i][j] {
			return nil, errors.New("volsurface: duplicate grid point")
		}
		vol[i][j] = p.Vol
		filled[i][j] = true
	}
	for i := range filled {
		for j := range filled[i] {
			if !filled[i][j] {
				return nil, errors.New("volsurface: incomplete grid (missing strike×expiry cell)")
			}
		}
	}
	return &Surface{strikes: strikes, expiries: expiries, vol: vol}, nil
}

// FromQuotes builds a Surface by solving each quote's implied vol against the
// shared spot/rate, then assembling the grid. Quotes whose price is outside the
// arbitrage band are skipped; an empty result is an error.
func FromQuotes(quotes []OptionQuote, S, r, q float64) (*Surface, error) {
	points := make([]Point, 0, len(quotes))
	for _, qt := range quotes {
		iv, ok := ImpliedVol(qt.Type, qt.Price, S, qt.Strike, qt.Expiry, r, q)
		if !ok {
			continue
		}
		points = append(points, Point{Strike: qt.Strike, Expiry: qt.Expiry, Vol: iv})
	}
	if len(points) == 0 {
		return nil, errors.New("volsurface: no quotes yielded a valid implied vol")
	}
	return Build(points)
}

// Vol returns the interpolated implied vol at (strike, expiry). Inside the grid
// it bilinearly interpolates; outside it clamps to the nearest edge (flat
// extrapolation). ok=false only for an empty surface.
func (s *Surface) Vol(strike, expiry float64) (float64, bool) {
	if s == nil || len(s.strikes) == 0 || len(s.expiries) == 0 {
		return 0, false
	}
	i0, i1, ti := bracket(s.expiries, expiry)
	j0, j1, tj := bracket(s.strikes, strike)
	// Bilinear blend of the four corners.
	v00 := s.vol[i0][j0]
	v01 := s.vol[i0][j1]
	v10 := s.vol[i1][j0]
	v11 := s.vol[i1][j1]
	top := v00*(1-tj) + v01*tj
	bot := v10*(1-tj) + v11*tj
	return top*(1-ti) + bot*ti, true
}

// bracket finds the indices lo,hi in a sorted axis that bound x and the
// interpolation weight w∈[0,1] so axis[lo]+w·(axis[hi]−axis[lo]) ≈ x. Outside
// the range it clamps (lo==hi, w=0).
func bracket(axis []float64, x float64) (lo, hi int, w float64) {
	n := len(axis)
	if x <= axis[0] {
		return 0, 0, 0
	}
	if x >= axis[n-1] {
		return n - 1, n - 1, 0
	}
	hi = sort.SearchFloat64s(axis, x)
	if axis[hi] == x {
		return hi, hi, 0
	}
	lo = hi - 1
	w = (x - axis[lo]) / (axis[hi] - axis[lo])
	return lo, hi, w
}

func sortedKeys(m map[float64]bool) []float64 {
	out := make([]float64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Float64s(out)
	return out
}

func indexMap(xs []float64) map[float64]int {
	m := make(map[float64]int, len(xs))
	for i, x := range xs {
		m[x] = i
	}
	return m
}
