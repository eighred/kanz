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
// reads them point-in-time. Summing volume into complete UTC days and averaging
// is a real measurement of a real tape.
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
// shape CLAUDE.md forbids: "nothing configured" and "checked, and fine" reading
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
// # THE READ COST, STATED BECAUSE IT GATES WIRING
//
// The only bars this estate writes are 1-minute (marketdata.TranslateBar refuses
// a coarse bar at the ingest seam and NO ROLLUP JOB EXISTS — grep for a writer of
// Resolution1d and there is none, so the 1d and 1h series are empty in
// production today). At DefaultWindow that is 28 x 1440 = 40,320 rows per
// instrument per call, and compute calls this TWICE per position
// (LiquidationProfile and LiquidationCost each resolve the whole book). A
// 500-name portfolio is therefore ~40M rows per risk request. That is the
// strongest argument against wiring RegisterLiquidityRisk today, and it is
// recorded here rather than discovered in production. It is not fixed by a cache
// inside this type: an exact memo would have to key on asOf to stay
// point-in-time honest, which is a second design with its own staleness failure
// mode, and it belongs in its own change with its own evidence.
package liquiditysource

import (
	"context"
	"fmt"
	"math"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

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

	// DefaultMinDays is the fewest COMPLETE days that may produce an ADV.
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
	// ReasonNoBars: the store holds no bars for this instrument, at this venue,
	// at this resolution, within the window and known by asOf. If this fires for
	// EVERY instrument, suspect the venue or the resolution first — the estate
	// writes only 1-minute bars and has no rollup job, so a provider configured
	// for Resolution1d matches zero rows for every instrument that will ever
	// exist.
	ReasonNoBars = "no_bars"
	// ReasonInsufficientHistory: bars exist but cover fewer than MinDays complete
	// days. The day count is passed to the observer. See DefaultMinDays.
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

	// Resolution defaults to 1m, the base series. READ THE WARNING ON
	// ReasonNoBars before setting it to anything else: the coarser two are
	// rollups and no job in this repository produces them.
	Resolution store.Resolution

	// Window is how much history to read back from the last complete day.
	// Zero ⇒ DefaultWindow.
	Window time.Duration
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
	bars       BarStore
	venue      string
	resolution store.Resolution
	window     time.Duration
	minDays    int

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

// WithMinDays overrides DefaultMinDays — the fewest complete days that may
// produce an ADV. Non-positive is refused by FromBars: a floor of zero would let
// a single day's volume set a liquidation horizon, which is the failure
// DefaultMinDays exists to prevent.
func WithMinDays(n int) Option {
	return func(p *Provider) { p.minDays = n }
}

// WithObserver sets the hook invoked whenever a resolution is REFUSED or
// DEGRADED. reason is one of the Reason* constants; days is the count of
// complete days found and is meaningful only for ReasonInsufficientHistory, zero
// otherwise.
//
// UNLIKE spotsource's observer, ONE REASON HERE FIRES ON SUCCESS —
// ReasonSpreadUnavailable — because that is the degradation with no downstream
// symptom at all: the measure is served, it is a plausible number, and it
// happens to equal VaR99 forever. Nil (the default) means the degradation is
// unobserved. Wire it at the composition root.
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
		bars:       bars,
		venue:      cfg.Venue,
		resolution: cfg.Resolution,
		window:     cfg.Window,
		minDays:    DefaultMinDays,
	}
	for _, o := range opts {
		if o != nil {
			o(p)
		}
	}
	if p.venue == "" {
		return nil, errNoVenue
	}
	if p.resolution == "" {
		p.resolution = store.Resolution1m
	}
	if !p.resolution.Valid() {
		return nil, fmt.Errorf("liquiditysource: %q is not a stored resolution", p.resolution)
	}
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
	bars, err := p.bars.Bars(ctx, store.BarQuery{
		InstrumentID: instrumentID,
		Venue:        p.venue,
		Resolution:   p.resolution,
		From:         dayEnd.Add(-p.window),
		To:           dayEnd,
		AsOf:         asOf,
	})
	if err != nil {
		// A STORE ERROR ALSO RETURNS false, forced by the seam having no error
		// channel. It is not silent: a database that cannot be read fails the
		// risk engine's readiness probe long before it fails here. This counter is
		// what distinguishes it from an empty store, which the probe cannot see.
		p.report(instrumentID, ReasonStoreError, 0)
		return liquidity.LiquiditySpec{}, false
	}
	if len(bars) == 0 {
		p.report(instrumentID, ReasonNoBars, 0)
		return liquidity.LiquiditySpec{}, false
	}

	total, days, ok := dailyTotals(bars)
	if !ok {
		p.report(instrumentID, ReasonUnusableVolume, 0)
		return liquidity.LiquiditySpec{}, false
	}
	if days < p.minDays {
		p.report(instrumentID, ReasonInsufficientHistory, days)
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

// dailyTotals folds the bar series into complete-UTC-day volume totals and
// returns their sum and count.
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
		v, vok := decimalToFloat(bars[i].Volume)
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

// decimalToFloat converts an exact common.v1.Decimal to the float the liquidity
// analytics work in. ok is false for a nil decimal or a value that does not
// survive the conversion finitely, so an absent volume is refused rather than
// read as 0.
//
// THE DECIMAL BOUNDARY IS HERE, matching spotsource.decimalToFloat and
// indicator.decimalFloat: volumes stay exact common.v1.Decimal in the store, and
// the lossy conversion is confined to the analytics plane where liquidity.Model
// takes float64 by contract.
func decimalToFloat(d *commonpb.Decimal) (float64, bool) {
	if d == nil {
		return 0, false
	}
	f := float64(d.GetCoefficient()) * math.Pow10(int(d.GetExponent()))
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
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
)

type constErr string

func (e constErr) Error() string { return string(e) }
