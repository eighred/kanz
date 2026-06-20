package compute

import (
	"context"
	"math"
	"time"

	"github.com/kanz-eng/kanz/internal/marketdata/store"
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
// this file supplies the provider and the binding.
//
// # Point-in-time correctness
//
// The provider reads the store as of the portfolio's state time (p.AsOf()),
// bounding BOTH the observation window and the bitemporal knowledge horizon at
// that instant — so a recompute (or a replayed/backtested one) sees only the
// prices observable and known then, never a later correction (MODEL-01i, the
// no-future-leakage property the store guarantees).

// ReturnMethod is the return-computation convention. Mirrors
// reference.v1.ReturnMethod.
type ReturnMethod int32

const (
	// ReturnSimple: (p_t - p_{t-1}) / p_{t-1}.
	ReturnSimple ReturnMethod = iota
	// ReturnLog: ln(p_t / p_{t-1}) — additive across time, the usual VaR input.
	ReturnLog
)

// Default historical-returns parameters. A ~1-trading-year lookback of
// log returns on the close series is the conventional historical-VaR input.
const (
	DefaultReturnWindow              = 250
	DefaultReturnMethod ReturnMethod = ReturnLog
)

// DefaultReturnKind is the price mark returns are derived from — the close.
var DefaultReturnKind = store.PriceKindClose

// ReturnsConfig parameterizes a StoreReturnsProvider.
type ReturnsConfig struct {
	// Window is the number of returns to look back when a caller does not
	// specify one. Reads Window+1 prices.
	Window int
	// Method is the return convention the provider yields.
	Method ReturnMethod
	// Kind is the price mark returns are computed from.
	Kind store.PriceKind
}

func (c ReturnsConfig) withDefaults() ReturnsConfig {
	if c.Window <= 0 {
		c.Window = DefaultReturnWindow
	}
	if c.Kind == store.PriceKindUnspecified {
		c.Kind = DefaultReturnKind
	}
	return c
}

// ReturnsProvider supplies an instrument's historical return series, read
// point-in-time-correct as of a knowledge horizon. VaR measures close over it
// (via BindReturns) rather than taking it as a parameter, so the MeasureFunc
// signature is preserved.
type ReturnsProvider interface {
	// Returns yields up to `window` most recent returns for instrumentID ending
	// at-or-before asOf, oldest first. A window <= 0 uses the provider default.
	// Returns an empty slice (not an error) when history is insufficient — the
	// VaR caller degrades to a zero/placeholder measure + quality flag rather
	// than failing, honoring the MeasureFunc "never error" contract.
	Returns(ctx context.Context, instrumentID string, asOf time.Time, window int) ([]float64, error)
}

// StoreReturnsProvider derives return series from the MODEL-01b price-history
// store. It is the concrete "historical-returns provider" the VaR closures
// depend on.
type StoreReturnsProvider struct {
	store store.Store
	cfg   ReturnsConfig
}

// NewStoreReturnsProvider builds a provider over s with cfg (zero fields take
// the package defaults).
func NewStoreReturnsProvider(s store.Store, cfg ReturnsConfig) *StoreReturnsProvider {
	return &StoreReturnsProvider{store: s, cfg: cfg.withDefaults()}
}

// Returns reads the point-in-time close series and differences it into returns.
func (p *StoreReturnsProvider) Returns(ctx context.Context, instrumentID string, asOf time.Time, window int) ([]float64, error) {
	if window <= 0 {
		window = p.cfg.Window
	}
	obs, err := p.store.History(ctx, store.Query{
		InstrumentID: instrumentID,
		Kind:         p.cfg.Kind,
		End:          asOf, // observation_time <= asOf
		AsOf:         asOf, // knowledge_time   <= asOf (no future leakage)
	})
	if err != nil {
		return nil, err
	}
	prices := make([]float64, 0, len(obs))
	for _, o := range obs {
		prices = append(prices, decimalToFloat(o.Price))
	}
	// window returns need window+1 prices; keep only the most recent tail.
	if n := window + 1; len(prices) > n {
		prices = prices[len(prices)-n:]
	}
	return computeReturns(prices, p.cfg.Method), nil
}

// computeReturns differences a price series (oldest first) into returns. A
// zero/negative previous price is skipped — it would make the ratio undefined
// (a corporate-action gap or bad mark, not a real return).
func computeReturns(prices []float64, method ReturnMethod) []float64 {
	if len(prices) < 2 {
		return nil
	}
	out := make([]float64, 0, len(prices)-1)
	for i := 1; i < len(prices); i++ {
		prev, cur := prices[i-1], prices[i]
		if prev <= 0 {
			continue
		}
		switch method {
		case ReturnLog:
			if cur <= 0 {
				continue
			}
			out = append(out, math.Log(cur/prev))
		default: // ReturnSimple
			out = append(out, (cur-prev)/prev)
		}
	}
	return out
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

// Compile-time assertion that StoreReturnsProvider satisfies ReturnsProvider.
var _ ReturnsProvider = (*StoreReturnsProvider)(nil)
