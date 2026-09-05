package compute

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/risk/factormodel"
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

// DefaultFitCadence is how often the live provider RE-ESTIMATES the factor
// model. Twenty-four hours, and the number is the schema's own stated intent:
// factor.v1's package doc says "a model is re-estimated daily, and a historical
// recompute/backtest reads the model that was in effect then".
//
// # Why a cadence had to be decided before the model could be published (#1039)
//
// The fit was PER EVALUATION. The cache was keyed on the exact requested as-of,
// and a portfolio's as-of is the timestamp of the newest event applied to it, so
// two recomputes of the same book a second apart produced two different fitted
// models. That is defensible while the model is a transient — it is the freshest
// estimate available for that instant — and it becomes indefensible the moment
// the model is an artifact somebody cites: model_id + as_of would key a
// different instance for every event the book received, the risk.factor topic
// would carry one model per recompute, and "the model that priced this" would
// name a fit that existed for the duration of one call.
//
// A cadence makes the model an INSTANCE with a lifetime. Within one period every
// measure on every portfolio resolves the same fit, so a limit that breached at
// 14:30 and held at 14:00 is attributable to the book rather than to the model —
// which is the second of the four desk questions #1039 lists.
//
// # What it is NOT: the fit is not moved to the period boundary
//
// The obvious spelling — snap the requested as-of down to the period boundary and
// fit there — was rejected, for two reasons that both bite.
//
// It would make every live evaluation a HISTORICAL one. bookuniverse's universe
// is the book as it stands now for any as-of, and it fires
// WithLookaheadObserver whenever the requested as-of predates the newest state
// held — the counter that exists so a backtest cannot quietly acquire a
// survivorship universe. Fitting at midnight while the book is at 14:30 would
// trip it on every fit, and a signal that fires always reports nothing.
//
// It would also date the model to an instant its inputs were not cut at. The
// as_of on a published FactorModel is the model's point-in-time key; stamping it
// with a boundary the estimator did not read to is exactly the "declares a
// verdict it never computed" shape this estate refuses elsewhere.
//
// So the period bounds HOW OFTEN a fit happens, not WHEN it is dated. The first
// evaluation in a period fits at its own as-of and that instance serves the rest
// of the period.
const DefaultFitCadence = 24 * time.Hour

// LiveModelProvider is the production ModelProvider: it fits the configured
// factor model over the live universe and reuses that fit for the rest of the
// cadence period, so all factor measures computed in one period resolve ONE
// model instance (and a historical recompute in an earlier period estimates on
// the data observable then). A fit failure or empty universe yields ok=false —
// the factor measures report zero rather than failing the recompute (the
// ModelProvider contract).
type LiveModelProvider struct {
	cfg       factormodel.Config
	universe  UniverseFunc
	providers factormodel.Providers
	cadence   time.Duration
	onFit     func(ctx context.Context, m *factormodel.Model)

	mu    sync.Mutex
	cache map[int64]*factormodel.Model // keyed by cadence-period epoch, UnixNano
}

// maxCachedFits bounds the per-period cache; past it the cache resets (a
// long backtest sweeps many periods, each visited once).
const maxCachedFits = 64

// LiveModelOption configures a LiveModelProvider.
type LiveModelOption func(*LiveModelProvider)

// WithFitCadence sets how often the model is re-estimated. Zero or negative
// selects DefaultFitCadence — there is deliberately no way to say "fit every
// time", because that is the state #1039 found: a model instance that exists for
// the duration of one call cannot be published, cited or reproduced.
func WithFitCadence(d time.Duration) LiveModelOption {
	return func(p *LiveModelProvider) {
		if d > 0 {
			p.cadence = d
		}
	}
}

// WithFitObserver is called ONCE PER FIT, with the model that was just
// estimated — the seam the composition root hangs the factor.v1 publisher on.
//
// ON THE FIT AND NOT ON THE READ, deliberately. A model instance is worth
// recording once; hanging the record on Model() would republish the same
// instance on every measure of every portfolio for the whole period, and the
// record's whole purpose is that (model_id, as_of) names one thing.
//
// It runs SYNCHRONOUSLY on the fitting call, which the cadence is what makes
// affordable: a publish now costs one bus round-trip per period rather than one
// per evaluation. An observer that blocks blocks a recompute, so an observer
// that can be slow must do its own handoff.
func WithFitObserver(fn func(ctx context.Context, m *factormodel.Model)) LiveModelOption {
	return func(p *LiveModelProvider) { p.onFit = fn }
}

// NewLiveModelProvider builds the provider. universe and providers.Returns are
// required; providers.Characteristics per the model type (the Fit contract).
func NewLiveModelProvider(cfg factormodel.Config, universe UniverseFunc, providers factormodel.Providers, opts ...LiveModelOption) *LiveModelProvider {
	p := &LiveModelProvider{
		cfg:       cfg,
		universe:  universe,
		providers: providers,
		cadence:   DefaultFitCadence,
		cache:     map[int64]*factormodel.Model{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// epoch is the cadence period asOf falls in — the cache key, and nothing else.
// See DefaultFitCadence for why it is not the fit's as-of.
func (p *LiveModelProvider) epoch(asOf time.Time) int64 {
	return asOf.UTC().Truncate(p.cadence).UnixNano()
}

// Model implements ModelProvider.
func (p *LiveModelProvider) Model(ctx context.Context, asOf time.Time) (*factormodel.Model, bool) {
	key := p.epoch(asOf)
	p.mu.Lock()
	cached, hit := p.cache[key]
	p.mu.Unlock()
	// THE SECOND CONDITION IS THE NO-LOOK-AHEAD ARM. A cached instance was fitted
	// at the as-of of the first evaluation in this period, and an evaluation that
	// arrives afterwards asking about an EARLIER instant would otherwise be priced
	// off a model that read data its own state had not seen. That is the leakage
	// the point-in-time discipline exists to prevent, so the earlier request pays
	// for its own fit instead. It is rare by construction — within one period the
	// as-ofs a live book produces are monotone — and it is not cached over the
	// period's instance, which would make the model walk backwards for everyone.
	if hit && !cached.AsOf.After(asOf) {
		return cached, true
	}

	instruments, err := p.universe(ctx, asOf)
	if err != nil || len(instruments) == 0 {
		return nil, false
	}
	m, err := factormodel.Fit(ctx, p.cfg, instruments, asOf, p.providers)
	if err != nil {
		return nil, false
	}
	if !hit {
		p.mu.Lock()
		if len(p.cache) >= maxCachedFits {
			p.cache = map[int64]*factormodel.Model{}
		}
		p.cache[key] = m
		p.mu.Unlock()
	}
	if p.onFit != nil {
		p.onFit(ctx, m)
	}
	return m, true
}

var _ ModelProvider = (*LiveModelProvider)(nil)
