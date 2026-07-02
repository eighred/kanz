package volsurface

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/kanz-eng/kanz/internal/risk/pricing"
)

// Parametric vol-surface calibration (PARITY-03b). The DERIV-01c Surface
// interpolates raw implied-vol quotes bilinearly — faithful to the quotes but
// with no structure between them and no arbitrage discipline. FitSVI fits each
// expiry slice to the raw SVI parameterization of total implied variance
//
//	w(k) = a + b·(ρ·(k−m) + √((k−m)² + σ²)),  k = ln(K/F)
//
// (Gatheral), then REJECTS the fit unless it is arbitrage-free: Gatheral's
// butterfly density condition g(k) ≥ 0 on every slice, and calendar
// monotonicity (total variance non-decreasing in expiry at fixed moneyness)
// across slices — a violating quote set errors rather than feeding the Greek
// pricers a surface admitting negative densities. SVI is the listed-equity
// parameterization; a rates SABR cube plugs in behind the same VolProvider
// seam when swaption calibration lands.
//
// The fit is the quasi-explicit calibration: for fixed (m, σ) the slice is
// LINEAR in (a, d=ρbσ, c=bσ) — solved by least squares — and the outer (m, σ)
// pair by a coarse-to-fine grid search. Deterministic, dependency-free.

// SVIParams is one raw-SVI expiry slice over log-moneyness k = ln(K/F).
type SVIParams struct {
	A, B, Rho, M, Sigma float64
}

// TotalVar returns w(k), the total implied variance at log-moneyness k.
func (p SVIParams) TotalVar(k float64) float64 {
	d := k - p.M
	return p.A + p.B*(p.Rho*d+math.Sqrt(d*d+p.Sigma*p.Sigma))
}

// dw and d2w are the analytic first/second derivatives of w(k).
func (p SVIParams) dw(k float64) float64 {
	d := k - p.M
	return p.B * (p.Rho + d/math.Sqrt(d*d+p.Sigma*p.Sigma))
}

func (p SVIParams) d2w(k float64) float64 {
	d := k - p.M
	s := math.Sqrt(d*d + p.Sigma*p.Sigma)
	return p.B * p.Sigma * p.Sigma / (s * s * s)
}

// ButterflyFree reports whether the slice admits no butterfly arbitrage on
// [kLo, kHi]: Gatheral's density function
//
//	g(k) = (1 − k·w′/(2w))² − (w′²/4)·(1/w + ¼) + w″/2
//
// must be ≥ 0 (and w > 0) everywhere — g < 0 means a negative implied density.
func (p SVIParams) ButterflyFree(kLo, kHi float64) bool {
	const steps = 200
	for i := 0; i <= steps; i++ {
		k := kLo + (kHi-kLo)*float64(i)/steps
		w := p.TotalVar(k)
		if w <= 0 {
			return false
		}
		w1, w2 := p.dw(k), p.d2w(k)
		g := (1-k*w1/(2*w))*(1-k*w1/(2*w)) - (w1*w1/4)*(1/w+0.25) + w2/2
		if g < 0 {
			return false
		}
	}
	return true
}

// ErrSVIFit is returned when a quote set cannot be fitted (too few valid
// quotes on a slice, or no feasible parameters).
var ErrSVIFit = errors.New("volsurface: cannot fit SVI")

// ErrArbitrage is returned when the fitted surface admits butterfly or
// calendar arbitrage — the quote set is rejected, never served.
var ErrArbitrage = errors.New("volsurface: fitted surface admits arbitrage")

// SVISurface is a fitted, arbitrage-checked surface: one SVI slice per expiry
// with its forward. Vol interpolates total variance linearly in expiry at
// fixed strike and clamps beyond the fitted expiries (the Surface stance).
type SVISurface struct {
	expiries []float64 // ascending, years
	forwards []float64
	slices   []SVIParams
}

// FitSVI calibrates an SVISurface from listed-option quotes: implied vols are
// backed out per quote (reusing ImpliedVol), grouped by expiry into total-
// variance smiles, each fitted to raw SVI, then the butterfly + calendar
// checks gate the result. disc supplies the zero rate per expiry (the
// PARITY-03a calibrated curve satisfies pricing.DiscountCurve); divYield is
// the flat dividend/carry yield. A slice needs ≥ 3 valid quotes.
func FitSVI(quotes []OptionQuote, spot float64, disc pricing.DiscountCurve, divYield float64) (*SVISurface, error) {
	if spot <= 0 || disc == nil {
		return nil, fmt.Errorf("%w: spot and discount curve required", ErrSVIFit)
	}
	type smilePt struct{ k, w float64 }
	smiles := map[float64][]smilePt{}
	for _, q := range quotes {
		r := disc.Rate(q.Expiry)
		iv, ok := ImpliedVol(q.Type, q.Price, spot, q.Strike, q.Expiry, r, divYield)
		if !ok {
			continue // outside the arbitrage band — same skip as FromQuotes
		}
		fwd := spot * math.Exp((r-divYield)*q.Expiry)
		smiles[q.Expiry] = append(smiles[q.Expiry], smilePt{
			k: math.Log(q.Strike / fwd),
			w: iv * iv * q.Expiry,
		})
	}
	if len(smiles) == 0 {
		return nil, fmt.Errorf("%w: no quotes yielded a valid implied vol", ErrSVIFit)
	}

	surf := &SVISurface{}
	for _, expiry := range sortedFloatKeys(smiles) {
		pts := smiles[expiry]
		if len(pts) < 3 {
			return nil, fmt.Errorf("%w: %d quotes at expiry %.4g (need ≥3)", ErrSVIFit, len(pts), expiry)
		}
		ks := make([]float64, len(pts))
		ws := make([]float64, len(pts))
		for i, p := range pts {
			ks[i], ws[i] = p.k, p.w
		}
		params, err := fitSlice(ks, ws)
		if err != nil {
			return nil, fmt.Errorf("%w at expiry %.4g", err, expiry)
		}
		kLo, kHi := minF(ks)-0.5, maxF(ks)+0.5
		if !params.ButterflyFree(kLo, kHi) {
			return nil, fmt.Errorf("%w: butterfly (negative density) at expiry %.4g", ErrArbitrage, expiry)
		}
		r := disc.Rate(expiry)
		surf.expiries = append(surf.expiries, expiry)
		surf.forwards = append(surf.forwards, spot*math.Exp((r-divYield)*expiry))
		surf.slices = append(surf.slices, params)
	}

	// Calendar: total variance non-decreasing in expiry at fixed moneyness.
	const kSpan = 1.0
	for i := 1; i < len(surf.slices); i++ {
		for j := 0; j <= 40; j++ {
			k := -kSpan + 2*kSpan*float64(j)/40
			if surf.slices[i].TotalVar(k) < surf.slices[i-1].TotalVar(k)-1e-9 {
				return nil, fmt.Errorf("%w: calendar (total variance decreasing %.4g→%.4g at k=%.2f)",
					ErrArbitrage, surf.expiries[i-1], surf.expiries[i], k)
			}
		}
	}
	return surf, nil
}

// fitSlice runs the quasi-explicit calibration for one smile: grid over
// (m, σ) with a linear least-squares inner solve for (a, d, c), coarse then
// refined around the best cell. A flat slice (b=0 at the mean) is always a
// candidate, so a vol-constant smile fits without degeneracy.
func fitSlice(ks, ws []float64) (SVIParams, error) {
	kLo, kHi := minF(ks), maxF(ks)
	mean := 0.0
	for _, w := range ws {
		mean += w
	}
	mean /= float64(len(ws))
	best := SVIParams{A: mean} // flat baseline: b=0, w ≡ mean
	if best.A < 0 {
		best.A = 0
	}
	bestSSE := sse(best, ks, ws)

	span := kHi - kLo
	if span <= 0 {
		span = 0.5
	}
	try := func(m, sigma float64) {
		p, ok := solveLinear(ks, ws, m, sigma)
		if !ok {
			return
		}
		if s := sse(p, ks, ws); s < bestSSE {
			best, bestSSE = p, s
		}
	}
	for i := 0; i <= 16; i++ {
		m := kLo - 0.25*span + 1.5*span*float64(i)/16
		for j := 0; j <= 14; j++ {
			try(m, 0.02*math.Pow(100, float64(j)/14)) // σ ∈ [0.02, 2] geometric
		}
	}
	// One refinement pass around the incumbent.
	m0, s0 := best.M, best.Sigma
	if s0 <= 0 {
		s0 = 0.2
	}
	for i := -4; i <= 4; i++ {
		for j := -4; j <= 4; j++ {
			try(m0+0.05*span*float64(i), s0*math.Pow(1.15, float64(j)))
		}
	}
	return best, nil
}

// solveLinear solves the inner least squares for fixed (m, σ): with
// y=(k−m)/σ, z=√(y²+1), fit w ≈ a + d·y + c·z, then b=c/σ, ρ=d/c. Candidates
// violating SVI feasibility (c<0, |ρ|>1, w_min<0) are rejected; |ρ|>1 falls
// back to a clamped-ρ two-variable solve.
func solveLinear(ks, ws []float64, m, sigma float64) (SVIParams, bool) {
	n := float64(len(ks))
	var sy, sz, syy, szz, syz, sw, swy, swz float64
	for i := range ks {
		y := (ks[i] - m) / sigma
		z := math.Sqrt(y*y + 1)
		w := ws[i]
		sy += y
		sz += z
		syy += y * y
		szz += z * z
		syz += y * z
		sw += w
		swy += w * y
		swz += w * z
	}
	// Cramer on the 3×3 normal equations.
	det := n*(syy*szz-syz*syz) - sy*(sy*szz-syz*sz) + sz*(sy*syz-syy*sz)
	if math.Abs(det) < 1e-12 {
		return SVIParams{}, false
	}
	a := (sw*(syy*szz-syz*syz) - sy*(swy*szz-syz*swz) + sz*(swy*syz-syy*swz)) / det
	d := (n*(swy*szz-swz*syz) - sw*(sy*szz-syz*sz) + sz*(sy*swz-swy*sz)) / det
	c := (n*(syy*swz-syz*swy) - sy*(sy*swz-swy*sz) + sw*(sy*syz-syy*sz)) / det
	if c < 0 {
		return SVIParams{}, false
	}
	rho := 0.0
	if c > 1e-12 {
		rho = d / c
	}
	if math.Abs(rho) > 1 {
		// Clamp ρ and re-solve (a, c) with u = ρ·y + z.
		rho = math.Copysign(0.999, rho)
		var su, suu, swu float64
		for i := range ks {
			y := (ks[i] - m) / sigma
			u := rho*y + math.Sqrt(y*y+1)
			su += u
			suu += u * u
			swu += ws[i] * u
		}
		det2 := n*suu - su*su
		if math.Abs(det2) < 1e-12 {
			return SVIParams{}, false
		}
		a = (sw*suu - su*swu) / det2
		c = (n*swu - su*sw) / det2
		if c < 0 {
			return SVIParams{}, false
		}
	}
	p := SVIParams{A: a, B: c / sigma, Rho: rho, M: m, Sigma: sigma}
	if p.A+p.B*p.Sigma*math.Sqrt(1-p.Rho*p.Rho) < 0 {
		return SVIParams{}, false // minimum total variance negative
	}
	return p, true
}

func sse(p SVIParams, ks, ws []float64) float64 {
	s := 0.0
	for i := range ks {
		d := p.TotalVar(ks[i]) - ws[i]
		s += d * d
	}
	return s
}

// Vol returns the fitted implied vol at (strike, expiry): total variance
// interpolated linearly in expiry at fixed strike, vol = √(w/T). Beyond the
// fitted expiries the nearest slice's vol clamps. ok=false only for an empty
// surface or non-positive inputs.
func (s *SVISurface) Vol(strike, expiry float64) (float64, bool) {
	if s == nil || len(s.slices) == 0 || strike <= 0 || expiry <= 0 {
		return 0, false
	}
	wAt := func(i int) float64 {
		return s.slices[i].TotalVar(math.Log(strike / s.forwards[i]))
	}
	n := len(s.expiries)
	if expiry <= s.expiries[0] {
		return math.Sqrt(wAt(0) / s.expiries[0]), true
	}
	if expiry >= s.expiries[n-1] {
		return math.Sqrt(wAt(n-1) / s.expiries[n-1]), true
	}
	i := sort.SearchFloat64s(s.expiries, expiry)
	if s.expiries[i] == expiry {
		return math.Sqrt(wAt(i) / expiry), true
	}
	t0, t1 := s.expiries[i-1], s.expiries[i]
	w := wAt(i-1) + (wAt(i)-wAt(i-1))*(expiry-t0)/(t1-t0)
	return math.Sqrt(w / expiry), true
}

// Expiries returns the fitted slice expiries (ascending). Copy.
func (s *SVISurface) Expiries() []float64 {
	out := make([]float64, len(s.expiries))
	copy(out, s.expiries)
	return out
}

// Slice returns the fitted SVI parameters at the i-th expiry.
func (s *SVISurface) Slice(i int) SVIParams { return s.slices[i] }

// Store is the point-in-time surface store behind the DERIV-01d VolProvider
// seam (the vol mirror of the PARITY-03a curve.Store): each Refresh publishes
// a fitted surface versioned by as-of; a read resolves the surface live at the
// requested as_of. Satisfies compute.VolProvider implicitly.
type Store struct {
	mu           sync.RWMutex
	byUnderlying map[string][]surfaceVersion
}

type surfaceVersion struct {
	asOf time.Time
	s    *SVISurface
}

// NewStore returns an empty point-in-time surface store.
func NewStore() *Store {
	return &Store{byUnderlying: map[string][]surfaceVersion{}}
}

// Put publishes a surface for the underlying effective at asOf; out-of-order
// versions insert in place, same-asOf replaces (the curve.Store contract).
func (st *Store) Put(underlyingID string, asOf time.Time, s *SVISurface) {
	st.mu.Lock()
	defer st.mu.Unlock()
	vs := st.byUnderlying[underlyingID]
	i := sort.Search(len(vs), func(i int) bool { return !vs[i].asOf.Before(asOf) })
	if i < len(vs) && vs[i].asOf.Equal(asOf) {
		vs[i].s = s
	} else {
		vs = append(vs, surfaceVersion{})
		copy(vs[i+1:], vs[i:])
		vs[i] = surfaceVersion{asOf: asOf, s: s}
	}
	st.byUnderlying[underlyingID] = vs
}

// Vol resolves the surface live at asOf and evaluates it — the
// compute.VolProvider contract.
func (st *Store) Vol(_ context.Context, underlyingID string, strike, ttmYears float64, asOf time.Time) (float64, bool) {
	st.mu.RLock()
	vs := st.byUnderlying[underlyingID]
	i := sort.Search(len(vs), func(i int) bool { return vs[i].asOf.After(asOf) })
	st.mu.RUnlock()
	if i == 0 {
		return 0, false
	}
	return vs[i-1].s.Vol(strike, ttmYears)
}

// Calibrator ties the existing QuoteProvider seam to the fit and the store:
// pull quotes, fit + arb-check, publish point-in-time. A failed fit or an
// arbitrage-violating quote set leaves the previous surface serving.
type Calibrator struct {
	Source   QuoteProvider
	Store    *Store
	Disc     pricing.DiscountCurve // zero rates (the PARITY-03a curve)
	DivYield float64
}

// Refresh fits the underlying's surface from quotes as of asOf and publishes
// it, returning the fitted surface.
func (cal *Calibrator) Refresh(ctx context.Context, underlyingID string, asOf time.Time) (*SVISurface, error) {
	quotes, spot, err := cal.Source.Quotes(ctx, underlyingID, asOf)
	if err != nil {
		return nil, fmt.Errorf("volsurface: quote source for %s: %w", underlyingID, err)
	}
	s, err := FitSVI(quotes, spot, cal.Disc, cal.DivYield)
	if err != nil {
		return nil, err
	}
	cal.Store.Put(underlyingID, asOf, s)
	return s, nil
}

func sortedFloatKeys[V any](m map[float64]V) []float64 {
	out := make([]float64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Float64s(out)
	return out
}

func minF(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

func maxF(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}
