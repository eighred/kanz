package compute

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/kanz-eng/kanz/internal/risk/factormodel"
)

// Live factor-model estimation (PARITY-03d). FACTOR-01 delivered the
// estimation math (factormodel.Fit) behind two data seams; this file supplies
// the PRODUCTION providers so the model is estimated on the live universe:
//
//   - StoreCharacteristicProvider derives real style characteristics from the
//     MODEL-01b price history (momentum, volatility — the two style factors
//     observable from prices alone) and takes the industry label through a
//     SectorFunc closure. The closure is deliberate: factor.Classifier lives in
//     compute/factor which imports THIS package, so the composition root adapts
//     Classify into a SectorFunc (the MODEL-01c closure-injection stance).
//   - LiveModelProvider implements ModelProvider by fitting the model over the
//     production book's universe point-in-time, cached per as-of so every
//     factor measure on one snapshot shares one fit (per-snapshot resolution).

// Style-characteristic names the store provider emits — list them in
// factormodel.Config.StyleFactors when wiring a fundamental/blend model.
const (
	StyleMomentum   = "Momentum"
	StyleVolatility = "Volatility"
)

// SectorFunc resolves an instrument's industry label as of a point in time —
// the composition root wraps factor.Classifier.Classify (sector key) here.
type SectorFunc func(ctx context.Context, instrumentID string, asOf time.Time) (string, bool)

// CharacteristicConfig parameterizes the derived style characteristics.
type CharacteristicConfig struct {
	// Window is the return lookback (trading days). 0 ⇒ 250.
	Window int
	// MomentumSkip excludes the most recent observations from momentum (the
	// 12-1 convention: skip the last month). 0 ⇒ 21.
	MomentumSkip int
}

func (c CharacteristicConfig) window() int {
	if c.Window <= 0 {
		return 250
	}
	return c.Window
}

func (c CharacteristicConfig) skip() int {
	if c.MomentumSkip <= 0 {
		return 21
	}
	return c.MomentumSkip
}

// StoreCharacteristicProvider is the production factormodel.CharacteristicProvider:
// momentum and volatility derived from the point-in-time return history, the
// industry label from the reference classification. An instrument with neither
// history nor classification is unknown (ok=false).
type StoreCharacteristicProvider struct {
	returns ReturnsProvider
	sector  SectorFunc
	cfg     CharacteristicConfig
}

// NewStoreCharacteristicProvider builds the provider over the MODEL-01c
// returns provider and an optional sector lookup (nil ⇒ no industry factor).
func NewStoreCharacteristicProvider(rp ReturnsProvider, sector SectorFunc, cfg CharacteristicConfig) *StoreCharacteristicProvider {
	return &StoreCharacteristicProvider{returns: rp, sector: sector, cfg: cfg}
}

// Characteristics implements factormodel.CharacteristicProvider.
func (p *StoreCharacteristicProvider) Characteristics(ctx context.Context, instrumentID string, asOf time.Time) (factormodel.Characteristics, bool) {
	var c factormodel.Characteristics
	if p.sector != nil {
		if ind, ok := p.sector(ctx, instrumentID, asOf); ok {
			c.Industry = ind
		}
	}
	rets, err := p.returns.Returns(ctx, instrumentID, asOf, p.cfg.window())
	if err != nil || len(rets) == 0 {
		if c.Industry == "" {
			return factormodel.Characteristics{}, false
		}
		return c, true // classified but priceless (e.g. a fresh listing)
	}
	momEnd := len(rets) - p.cfg.skip()
	if momEnd < 0 {
		momEnd = 0
	}
	compound := 1.0
	for _, r := range rets[:momEnd] {
		compound *= 1 + r
	}
	var mean, varSum float64
	for _, r := range rets {
		mean += r
	}
	mean /= float64(len(rets))
	for _, r := range rets {
		varSum += (r - mean) * (r - mean)
	}
	vol := 0.0
	if len(rets) > 1 {
		vol = math.Sqrt(varSum / float64(len(rets)-1))
	}
	c.Style = map[string]float64{
		StyleMomentum:   compound - 1,
		StyleVolatility: vol,
	}
	return c, true
}

var _ factormodel.CharacteristicProvider = (*StoreCharacteristicProvider)(nil)

// UniverseFunc supplies the estimation universe as of a point in time — the
// production book's instrument ids (the composition root reads them off the
// position store / IBOR book).
type UniverseFunc func(ctx context.Context, asOf time.Time) ([]string, error)

// LiveModelProvider is the production ModelProvider: it fits the configured
// factor model over the live universe at each requested as-of and caches the
// fit per as-of, so all factor measures on one portfolio snapshot resolve ONE
// model (and a historical recompute at an earlier as-of estimates on the data
// observable then). A fit failure or empty universe yields ok=false — the
// factor measures report zero rather than failing the recompute (the
// ModelProvider contract).
type LiveModelProvider struct {
	cfg       factormodel.Config
	universe  UniverseFunc
	providers factormodel.Providers

	mu    sync.Mutex
	cache map[int64]*factormodel.Model // keyed by asOf UnixNano
}

// maxCachedFits bounds the per-asOf cache; past it the cache resets (a
// long backtest sweeps many as-ofs, each visited once).
const maxCachedFits = 64

// NewLiveModelProvider builds the provider. universe and providers.Returns are
// required; providers.Characteristics per the model type (the Fit contract).
func NewLiveModelProvider(cfg factormodel.Config, universe UniverseFunc, providers factormodel.Providers) *LiveModelProvider {
	return &LiveModelProvider{
		cfg:       cfg,
		universe:  universe,
		providers: providers,
		cache:     map[int64]*factormodel.Model{},
	}
}

// Model implements ModelProvider.
func (p *LiveModelProvider) Model(ctx context.Context, asOf time.Time) (*factormodel.Model, bool) {
	key := asOf.UnixNano()
	p.mu.Lock()
	if m, ok := p.cache[key]; ok {
		p.mu.Unlock()
		return m, true
	}
	p.mu.Unlock()

	instruments, err := p.universe(ctx, asOf)
	if err != nil || len(instruments) == 0 {
		return nil, false
	}
	m, err := factormodel.Fit(ctx, p.cfg, instruments, asOf, p.providers)
	if err != nil {
		return nil, false
	}
	p.mu.Lock()
	if len(p.cache) >= maxCachedFits {
		p.cache = map[int64]*factormodel.Model{}
	}
	p.cache[key] = m
	p.mu.Unlock()
	return m, true
}

var _ ModelProvider = (*LiveModelProvider)(nil)
