package xva

import (
	"errors"
	"fmt"
	"sort"
)

// CDS term-structure bootstrap (PARITY-03c). FromCDS collapses one spread to a
// flat hazard via the credit triangle — fine for a single quote, wrong for a
// term structure: a 1y/5y/10y spread curve implies DIFFERENT forward hazards
// per segment, and CVA/DVA integrate against exactly those marginals.
// BootstrapCDS solves the piecewise-constant forward hazards of a CreditCurve
// so every quoted CDS reprices to par, sequentially per pillar (the FI-01b
// bootstrap stance applied to credit).
//
// Discounting enters through a plain df func(t) → DF seam so this package
// stays curve-free (the Adjustments flat-DiscountRate stance): the composition
// root passes the PARITY-03a calibrated curve's Discount, or exp(−r·t).
//
// # Pricing model
//
// Quarterly premium grid to maturity; per unit notional and unit spread the
// risky PV01 and protection leg over grid times t_i (Q = Survival, Δ accrual):
//
//	RPV01 = Σ_i Δ_i·DF(t_i)·(Q(t_i) + (Q(t_{i−1})−Q(t_i))/2)   (accrual-on-default half period)
//	Prot  = LGD · Σ_i DF(t_i)·(Q(t_{i−1})−Q(t_i))
//
// Par spread = Prot / RPV01. The bootstrap bisects each new segment's hazard
// until the model par spread hits the quote.

// CDSQuote is one par CDS spread: Tenor the maturity (years), Spread the
// quoted running spread (decimal, 0.01 = 100bp).
type CDSQuote struct {
	Tenor  float64
	Spread float64
}

// ErrCDSBootstrap is returned when the quote set cannot be bootstrapped
// (empty/non-ascending quotes, invalid recovery, or a spread curve so inverted
// it needs a negative forward hazard — an arbitrageable quote set).
var ErrCDSBootstrap = errors.New("xva: cannot bootstrap CDS quotes")

// BootstrapCDS builds the hazard term structure from par CDS quotes at a
// common recovery. df returns the risk-free discount factor at t; nil means
// undiscounted (DF ≡ 1). Quotes need not be pre-sorted.
func BootstrapCDS(quotes []CDSQuote, recovery float64, df func(t float64) float64) (CreditCurve, error) {
	if len(quotes) == 0 {
		return CreditCurve{}, fmt.Errorf("%w: no quotes", ErrCDSBootstrap)
	}
	if recovery < 0 || recovery >= 1 {
		return CreditCurve{}, fmt.Errorf("%w: recovery %.4g outside [0,1)", ErrCDSBootstrap, recovery)
	}
	if df == nil {
		df = func(float64) float64 { return 1 }
	}
	qs := make([]CDSQuote, len(quotes))
	copy(qs, quotes)
	sort.Slice(qs, func(i, j int) bool { return qs[i].Tenor < qs[j].Tenor })

	c := CreditCurve{Recovery: recovery}
	prev := 0.0
	for _, q := range qs {
		if q.Tenor <= 0 || q.Tenor <= prev {
			return CreditCurve{}, fmt.Errorf("%w: tenors must be strictly ascending and positive (%.4g after %.4g)", ErrCDSBootstrap, q.Tenor, prev)
		}
		if q.Spread < 0 {
			return CreditCurve{}, fmt.Errorf("%w: negative spread at %.4gy", ErrCDSBootstrap, q.Tenor)
		}
		h, err := solveHazard(c, q, df)
		if err != nil {
			return CreditCurve{}, err
		}
		c.Tenors = append(c.Tenors, q.Tenor)
		c.Hazards = append(c.Hazards, h)
		prev = q.Tenor
	}
	return c, nil
}

// solveHazard bisects the constant hazard on the new segment (prev, q.Tenor]
// so the CDS maturing at q.Tenor reprices to par. The par spread is monotone
// increasing in the segment hazard, so a sign change brackets the root; a
// quote below the zero-hazard model spread needs a negative forward hazard
// and is rejected.
func solveHazard(c CreditCurve, q CDSQuote, df func(float64) float64) (float64, error) {
	trial := func(h float64) float64 {
		t := CreditCurve{
			Tenors:   append(append([]float64{}, c.Tenors...), q.Tenor),
			Hazards:  append(append([]float64{}, c.Hazards...), h),
			Recovery: c.Recovery,
		}
		return t.ParSpread(q.Tenor, df) - q.Spread
	}
	lo, hi := 0.0, 20.0
	if trial(lo) > 1e-12 {
		return 0, fmt.Errorf("%w: quote %.6g at %.4gy needs a negative forward hazard", ErrCDSBootstrap, q.Spread, q.Tenor)
	}
	if trial(hi) < 0 {
		return 0, fmt.Errorf("%w: quote %.6g at %.4gy exceeds the solvable hazard range", ErrCDSBootstrap, q.Spread, q.Tenor)
	}
	for i := 0; i < 200 && hi-lo > 1e-14; i++ {
		mid := (lo + hi) / 2
		if trial(mid) < 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2, nil
}

// ParSpread returns the model par spread of a CDS maturing at T priced off the
// curve: protection leg over risky PV01 on the quarterly grid. df nil means
// undiscounted. T ≤ 0 returns 0.
func (c CreditCurve) ParSpread(T float64, df func(t float64) float64) float64 {
	if T <= 0 {
		return 0
	}
	if df == nil {
		df = func(float64) float64 { return 1 }
	}
	var rpv01, prot float64
	prevT, prevQ := 0.0, 1.0
	for _, t := range premiumGrid(T) {
		q := c.Survival(t)
		d := df(t)
		delta := t - prevT
		rpv01 += delta * d * (q + (prevQ-q)/2)
		prot += d * (prevQ - q)
		prevT, prevQ = t, q
	}
	if rpv01 == 0 {
		return 0
	}
	return c.LGD() * prot / rpv01
}

// premiumGrid is the quarterly payment schedule to maturity, with a final stub
// at T when T is not a multiple of 0.25.
func premiumGrid(T float64) []float64 {
	var ts []float64
	for t := 0.25; t < T-1e-9; t += 0.25 {
		ts = append(ts, t)
	}
	return append(ts, T)
}
