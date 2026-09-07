// Package liquiditysource resolves a position's liquidity reference — ADV and
// spread — from the MODEL-01b durable bar series, satisfying liquidity.Provider:
// the seam compute.RegisterLiquidityRisk consumes, and which had ZERO production
// implementations (#509). It is the liquidity sibling of internal/risk/spotsource
// (the production SpotProvider) and internal/risk/termsource (contract terms):
// one small package per seam, wrapping the store the estate already fills.
//
// # THE HALF THAT IS MEASURABLE, AND THE HALF THAT IS NOT
//
// liquidity.LiquiditySpec carries two numbers and this repository can supply
// exactly one of them.
//
// ADV IS MEASURABLE. store.Bar carries Volume on every candle, the ingest path
// writes 1-minute bars for every configured (instrument, venue), and BarQuery
// reads them point-in-time. Summing volume into UTC-day buckets and averaging
// over the days the series TOUCHED is a real measurement of a real tape — of the
// intervals the series holds, and only those. It is not a measurement of the
// intervals it does not hold, and the difference is counted rather than assumed:
// see dailyTotals and ReasonWindowNotWhole (#591).
//
// SPREAD IS NOT MEASURABLE HERE. Nothing in this repository persists a bid or an
// ask. The evidence, checked rather than assumed:
//
//   - marketdata.Translate folds market.v1.Quote to a MID (obs.Price =
//     midDecimal(bid, ask), internal/marketdata/ingest.go) and stores it as
//     PriceKindMid. Both sides are discarded at the ingest boundary; a mid has no
//     width, so the spread cannot be recovered from what was kept.
//   - No migration under services/*/migrations creates a bid, ask or spread
//     column. price_history holds (instrument, kind, price); ohlcv_bars holds
//     OHLCV.
//   - internal/marketedge/book holds a live order book, in memory, in one
//     process. It has no writer to any store, so it is gone at restart and
//     invisible to a risk engine in another pod.
//   - reference.v1.LiquidityProfile — the schema whose average_spread field this
//     type mirrors — has zero Go consumers. It is a message with no store, no
//     writer and no reader.
//
// So this provider MEASURES ADV and can only ever be TOLD a spread. That
// asymmetry is the whole design below, and it is not smoothed over: the
// deployment must SPELL which of the two spread postures it wants, because they
// produce different lies if chosen by accident.
//
// # WHAT A ZERO SPREAD DOES DOWNSTREAM, AND WHAT ok=false DOES INSTEAD
//
// Both postures are degraded. They are degraded in different measures, and the
// choice is which measure carries the degradation:
//
//	Model.CostFraction returns 0 when spread <= 0 (impact.go). So a spread of
//	zero makes LiquidationCost exactly 0 and LVaR99 exactly VaR99 — a
//	liquidity-ADJUSTED VaR that is definitionally never an adjustment.
//
//	liquidity.Profile skips a position whose provider answers false, and
//	compute.liquidationHorizonMeasure emits prof.WeightedDays, which is 0 when no
//	position resolved. So refusing every instrument makes LiquidationHorizon
//	report 0.0000 DAYS — "this book liquidates instantly", the most liquid answer
//	the measure can give, for a book about which nothing is known.
//
// THAT ASYMMETRY DECIDES IT. A zero spread degrades LVaR99 toward VaR99, which
// is the number the engine already serves and therefore adds nothing while
// subtracting nothing. A blanket ok=false degrades LiquidationHorizon toward
// zero, which is a NEW claim, in the unsafe direction, and it is exactly the
// shape AGENTS.md forbids: "nothing configured" and "checked, and fine" reading
// the same. WithNoSpread therefore exists — but it is opt-in and spelled, never
// a zero-valued config field, and every spread it serves fires the observer so
// the degeneracy is COUNTED rather than inferred from a suspiciously round LVaR.
//
// # ASSUMED IS NEVER MEASURED, AND THE API SAYS SO
//
// The only legitimate source of a spread here is a desk that entered one
// deliberately: WithAssumedSpreads takes a per-instrument map. There is no
// default spread constant in this package and there must not be one — a
// hardcoded 5bps would put an invented number inside LVaR99 with nothing
// anywhere to say it was invented, which is the failure #345 rules on for
// credit ("a SUCCESSFUL calibration of invented quotes is worse than a failed
// one"). An assumption a human typed into a config is auditable; a constant
// compiled into a risk library is not.
//
// # THE READ COST, AND THE COARSE SERIES THAT FIXES IT
//
// The base series is 1-minute. At DefaultWindow that is 28 x 1440 = 40,320 rows
// per instrument per call, and compute calls this TWICE per position
// (LiquidationProfile and LiquidationCost each resolve the whole book). A
// 500-name portfolio is therefore ~40M rows per risk request. That was the
// strongest argument against wiring RegisterLiquidityRisk, and it is recorded
// here rather than discovered in production.
//
// A DAILY ROLLUP MAKES THE SAME ADV OUT OF 28 ROWS, and that is not an
// approximation. dailyTotals sums volume and counts DISTINCT UTC DAYS, so the
// statistic is INVARIANT under the resolution it is measured from: a faithful 1d
// rollup of the same window has the same total volume and the same day count,
// therefore the same mean. A 1h series likewise. The only thing that changes is
// the row count — 40,320 → 672 → 28.
//
// Config.ResolutionPreference is how a deployment takes that: an ordered,
// coarsest-first list, of which the provider uses the FIRST that can actually
// produce an ADV. It is OPT-IN and the default is still the 1-minute base
// series, because a coarse series that is EMPTY is the worst outcome available
// here — it returns no bars, which refuses the instrument, which
// liquidity.Profile drops, which makes LiquidationHorizon report zero days: "this
// book liquidates instantly", for a book nobody measured. A silent default onto
// a series no job in this repository is known to write would be exactly that
// failure, dressed as an optimisation.
//
// A cache is deliberately NOT the answer to the read cost: an exact memo would
// have to key on asOf to stay point-in-time honest, which is a second design
// with its own staleness failure mode, and it belongs in its own change with its
// own evidence.
package liquiditysource

import (
	"context"
	"fmt"
	decutil "github.com/eighred/kanz/internal/dec"
	"math"
	"time"

	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/risk/liquidity"
)

// BarStore is the one read this provider needs from the market-data store.
//
// NARROWED ON PURPOSE, and declared at the consumer where Go puts an interface.
// *store.Postgres, *store.Memory and store.BarStore all satisfy it, so the
// composition root passes its real store and a test passes four lines of fake
// without a database — and this provider cannot grow a PutBars call without this
// interface changing in the diff.
type BarStore interface {
	// Bars returns the point-in-time-correct series, ordered by BucketStart
	// ascending. BarQuery.AsOf is the KNOWLEDGE horizon — see store.BarQuery.
	Bars(ctx context.Context, q store.BarQuery) ([]store.Bar, error)
}

// Defaults.
const (
	// DefaultWindow is four calendar weeks of history read back from the last
	// COMPLETE day before the valuation time.
	//
	// # Why calendar weeks and not trading days
	//
	// This platform has no trading calendar — nothing anywhere knows which days a
	// venue was open. So the window is calendar-based and the number of
	// CONTRIBUTING days is venue-dependent: a 24/7 crypto venue contributes ~28
	// days, a western equity venue ~20. That is tolerable precisely because the
	// statistic is a MEAN PER DAY rather than a total: both populations estimate
	// the same quantity, and only the standard error differs. A window expressed
	// in trading days would need the calendar this repository does not have, and
	// inventing one is how a holiday becomes a zero-volume day that halves an ADV.
	//
	// Four weeks is the shortest window that spans a full monthly cycle
	// (option expiries, index rebalances, month-end flows) so that a name whose
	// liquidity is concentrated on those dates is not measured entirely on or
	// entirely off them.
	DefaultWindow = 28 * 24 * time.Hour

	// DefaultMinDays is the fewest days the series must TOUCH to produce an ADV.
	//
	// TOUCHED, NOT COMPLETE. dailyTotals counts a day that holds one bar exactly
	// as it counts a day that holds all 1440, so this floor bounds how many days
	// the mean is spread over and NOT how much of each of them was observed. The
	// second question has no answer from the bar series alone; ReasonWindowNotWhole
	// counts it instead (#591).
	//
	// # Why there is a floor at all
	//
	// ADV is the denominator of liquidity.Model.DaysToLiquidate, so the horizon is
	// LINEAR in it: an ADV measured off one unusual day — a listing day, an
	// index add, a liquidation cascade — moves the reported days-to-liquidate by
	// the same factor it moves the volume. And the result does not LOOK
	// provisional. It is a finite number of days, in range, indistinguishable at
	// the measure from an ADV averaged over a month.
	//
	// So an instrument listed three days ago gets NO liquidity rather than a
	// noisy one, and the refusal is reported (ReasonInsufficientHistory carries
	// the day count) so a new listing is visibly excluded rather than silently
	// mis-sized. The cost of that choice is real and runs the other way: a
	// refused position is skipped by liquidity.Profile, so its notional is absent
	// from the weighted horizon entirely — it is neither liquid nor counted in
	// IlliquidNotional. A book of new listings would report a horizon over the
	// names it happens to know.
	//
	// # Where five comes from
	//
	// One week. It is the shortest span containing a complete weekly cycle for a
	// 24/7 venue and a complete Mon-Fri for an equity, so no single weekday's
	// characteristic volume dominates the mean. Below it the estimate is a
	// single-observation artifact wearing the shape of an average.
	DefaultMinDays = 5

	// MaxAssumedSpread bounds what WithAssumedSpreads will accept. A spread is a
	// FRACTION OF PRICE (0.0005 = 5bps, per liquidity.LiquiditySpec), so a value
	// at or above 1 says crossing the book costs at least the entire notional.
	// That is not a wide market, it is a unit error — basis points entered where
	// a fraction was wanted — and CostFraction's cap at 1 would absorb it
	// silently, turning every position's liquidation cost into its full value.
	// Refused at construction, where a typo is still cheap.
	MaxAssumedSpread = 1.0
)

// The reasons a liquidity resolution was refused or degraded.
//
// A SMALL CLOSED SET, NOT FREE TEXT, for the same reason spotsource's are: the
// caller counts by this string and a metric label must not be whatever a future
// edit writes.
//
// Upstream every refusal is anonymous — liquidity.LiquidationProfile simply
// `continue`s on ok=false — so a book that was never ingested, one read at the
// wrong venue, one whose names are all too new, and one whose desk forgot to
// enter a spread are four different operational faults with four different
// fixes, and at the measure they are one absent position.
const (
	// ReasonNoAsOf: the caller passed a zero valuation time. Refused before the
	// store is touched — see Liquidity.
	ReasonNoAsOf = "no_as_of"
	// ReasonNoAssumedSpread: WithAssumedSpreads is in force, this instrument is
	// not in the map, and WithNoSpread was not passed — so the desk declared it
	// supplies spreads and this name is a gap in its own table. NOT a licence to
	// serve a zero: see the package doc on what a zero spread costs.
	ReasonNoAssumedSpread = "no_assumed_spread"
	// ReasonSpreadUnavailable: the ADV resolved and the spread is ZERO because
	// WithNoSpread was passed. THE ONE REASON HERE THAT ACCOMPANIES ok=true. It
	// fires on every such resolution because the consequence is invisible
	// downstream: LVaR99 will equal VaR99 exactly, and a liquidity-adjusted VaR
	// that never adjusts is indistinguishable from a perfectly liquid book.
	ReasonSpreadUnavailable = "spread_unavailable"
	// ReasonWindowNotWhole: the ADV resolved and the window it was measured over
	// is MISSING BUCKETS. THE SECOND REASON HERE THAT ACCOMPANIES ok=true (#591).
	//
	// ADV is total / days-TOUCHED, and a day holding one bar counts in that
	// denominator exactly as a day holding all 1440. So a partially covered day
	// drops volume from the numerator without dropping the day from the
	// denominator, and the ADV served is a FLOOR. Through DaysToLiquidate that
	// LENGTHENS the reported horizon — the conservative direction, which is
	// exactly why it needs a counter: nothing downstream looks wrong.
	//
	// A DAY MISSING ENTIRELY CARRIES NO DIRECTION — it leaves the numerator and
	// the denominator together, so the mean is simply taken over the days that
	// were seen. This reason does not tell the two cases apart, and its count is
	// not a size of error.
	//
	// ON THE 1m SERIES IT FIRES ON EVERY RESOLUTION, and that is the true
	// statement rather than a defect in the counter: bars/fold.go emits NO BAR for
	// a minute in which nothing traded, so a 1-minute window is never PROVABLY
	// whole. Its value is against a coarse series — under CoarsestFirst with a
	// populated 1d rollup it is silent, and the rate at which it starts firing is
	// how a rollup losing days becomes visible before the horizon it feeds moves.
	//
	// SILENCE IS NOT A HEALTH SIGNAL. It means "nothing is missing from the
	// series", never "the feed was up": store/gaps.go carries that ruling, and the
	// INGESTION-COVERAGE RECORD it names — which states which intervals were
	// actually observed — is what would make the second half provable. This
	// package cannot supply it and must not imply it.
	ReasonWindowNotWhole = "window_not_whole"
	// ReasonNoBars: the store holds no bars for this instrument, at this venue,
	// at the LAST resolution in the preference, within the window and known by
	// asOf. TERMINAL — the instrument is refused. If this fires for EVERY
	// instrument, suspect the venue first, then whether the base series is being
	// ingested at all.
	ReasonNoBars = "no_bars"
	// ReasonCoarseSeriesEmpty: a PREFERRED (coarser) series held no bars in the
	// window and the next resolution was tried. NOT a refusal — the call
	// continues, and this observation is followed by whatever the finer series
	// resolved to. It is reported because the alternative is a provider that
	// silently reads 40,000 rows per instrument while a rollup nobody noticed is
	// missing: "the rollup job is not running" and "the rollup job is running"
	// must not look the same. Expect one per instrument per call until the
	// coarse series is populated.
	ReasonCoarseSeriesEmpty = "coarse_series_empty"
	// ReasonCoarseSeriesShort: a PREFERRED (coarser) series held bars but touched
	// fewer than MinDays days, so the next resolution was tried. NOT a refusal.
	// THIS IS THE HALF-BUILT-ROLLUP CASE and it is the reason the fallback cannot
	// key on emptiness alone: a rollup backfilling from today
	// forward has three days where the base series has twenty-eight, and honouring
	// it would refuse the instrument with ReasonInsufficientHistory — a
	// perfectly-liquid-looking zero horizon caused entirely by a job that is
	// working correctly and is not finished. The day count is passed to the
	// observer, so "how far has the rollup backfilled" is readable from the
	// metric.
	ReasonCoarseSeriesShort = "coarse_series_short"
	// ReasonInsufficientHistory: bars exist but touch fewer than MinDays days. The
	// day count is passed to the observer. See DefaultMinDays for why the floor is
	// on days TOUCHED and what that does not say about coverage within them.
	ReasonInsufficientHistory = "insufficient_history"
	// ReasonUnusableVolume: a bar exists and its volume cannot be used — nil,
	// negative, or non-finite after conversion. A stored-data defect rather than
	// a gap, and the one reason here that says something is WRONG rather than
	// MISSING. All-or-nothing: one bad bar refuses the instrument rather than
	// shortening the window, because a window with holes reports a mean over
	// fewer days than it claims.
	ReasonUnusableVolume = "unusable_volume"
	// ReasonStoreError: the read itself failed. Still ok=false — the seam has no
	// error channel.
	ReasonStoreError = "store_error"
)

// Config parameterises which series the ADV is measured from.
type Config struct {
	// Venue is the MIC whose candles to read. REQUIRED, and there is no "any"
	// value on purpose — the same refusal indicator.NewSource makes, for a
	// sharper reason here.
	//
	// ADV SUMMED ACROSS VENUES IS A DIFFERENT NUMBER, and it is the number that
	// flatters. liquidity.Model.DaysToLiquidate divides size by
	// participation x ADV, so summing three venues' volume asserts the desk can
	// work the order on all three simultaneously at the participation rate, and
	// reports a horizon three times shorter than any single book supports. One
	// venue understates available liquidity instead — the conservative direction
	// for a risk measure, and the honest one when the platform has no composite
	// series and no smart-order-router to justify one.
	//
	// A desk that genuinely trades several venues configures one provider per
	// venue and reconciles above this layer, where the routing assumption is
	// visible.
	Venue string

	// Resolution PINS one series. Zero ⇒ 1m, the base series. Setting this and
	// ResolutionPreference together is refused by FromBars — they are two
	// spellings of one decision, and a config that holds both has not made it.
	Resolution store.Resolution

	// ResolutionPreference is an ordered, COARSEST-FIRST list of series to try;
	// the provider uses the first that can actually produce an ADV. Empty ⇒ the
	// single pinned Resolution. CoarsestFirst is the intended production value.
	//
	// # Why a preference and not just a coarser pin
	//
	// A pin onto 1d is a bet that the rollup exists, is populated, and has
	// backfilled the whole window. Lose that bet and every instrument is refused,
	// liquidity.Profile drops every position, and LiquidationHorizon reports
	// 0.0000 days — an ACTIVE claim of perfect liquidity for a book nobody
	// measured. There is no downstream symptom: the measure is served, it is in
	// range, and it is the most flattering value it can take.
	//
	// A preference degrades the other way. A missing or half-built coarse series
	// costs one extra index seek that returns nothing, and the ADV comes from the
	// base series exactly as it does today — the same number (see the package
	// doc: the statistic is invariant under resolution), at the old read cost,
	// with ReasonCoarseSeriesEmpty / ReasonCoarseSeriesShort counting how often.
	// The optimisation is taken when it is real and skipped when it is not, and
	// which of those happened is a metric rather than an inference.
	//
	// # Why the order is validated rather than trusted
	//
	// The list must be strictly coarsest-first (each entry's interval longer than
	// the next). A list written fine-first is not a mistake this can absorb: 1m
	// always resolves, so it would be chosen every time and the preference would
	// be dead configuration that reads as live. FromBars refuses it.
	ResolutionPreference []store.Resolution

	// Window is how much history to read back from the last complete day.
	// Zero ⇒ DefaultWindow.
	Window time.Duration
}

// CoarsestFirst is the intended production ResolutionPreference: the daily
// rollup if it is there, the hourly if it is not, and the 1-minute base series
// as the floor that is always written.
//
// A FUNCTION RATHER THAN A PACKAGE VAR, so a caller cannot mutate the default
// for every other caller in the process — a slice-valued exported var is a
// shared mutable global wearing a constant's name.
func CoarsestFirst() []store.Resolution {
	return []store.Resolution{store.Resolution1d, store.Resolution1h, store.Resolution1m}
}

// Provider resolves liquidity from the durable bar series. Construct with
// FromBars.
//
// # The missing case is a counter, not an error, and that is forced
//
// liquidity.Provider returns (LiquiditySpec, bool) with no error channel, so
// every failure below collapses to the same false, and liquidity.Profile then
// drops the position entirely — it is neither counted liquid nor counted in
// IlliquidNotional. Widening the seam would touch every consumer and is a change
// worth taking on its own evidence rather than smuggled in with the first
// implementation (the ruling termsource and spotsource both made). So the
// resolution is made OBSERVABLE instead: WithObserver fires on every refusal AND
// on the one degradation that succeeds.
type Provider struct {
	bars  BarStore
	venue string
	// resolutions is the coarsest-first search order, never empty — a pinned
	// Config.Resolution is the one-element case, so there is ONE read path rather
	// than a pinned branch and a preference branch that can drift apart.
	resolutions []store.Resolution
	window      time.Duration
	minDays     int

	assumedSpreads map[string]float64
	noSpread       bool

	onResolution func(instrumentID, reason string, days int)
}

// Compile-time assertion that this satisfies the seam it exists for. Without it
// a signature drift in liquidity.Provider would be discovered at the composition
// root rather than here.
var _ liquidity.Provider = (*Provider)(nil)

// Option customizes a Provider.
type Option func(*Provider)

// WithAssumedSpreads supplies per-instrument spreads as a FRACTION OF PRICE
// (0.0005 = 5bps), keyed by canonical instrument id.
//
// THESE ARE DESK ASSUMPTIONS AND THE NAME SAYS SO. Nothing in this repository
// measures a spread (see the package doc), so a number arriving here came from a
// human, and the only way that stays honest is for the API never to pretend
// otherwise. An instrument absent from the map is REFUSED unless WithNoSpread is
// also passed — a desk that declared it supplies spreads has a gap in its table,
// not a name that trades at zero cost.
//
// The map is copied, so a caller mutating it afterwards cannot change what a
// live risk engine assumes mid-run. Values outside [0, MaxAssumedSpread) are
// refused by FromBars.
func WithAssumedSpreads(m map[string]float64) Option {
	return func(p *Provider) {
		p.assumedSpreads = make(map[string]float64, len(m))
		for k, v := range m {
			p.assumedSpreads[k] = v
		}
	}
}

// WithNoSpread serves a spread of ZERO for any instrument no assumption covers —
// an ADV-only spec.
//
// WHAT IT COSTS, SPELLED BECAUSE IT MUST BE CHOSEN RATHER THAN DEFAULTED:
// Model.CostFraction returns 0 for a non-positive spread, so LiquidationCost is
// exactly 0 and LVaR99 is exactly VaR99 for every position this covers. The
// engine then serves a liquidity-adjusted VaR that is definitionally never an
// adjustment. LiquidationHorizon, which reads only ADV, remains genuinely
// correct.
//
// It exists because the alternative is worse in the other direction — refusing
// every instrument makes LiquidationHorizon report zero days, which is an active
// claim of perfect liquidity rather than an absent one (see the package doc). It
// is opt-in so that the pair of measures a deployment serves is a decision
// somebody made, and every resolution it degrades fires the observer with
// ReasonSpreadUnavailable so the count exists.
func WithNoSpread() Option {
	return func(p *Provider) { p.noSpread = true }
}

// WithWindow overrides DefaultWindow. Non-positive is refused by FromBars rather
// than quietly meaning "everything": an unbounded window would make an ADV depend
// on how long the estate has been ingesting.
func WithWindow(d time.Duration) Option {
	return func(p *Provider) { p.window = d }
}

// WithMinDays overrides DefaultMinDays — the fewest days the series must TOUCH
// to produce an ADV. Non-positive is refused by FromBars: a floor of zero would let
// a single day's volume set a liquidation horizon, which is the failure
// DefaultMinDays exists to prevent.
func WithMinDays(n int) Option {
	return func(p *Provider) { p.minDays = n }
}

// WithObserver sets the hook invoked whenever a resolution is REFUSED or
// DEGRADED. reason is one of the Reason* constants; days is the number of days
// the series TOUCHED and is meaningful for ReasonInsufficientHistory,
// ReasonCoarseSeriesShort, ReasonSpreadUnavailable and ReasonWindowNotWhole, zero
// otherwise. IT IS NEVER A COVERAGE FIGURE — see DefaultMinDays.
//
// UNLIKE spotsource's observer, TWO REASONS HERE FIRE ON SUCCESS —
// ReasonSpreadUnavailable and ReasonWindowNotWhole — because those are the
// degradations with no downstream symptom at all: the measure is served, it is a
// plausible number, and it happens to equal VaR99 forever, or to be a floor over a
// window nobody can account for. Nil (the default) means the degradation is
// unobserved. Wire it at the composition root.
//
// A CALL CAN REPORT MORE THAN ONCE, INCLUDING WHEN IT SUCCEEDS. With one series
// configured a successful call reports up to twice — the spread posture and the
// window coverage are independent facts about the same answer. With
// Config.ResolutionPreference each series that could not answer reports
// ReasonCoarseSeriesEmpty or ReasonCoarseSeriesShort before the next is tried, on
// top of those. Read those two reasons as "series skipped", never as "instruments
// refused" — a counter that conflates them would show every instrument failing on
// the day the rollup is deployed, when in fact every instrument resolved.
func WithObserver(fn func(instrumentID, reason string, days int)) Option {
	return func(p *Provider) { p.onResolution = fn }
}

// FromBars returns a liquidity provider over the bar series.
//
// IT RETURNS AN ERROR RATHER THAN A WORKING-LOOKING PROVIDER. Every refusal
// below produces the same runtime symptom — every position skipped, forever,
// uniformly — which at the measure is indistinguishable from a portfolio that
// holds nothing. A misconfiguration must surface on the first event, and the
// first event here is startup.
//
// A SPREAD POSTURE MUST BE SPELLED: at least one of WithAssumedSpreads or
// WithNoSpread is required. There is deliberately no default, because the two
// postures degrade DIFFERENT measures (see the package doc) and a deployment
// that arrives at one by omission has not chosen which of its two liquidity
// numbers is the honest one.
func FromBars(bars BarStore, cfg Config, opts ...Option) (*Provider, error) {
	if bars == nil {
		return nil, errNilStore
	}
	p := &Provider{
		bars:    bars,
		venue:   cfg.Venue,
		window:  cfg.Window,
		minDays: DefaultMinDays,
	}
	for _, o := range opts {
		if o != nil {
			o(p)
		}
	}
	if p.venue == "" {
		return nil, errNoVenue
	}
	res, err := resolutionSearch(cfg)
	if err != nil {
		return nil, err
	}
	p.resolutions = res
	if p.window == 0 {
		p.window = DefaultWindow
	}
	if p.window <= 0 {
		return nil, errNonPositiveWindow
	}
	if p.minDays <= 0 {
		return nil, errNonPositiveMinDays
	}
	if len(p.assumedSpreads) == 0 && !p.noSpread {
		return nil, errNoSpreadPosture
	}
	for id, s := range p.assumedSpreads {
		if s < 0 || s >= MaxAssumedSpread || math.IsNaN(s) {
			return nil, fmt.Errorf("%w: %s=%v", errAssumedSpreadOutOfRange, id, s)
		}
	}
	return p, nil
}

// resolutionSearch reduces the two spellings of "which series" to the single
// coarsest-first order the read path walks, refusing anything that would make
// the preference dead configuration.
func resolutionSearch(cfg Config) ([]store.Resolution, error) {
	if cfg.Resolution != "" && len(cfg.ResolutionPreference) > 0 {
		return nil, errBothResolutionForms
	}
	if len(cfg.ResolutionPreference) == 0 {
		r := cfg.Resolution
		if r == "" {
			r = store.Resolution1m
		}
		if !r.Valid() {
			return nil, fmt.Errorf("%w: %q", errUnstoredResolution, r)
		}
		return []store.Resolution{r}, nil
	}
	out := make([]store.Resolution, 0, len(cfg.ResolutionPreference))
	prev := time.Duration(0)
	for i, r := range cfg.ResolutionPreference {
		iv, ok := r.Interval()
		if !ok {
			return nil, fmt.Errorf("%w: %q", errUnstoredResolution, r)
		}
		// STRICTLY DECREASING, so a duplicate is refused by the same test that
		// refuses a fine-first list. Both are dead configuration: whichever entry
		// comes first wins every time and the rest never runs.
		if i > 0 && iv >= prev {
			return nil, fmt.Errorf("%w: %v", errPreferenceNotCoarsestFirst, cfg.ResolutionPreference)
		}
		prev = iv
		out = append(out, r)
	}
	return out, nil
}

// Liquidity resolves the instrument's ADV and spread as of a point in time.
// ok=false means "this position contributes to no liquidity measure" — see the
// type doc for what that costs and how to observe it.
//
// THE WINDOW ENDS AT THE LAST COMPLETE UTC DAY BEFORE asOf, NOT AT asOf. That is
// the one place this diverges from indicator.Source, which bounds To and AsOf at
// the same instant, and the divergence is the point: a valuation at 14:30 would
// otherwise fold a half-finished day into the mean as if it were a whole one,
// and a half day's volume is not a day's volume. The bug it prevents is
// directional and invisible — every intraday risk run would report an ADV
// slightly low and a liquidation horizon slightly long, with nothing anywhere
// saying why.
//
// THE KNOWLEDGE AXIS IS STILL BOUND AT asOf, so a venue restating a candle after
// the valuation cannot leak backwards into it (store.BarQuery.AsOf).
func (p *Provider) Liquidity(ctx context.Context, instrumentID string, asOf time.Time) (liquidity.LiquiditySpec, bool) {
	if p == nil || p.bars == nil {
		return liquidity.LiquiditySpec{}, false
	}
	if asOf.IsZero() {
		// REFUSED BEFORE THE STORE IS TOUCHED. A zero asOf means "no knowledge
		// horizon" to store.BarQuery, so a restatement stamped after the valuation
		// would leak into the ADV — precisely what the bitemporal contract exists
		// to prevent. It also leaves the complete-day boundary undefined, since
		// there is no day to be before.
		p.report(instrumentID, ReasonNoAsOf, 0)
		return liquidity.LiquiditySpec{}, false
	}

	// The spread posture is settled BEFORE the read, so an instrument no posture
	// covers costs nothing to refuse — but it is REPORTED at the end, so one call
	// produces at most one observation rather than a refusal stacked on a
	// degradation.
	spread, assumed, spreadOK := p.spreadFor(instrumentID)
	if !spreadOK {
		p.report(instrumentID, ReasonNoAssumedSpread, 0)
		return liquidity.LiquiditySpec{}, false
	}

	dayEnd := startOfUTCDay(asOf)
	total, days, ok := p.advOverPreference(ctx, instrumentID, dayEnd, asOf)
	if !ok {
		return liquidity.LiquiditySpec{}, false
	}

	if !assumed {
		p.report(instrumentID, ReasonSpreadUnavailable, days)
	}
	return liquidity.LiquiditySpec{
		ADV:    total / float64(days),
		Spread: spread,
		// ParticipationRate IS DELIBERATELY LEFT ZERO, and unlike Spread that is
		// not a degradation. liquidity.Model.participation documents zero as "use
		// the model default", so the desk-level DefaultParticipationRate applies —
		// the field's own contract, not a gap in this provider. A per-instrument
		// override is a desk assumption with no measurement behind it, exactly like
		// a spread; if one is ever wanted it belongs in an explicit option beside
		// WithAssumedSpreads rather than invented here.
		ParticipationRate: 0,
	}, true
}

// advOverPreference walks the coarsest-first search order and returns the volume
// total and the TOUCHED-day count of the FIRST series that can produce an ADV.
//
// # The count is days touched, and the window's holes are counted beside it
//
// dailyTotals cannot tell a day holding one bar from a day holding all of them,
// and nothing in the bar series can (#591). So this reports the number it
// actually has — days touched — and fires ReasonWindowNotWhole when the window
// the ADV was measured over is missing buckets, rather than asserting a
// completeness it cannot check. store.WindowOf is the one implementation of that
// question in this estate, and a count written out again here would be a second
// answer to it — the shape that has already produced seventeen secret() helpers.
//
// THE COVERAGE IS COUNTED, NEVER GATED. On the 1m series no window is ever whole,
// so a gate would refuse every instrument and LiquidationHorizon would report
// 0.0000 days — an active claim of perfect liquidity, the failure the package doc
// rules on.
//
// # What falls through, and what does not
//
// FALLS THROUGH: no bars, and too few touched days. Both mean "this series
// cannot answer" — a rollup that does not exist, or one still backfilling — and
// in both cases the finer series holds the same tape and yields the same ADV.
// Honouring either would refuse the instrument, and a refused instrument is
// dropped by liquidity.Profile and reported as zero days to liquidate.
//
// DOES NOT FALL THROUGH: an unusable volume, and a store error. Both say
// something is WRONG rather than MISSING, and falling through would let a
// CORRUPT rollup be silently papered over by the base series — the job would
// keep emitting bad bars with every risk run looking healthy. The whole value of
// a rollup is that its output can be trusted, so a defect in it refuses the
// instrument exactly as a defect in the base series does, and is counted under
// the same reason.
//
// The LAST resolution reports the terminal reasons (ReasonNoBars /
// ReasonInsufficientHistory) rather than the fallback ones, so a one-element
// preference — the pinned default — observes exactly what it observed before
// this search existed.
func (p *Provider) advOverPreference(ctx context.Context, instrumentID string, dayEnd, asOf time.Time) (total float64, days int, ok bool) {
	last := len(p.resolutions) - 1
	// The window is the same for every series in the preference — only the
	// resolution changes — so it is named once and reused by the coverage count
	// below, where a second expression of the same bounds could drift from the one
	// the store was actually asked for.
	from, to := dayEnd.Add(-p.window), dayEnd
	for i, res := range p.resolutions {
		bars, err := p.bars.Bars(ctx, store.BarQuery{
			InstrumentID: instrumentID,
			Venue:        p.venue,
			Resolution:   res,
			From:         from,
			To:           to,
			AsOf:         asOf,
		})
		if err != nil {
			// A STORE ERROR ALSO RETURNS false, forced by the seam having no error
			// channel. It is not silent: a database that cannot be read fails the
			// risk engine's readiness probe long before it fails here. This counter
			// is what distinguishes it from an empty store, which the probe cannot
			// see.
			p.report(instrumentID, ReasonStoreError, 0)
			return 0, 0, false
		}
		if len(bars) == 0 {
			if i < last {
				p.report(instrumentID, ReasonCoarseSeriesEmpty, 0)
				continue
			}
			p.report(instrumentID, ReasonNoBars, 0)
			return 0, 0, false
		}
		total, days, ok = dailyTotals(bars)
		if !ok {
			p.report(instrumentID, ReasonUnusableVolume, 0)
			return 0, 0, false
		}
		if days < p.minDays {
			if i < last {
				p.report(instrumentID, ReasonCoarseSeriesShort, days)
				continue
			}
			p.report(instrumentID, ReasonInsufficientHistory, days)
			return 0, 0, false
		}
		// THE ANSWER CARRIES ITS COVERAGE (#591). The seam is (LiquiditySpec,
		// bool), so coverage cannot ride ON the value the way v1.InputCoverage
		// rides on a risk measure; the observer is what this package has, and the
		// Provider doc already rules that the missing case being a counter is
		// forced by that seam.
		//
		// ok=false from WindowOf, and a window holding no whole bucket at all, both
		// reach the same report: Whole() is VACUOUSLY true over zero buckets
		// (store.Window says so), so letting that silence stand would make a window
		// too short to contain one bar read as fully covered — the defect this is
		// closing, arriving by the back door.
		if cov, covOK := store.WindowOf(bars, from, to, res); !covOK || cov.Buckets == 0 || !cov.Whole() {
			p.report(instrumentID, ReasonWindowNotWhole, days)
		}
		return total, days, true
	}
	// UNREACHABLE: resolutionSearch guarantees a non-empty order and the final
	// iteration always returns. Kept as a refusal rather than a panic — a risk
	// engine that mis-wires this loses one instrument, it does not lose the pod.
	p.report(instrumentID, ReasonNoBars, 0)
	return 0, 0, false
}

// ServesSpread reports whether this provider can ever serve a NON-ZERO spread —
// true iff a desk entered at least one assumed spread. It satisfies
// compute.SpreadServing, which compute.RegisterLiquidityRisk consults to decide
// whether LVaR99 is a measure worth putting on the wire at all: under WithNoSpread
// every spread is zero, so CostFraction is zero, so LVaR99 equals VaR99 exactly
// on every book forever.
//
// THE POSTURE IS ANSWERED WHERE IT WAS SPELLED. FromBars already forces the
// deployment to choose WithAssumedSpreads or WithNoSpread; a second flag at the
// registration site would be the same decision made twice by two callers who can
// disagree, and the disagreement would be invisible because the degenerate LVaR99
// looks like a working one.
//
// The relationship is duck-typed on purpose — compute declares the interface at
// the consumer, so this package does not import the measure registry — and the
// connection is held by a compile-time assertion in compute's own tests, because
// a duck-typed seam that silently stops matching is how a method rename would
// quietly put the degenerate measure back on the wire.
func (p *Provider) ServesSpread() bool {
	return p != nil && len(p.assumedSpreads) > 0
}

// spreadFor resolves the instrument's spread under the configured posture.
// assumed reports whether a human entered this number; ok=false means no posture
// covers the instrument.
func (p *Provider) spreadFor(instrumentID string) (spread float64, assumed, ok bool) {
	if s, hit := p.assumedSpreads[instrumentID]; hit {
		return s, true, true
	}
	if p.noSpread {
		return 0, false, true
	}
	return 0, false, false
}

// dailyTotals folds the bar series into UTC-day volume totals and returns their
// sum and the number of days the series TOUCHED — days holding at least one bar.
//
// TOUCHED IS NOT COMPLETE, AND THIS FUNCTION CANNOT TELL THEM APART (#591). A day
// holding a single 1-minute bar counts here exactly as a day holding all 1440, so
// ADV = total/days is UNDERSTATED whenever a day is partially covered: the absent
// minutes leave the numerator and the day stays in the denominator. Through
// DaysToLiquidate that lengthens the reported liquidation horizon, so the error
// runs the CONSERVATIVE way — a wrong measure, not an unsafe one. Its caller
// counts the shortfall as ReasonWindowNotWhole rather than claiming it away.
//
// REFUSING PARTIAL DAYS WOULD BE THE UNSAFE REPAIR, not the fix. bars/fold.go
// emits NO BAR for a minute in which nothing traded, so on the 1m series no real
// day is ever whole; a completeness gate would refuse every instrument,
// liquidity.Profile would drop every position, and LiquidationHorizon would report
// 0.0000 days — an ACTIVE claim of perfect liquidity, which is the failure this
// package's doc rules on at length.
//
// THE DAY IS THE UNIT, AND THAT IS THE WHOLE POINT OF THIS FUNCTION. ADV is
// AVERAGE DAILY volume; the stored series is 1-minute bars. Averaging bar
// volumes directly yields a per-MINUTE mean — a number 1440 times too small,
// which through DaysToLiquidate becomes a liquidation horizon 1440 times too
// long. It would not look wrong: it is positive, finite, monotonic in size, and
// wrong by three orders of magnitude.
//
// A DAY BUCKET WITH ZERO VOLUME STILL COUNTS AS A DAY. store.Bar's TradeCount
// doc makes the ruling one field over: "an interval in which nothing traded is a
// real observation, not a gap". A quiet day is part of what an average daily
// volume means, and dropping it would report the ADV of the days the instrument
// happened to trade.
//
// ok=false on the first unusable volume — see ReasonUnusableVolume.
func dailyTotals(bars []store.Bar) (total float64, days int, ok bool) {
	seen := map[int64]bool{}
	for i := range bars {
		v, vok := decutil.Float64(bars[i].Volume)
		if !vok || v < 0 {
			return 0, 0, false
		}
		total += v
		day := startOfUTCDay(bars[i].BucketStart).Unix()
		if !seen[day] {
			seen[day] = true
			days++
		}
	}
	return total, days, true
}

// startOfUTCDay is the midnight-UTC boundary at or before t.
//
// CONSTRUCTED RATHER THAN TRUNCATED. time.Truncate rounds relative to the zero
// time and ignores location, so it happens to give midnight UTC today and would
// stop doing so the moment a caller wanted a local calendar. Building the date
// explicitly says which calendar the day boundary belongs to, which is the thing
// an ADV depends on.
func startOfUTCDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// report fires the resolution hook if the caller asked to hear about them.
func (p *Provider) report(instrumentID, reason string, days int) {
	if p.onResolution != nil {
		p.onResolution(instrumentID, reason, days)
	}
}

// Construction errors. Named so a composition root can assert on them and a test
// does not match on prose.
var (
	errNilStore                = constErr("liquiditysource: nil bar store — every position would go unmeasured")
	errNoVenue                 = constErr("liquiditysource: venue is required — ADV summed across venues asserts a simultaneous multi-venue unwind and reports a horizon shorter than any single book supports")
	errNoSpreadPosture         = constErr("liquiditysource: a spread posture must be spelled — pass WithAssumedSpreads (desk-entered spreads) or WithNoSpread (ADV-only, which makes LVaR99 equal VaR99 exactly); nothing in this repository measures a spread, so there is no default that is not an invention")
	errNonPositiveWindow       = constErr("liquiditysource: window must be positive — an unbounded window makes an ADV depend on how long the estate has been ingesting")
	errNonPositiveMinDays      = constErr("liquiditysource: min days must be positive — a floor of zero lets one day's volume set a liquidation horizon")
	errAssumedSpreadOutOfRange = constErr("liquiditysource: assumed spread is outside [0,1) — a spread is a fraction of price, and a value at or above 1 is a unit error CostFraction's cap would absorb silently")

	errUnstoredResolution = constErr("liquiditysource: not a stored resolution — this platform stores 1m, 1h and 1d (store.Resolution)")

	errBothResolutionForms = constErr("liquiditysource: set Config.Resolution or Config.ResolutionPreference, never both — they are two spellings of one decision and a config holding both has not made it")

	errPreferenceNotCoarsestFirst = constErr("liquiditysource: ResolutionPreference must be strictly coarsest-first — a fine-first or duplicated list is dead configuration, because the first entry that always resolves is chosen every time and the rest never runs")
)

type constErr string

func (e constErr) Error() string { return string(e) }
