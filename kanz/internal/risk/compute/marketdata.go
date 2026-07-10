package compute

import (
	"context"
	"time"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// MODEL-01c wires the historical market-data plane (MODEL-01b store) into the
// RISK-07 measure framework WITHOUT touching the measure call-site contract.
//
// # The injection seam
//
// A MeasureFunc is `func(*domain.Portfolio) v1.Measure` — pure signature, no
// data dependency. Real VaR needs a historical return series, which is a data
// dependency. BindReturns reconciles the two: it closes a provider-aware
// computation (ReturnsMeasure) over a ReturnsProvider + a captured ctx,
// producing a plain MeasureFunc the Registry stores and ComputeMeasures runs
// unchanged. The data enters by closure; the api/v1 contract, the MeasureSet
// shape, and every caller stay exactly as RISK-07 designed them. MODEL-01d
// (historical-simulation) and MODEL-01e (Monte-Carlo) supply the ReturnsMeasure;
// this file supplies the binding.
//
// The concrete provider (StoreReturnsProvider over the MODEL-01b store) and the
// return/volatility math live in internal/marketdata/returns — they are pure
// market-data analytics with no risk-domain dependency, shared with the
// lakehouse (DEBT-ARCH-01). This file keeps only the ReturnsProvider interface
// it consumes and the risk-domain binding; the composition root injects a
// returns.StoreReturnsProvider, which satisfies ReturnsProvider structurally.
//
// # Point-in-time correctness
//
// The provider reads the store as of the portfolio's state time (p.AsOf()),
// bounding BOTH the observation window and the bitemporal knowledge horizon at
// that instant — so a recompute (or a replayed/backtested one) sees only the
// prices observable and known then, never a later correction (MODEL-01i, the
// no-future-leakage property the store guarantees).

// ReturnsProvider supplies an instrument's historical return series, read
// point-in-time-correct as of a knowledge horizon. VaR measures close over it
// (via BindReturns) rather than taking it as a parameter, so the MeasureFunc
// signature is preserved. It is declared here (not imported from the returns
// package) at the point of use — the concrete returns.StoreReturnsProvider
// satisfies it structurally, so compute needs no dependency on that package.
type ReturnsProvider interface {
	// Returns yields up to `window` most recent returns for instrumentID ending
	// at-or-before asOf, oldest first. A window <= 0 uses the provider default.
	// Returns an empty slice (not an error) when history is insufficient — the
	// VaR caller degrades to a zero/placeholder measure + quality flag rather
	// than failing, honoring the MeasureFunc "never error" contract.
	Returns(ctx context.Context, instrumentID string, asOf time.Time, window int) ([]float64, error)
}

// ReturnsMeasure is a provider-aware measure computation. It may read historical
// returns (doing I/O through the provider) but otherwise carries the same
// meaning as a MeasureFunc. MODEL-01d/e implement concrete ones.
type ReturnsMeasure func(ctx context.Context, p *domain.Portfolio, rp ReturnsProvider) v1.Measure

// BindReturns is the MODEL-01c injection seam: it closes a ReturnsMeasure over a
// provider and a ctx, yielding a plain MeasureFunc for the RISK-07 Registry. The
// ctx is captured at bind time (typically the engine's app-scoped baseCtx, as
// the recomputer is debounced/async), so the pure func(*domain.Portfolio)
// signature carries no ctx of its own.
func BindReturns(ctx context.Context, rp ReturnsProvider, m ReturnsMeasure) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		return m(ctx, p, rp)
	}
}
