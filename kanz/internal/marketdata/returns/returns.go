// Package returns derives instrument return series and return volatility from
// the MODEL-01b point-in-time market-data store. These are market-data
// analytics — pure functions of the price history with no dependency on the
// risk domain — so they live under internal/marketdata, next to the store they
// read, shared by every consumer that needs one consistent history:
//
//   - the risk engine's VaR + uncertainty path, which consumes them through the
//     compute.ReturnsProvider / compute.VolModel interfaces these concrete types
//     satisfy structurally (no import from compute — the factormodel stance); and
//   - the lakehouse feature dataset (LAKE-01), which derives point-in-time
//     features from the same code.
//
// One implementation is what makes a materialized feature bit-identical to the
// value the live engine computed — the property LAKE-01e's backtest-reproduces-
// live test leans on. This package moved out of internal/risk/compute (DEBT-ARCH-01)
// so the lakehouse no longer reaches past the risk module's api/v1 boundary for it.
//
// # Point-in-time correctness
//
// Every read is bounded at the caller's asOf on BOTH axes — observation time and
// the bitemporal knowledge horizon — so a recompute, backtest, or feature
// materialization sees only the prices observable and known then, never a later
// correction (MODEL-01i, the no-future-leakage property the store guarantees).
package returns

import (
	"context"
	"math"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

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
// point-in-time-correct as of a knowledge horizon. It is declared here (the
// abstraction's home) for NewReturnsVolModel; consuming packages that also take
// a provider (compute's VaR binding, factormodel) declare their own
// structurally-identical interface at the point of use, so no package has to
// import another for the type.
type ReturnsProvider interface {
	// Returns yields up to `window` most recent returns for instrumentID ending
	// at-or-before asOf, oldest first. A window <= 0 uses the provider default.
	// Returns an empty slice (not an error) when history is insufficient — the
	// caller degrades to a zero/placeholder result + quality flag rather than
	// failing.
	Returns(ctx context.Context, instrumentID string, asOf time.Time, window int) ([]float64, error)
}

// StoreReturnsProvider derives return series from the MODEL-01b price-history
// store. It is the concrete "historical-returns provider" the VaR closures and
// the feature dataset depend on.
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

// decimalToFloat converts an exact common.v1.Decimal to float64. Prices stay
// exact Decimal on the wire/store (double is banned, EVT-10); the lossy
// conversion is confined to the analytics plane, where a float64 is the input.
func decimalToFloat(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

// Compile-time assertion that StoreReturnsProvider satisfies ReturnsProvider.
var _ ReturnsProvider = (*StoreReturnsProvider)(nil)
