// Package xva is the FACTOR-sibling counterparty-credit-risk / valuation-
// adjustment layer (XVA-01): Monte-Carlo future-exposure simulation over netting
// sets, the credit/funding valuation adjustments (CVA/DVA/FVA) derived from those
// exposure profiles, and the Basel SA-CCR standardized exposure. It is the
// derivatives-book risk Phase-5 pricing made computable (ROI #32) — the DERIV/FI
// pricers price a trade today, this prices the DISTRIBUTION of its future value
// against a counterparty who may default.
//
// # The exposure-simulation core
//
// Counterparty exposure is path-dependent and netting-set-level: it is the
// positive part of the netted mark-to-market of all trades with one counterparty,
// in the future, under collateral. So the simulator evolves the risk factors the
// trades depend on along correlated diffusion paths (the MODEL-01e correlated-
// scenario idea, extended from one-period P&L to a multi-step exposure path),
// reprices the netting set at each grid date through a Pricer seam (the DERIV/FI
// pricers plug in), nets, applies the CSA, and reduces the per-date exposure
// distribution to the EE / PFE profile XVA-01c and SA-CCR consume.
//
// # Boundary
//
// xva is INSIDE kanz/internal/risk, so it reuses the pricing package directly (no
// RISK-02 edge). The heavy reference/market wiring (a counterparty book, real
// trade terms, CDS-implied curves) enters through the Pricer / provider seams at
// the composition root — the same seam stance every prior risk epic took.
package xva

import (
	"math"
	"math/rand"
	"sort"
)

// MarketState is one simulated state of the world: risk-factor id → level. A
// Pricer reprices a trade against it.
type MarketState map[string]float64

// Pricer prices one trade's mark-to-market value at simulation time t (years from
// now) in a given market state — the seam the DERIV (BlackScholesPrice) / FI
// (Bond) pricers plug into. t is passed because exposure is inherently time-
// dependent (an option's residual maturity, a bond's accrual). A positive value
// is an asset to us (exposure if the counterparty defaults).
type Pricer interface {
	Value(t float64, state MarketState) float64
}

// Kind is how a risk factor diffuses.
type Kind int

const (
	// Lognormal: geometric Brownian motion S₀·exp((μ−½σ²)t + σW) — equity/FX/
	// commodity spot. Stays positive.
	Lognormal Kind = iota
	// Normal: arithmetic Brownian S₀ + μt + σW — a rate/spread that can go
	// negative.
	Normal
)

// RiskFactor is one stochastic driver of the netting set's value.
type RiskFactor struct {
	ID    string
	Spot  float64 // level at t=0
	Drift float64 // μ, annualized
	Vol   float64 // σ, annualized
	Kind  Kind
}

// CSA is the collateral agreement governing a netting set. The zero value (or a
// nil *CSA) is uncollateralized. A non-nil CSA collateralizes exposure above the
// Threshold once the change exceeds the MinTransferAmount, with an
// IndependentAmount of over-collateralization always held.
type CSA struct {
	Threshold         float64
	MinTransferAmount float64
	IndependentAmount float64
}

// collateralizedExposure returns the exposure max(V − C, 0) after the CSA posts
// collateral C against a netted value V. nil CSA ⇒ uncollateralized max(V,0).
func (c *CSA) collateralizedExposure(v float64) float64 {
	if c == nil {
		return math.Max(v, 0)
	}
	// Counterparty posts the amount of V above the threshold (when it clears the
	// MTA); we also hold the independent amount.
	call := v - c.Threshold
	collateral := c.IndependentAmount
	if call > 0 && call >= c.MinTransferAmount {
		collateral += call
	}
	return math.Max(v-collateral, 0)
}

// NettingSet is the set of trades with one counterparty that net against each
// other on default, under one CSA.
type NettingSet struct {
	CounterpartyID string
	Trades         []Pricer
	CSA            *CSA
}

// netValue is the netted mark-to-market of every trade in the set at time t.
func (s NettingSet) netValue(t float64, state MarketState) float64 {
	var v float64
	for _, tr := range s.Trades {
		v += tr.Value(t, state)
	}
	return v
}

// Config parameterizes the exposure Monte-Carlo.
type Config struct {
	// Paths is the number of simulated paths. 0 ⇒ DefaultPaths.
	Paths int
	// Seed makes the simulation deterministic (replay reproduces the profile).
	// 0 ⇒ DefaultSeed.
	Seed int64
	// Quantile is the PFE confidence (e.g. 0.95). 0 ⇒ DefaultQuantile.
	Quantile float64
}

// Simulation defaults.
const (
	DefaultPaths    = 5000
	DefaultSeed     = 1
	DefaultQuantile = 0.95
)

func (c Config) withDefaults() Config {
	if c.Paths <= 0 {
		c.Paths = DefaultPaths
	}
	if c.Seed == 0 {
		c.Seed = DefaultSeed
	}
	if c.Quantile <= 0 || c.Quantile >= 1 {
		c.Quantile = DefaultQuantile
	}
	return c
}

// ExposureProfile is the per-grid-date exposure summary of a netting set.
type ExposureProfile struct {
	// Times are the future grid dates (years), ascending.
	Times []float64
	// EE[k] is the expected (positive) exposure E[max(V−C,0)] at Times[k] — the
	// CVA driver. PFE[k] is the Quantile-level exposure. NEE[k] is the negative
	// expected exposure E[max(C−V,0)] — the DVA driver.
	EE  []float64
	PFE []float64
	NEE []float64
}

// EPE is the time-averaged expected exposure (the headline CVA weight).
func (p ExposureProfile) EPE() float64 { return timeAverage(p.Times, p.EE) }

// EEPE is the effective EPE: the time-average of the running maximum of EE (Basel
// uses the non-decreasing effective EE so a declining profile does not understate
// capital). Used by SA-CCR-adjacent capital.
func (p ExposureProfile) EEPE() float64 {
	eff := make([]float64, len(p.EE))
	run := 0.0
	for k, ee := range p.EE {
		if ee > run {
			run = ee
		}
		eff[k] = run
	}
	return timeAverage(p.Times, eff)
}

// PeakPFE is the maximum PFE across the profile — the headline counterparty
// exposure limit number.
func (p ExposureProfile) PeakPFE() float64 {
	var m float64
	for _, v := range p.PFE {
		if v > m {
			m = v
		}
	}
	return m
}

// Simulate runs the exposure Monte-Carlo for a netting set: it diffuses the risk
// factors along correlated paths, reprices and nets the set at each grid date,
// and reduces each date's exposure distribution to EE / PFE / NEE. corr is the
// factor correlation matrix (factor order matches `factors`); a nil/empty corr is
// treated as the identity (independent factors). grid is the ascending vector of
// future dates in years.
func (cfg Config) Simulate(set NettingSet, factors []RiskFactor, corr [][]float64, grid []float64) ExposureProfile {
	cfg = cfg.withDefaults()
	nF := len(factors)
	nT := len(grid)
	chol := choleskyOrIdentity(corr, nF)

	// exposures[k] / negs[k] collect the per-path exposure at grid date k.
	exposures := make([][]float64, nT)
	negs := make([][]float64, nT)
	for k := range exposures {
		exposures[k] = make([]float64, cfg.Paths)
		negs[k] = make([]float64, cfg.Paths)
	}

	rng := rand.New(rand.NewSource(cfg.Seed))
	w := make([]float64, nF) // accumulated correlated Brownian per factor
	z := make([]float64, nF)
	corrZ := make([]float64, nF)
	for p := 0; p < cfg.Paths; p++ {
		for i := range w {
			w[i] = 0
		}
		prevT := 0.0
		for k := 0; k < nT; k++ {
			dt := grid[k] - prevT
			if dt < 0 {
				dt = 0
			}
			prevT = grid[k]
			for i := range z {
				z[i] = rng.NormFloat64()
			}
			// corrZ = chol · z, then accumulate √dt-scaled Brownian increment.
			for i := 0; i < nF; i++ {
				var s float64
				for j := 0; j <= i; j++ {
					s += chol[i][j] * z[j]
				}
				corrZ[i] = s
				w[i] += corrZ[i] * math.Sqrt(dt)
			}
			state := make(MarketState, nF)
			for i, f := range factors {
				state[f.ID] = evolve(f, grid[k], w[i])
			}
			v := set.netValue(grid[k], state)
			exposures[k][p] = set.CSA.collateralizedExposure(v)
			negs[k][p] = (&CSA{}).negExposure(v, set.CSA)
		}
	}

	prof := ExposureProfile{Times: append([]float64(nil), grid...), EE: make([]float64, nT), PFE: make([]float64, nT), NEE: make([]float64, nT)}
	for k := 0; k < nT; k++ {
		prof.EE[k] = mean(exposures[k])
		prof.NEE[k] = mean(negs[k])
		sort.Float64s(exposures[k])
		prof.PFE[k] = quantile(exposures[k], cfg.Quantile)
	}
	return prof
}

// negExposure is the negative exposure max(C − V, 0) (what WE owe — the DVA leg),
// mirroring collateralizedExposure on the other side of the CSA.
func (*CSA) negExposure(v float64, c *CSA) float64 {
	if c == nil {
		return math.Max(-v, 0)
	}
	post := -v - c.Threshold
	collateral := c.IndependentAmount
	if post > 0 && post >= c.MinTransferAmount {
		collateral += post
	}
	return math.Max(-v-collateral, 0)
}

// evolve returns the factor level at time t given its accumulated Brownian w.
func evolve(f RiskFactor, t, w float64) float64 {
	switch f.Kind {
	case Normal:
		return f.Spot + f.Drift*t + f.Vol*w
	default: // Lognormal
		return f.Spot * math.Exp((f.Drift-0.5*f.Vol*f.Vol)*t+f.Vol*w)
	}
}
