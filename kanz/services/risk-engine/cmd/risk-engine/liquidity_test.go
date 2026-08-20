package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/liquiditysource"
	"github.com/eighred/kanz/services/risk-engine/internal/config"
)

// THE LIQUIDITY WIRING (#509). What is graded here is the COMPOSITION — which
// venue is read, in which series order, with which spread posture, and whether
// every refusal reaches a counter. internal/risk/liquiditysource owns the ADV
// arithmetic and tests it; internal/risk/compute owns the measures.
//
// This file exists because main.go is the one place in this service no test
// reaches, and it has twice shipped a crashing composition root under a green
// suite. Every claim the wiring makes is asserted against a real store here.

const (
	liqVenue      = "XBIN"
	liqOtherVenue = "XOKX"
	liqInstrument = "BTC-USDT"
)

// liqAsOf is a mid-afternoon valuation, deliberately not midnight: the provider
// ends its window at the last COMPLETE UTC day, and a test pinned to midnight
// could not tell that apart from a window ending at asOf.
var liqAsOf = time.Date(2026, 6, 27, 14, 30, 0, 0, time.UTC)

// recordingBars is a real store.Memory with every query written down, so the
// test can assert WHICH series the provider asked for rather than inferring it
// from a number that happens to come out right.
type recordingBars struct {
	inner *store.Memory

	mu      sync.Mutex
	queries []store.BarQuery
}

func newRecordingBars() *recordingBars { return &recordingBars{inner: store.NewMemory()} }

func (r *recordingBars) Bars(ctx context.Context, q store.BarQuery) ([]store.Bar, error) {
	r.mu.Lock()
	r.queries = append(r.queries, q)
	r.mu.Unlock()
	return r.inner.Bars(ctx, q)
}

func (r *recordingBars) seen() []store.BarQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.BarQuery(nil), r.queries...)
}

// putBars writes one bar per complete UTC day, for the n days ending the day
// before liqAsOf, each with the given volume — so an ADV of exactly `volume`
// resolves off n distinct days. A midnight bucket start is a valid bucket in all
// three series, which is what lets one helper populate any of them.
func (r *recordingBars) putBars(t *testing.T, venue string, res store.Resolution, n int, volume int64) {
	t.Helper()
	dayEnd := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC)
	var bars []store.Bar
	for i := 1; i <= n; i++ {
		bucket := dayEnd.AddDate(0, 0, -i)
		bars = append(bars, store.Bar{
			InstrumentID:  liqInstrument,
			Venue:         venue,
			Resolution:    res,
			BucketStart:   bucket,
			Open:          liqDec(100, 0),
			High:          liqDec(100, 0),
			Low:           liqDec(100, 0),
			Close:         liqDec(100, 0),
			Volume:        liqDec(volume, 0),
			KnowledgeTime: bucket.Add(24 * time.Hour),
		})
	}
	if err := r.inner.PutBars(context.Background(), bars); err != nil {
		t.Fatalf("put bars: %v", err)
	}
}

func liqDec(coefficient int64, exponent int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coefficient, Exponent: exponent}
}

func liqLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// liqBook is a single-name, base-currency book valued at liqAsOf. 100,000 units
// against an ADV of 1,000,000 at the 20% default participation rate is 0.5 days.
func liqBook() *domain.Portfolio {
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{
		InstrumentID: liqInstrument,
		Quantity:     liqDec(100_000, 0),
		MarketValue:  &commonpb.Money{Amount: liqDec(1_000_000, 0), CurrencyCode: "USD"},
		AsOf:         liqAsOf,
	})
	return p
}

func liqRegistered(r *compute.Registry, name v1.MeasureName) bool {
	for _, n := range r.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// counters reads one metric family into reason → value. A missing family yields
// nil, which the callers distinguish from an empty one.
func counters(t *testing.T, g prometheus.Gatherer, name string) map[string]float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		out := map[string]float64{}
		for _, m := range f.GetMetric() {
			out[labelValue(m, "reason")] = m.GetCounter().GetValue()
		}
		return out
	}
	return nil
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// evaluate runs just the liquidation horizon over the book, which is what drives
// the provider and therefore both observers.
func evaluate(t *testing.T, r *compute.Registry, p *domain.Portfolio) float64 {
	t.Helper()
	ms := compute.ComputeMeasures(p, r, []v1.MeasureName{compute.MeasureLiquidationHorizon})
	m, ok := ms.Lookup(compute.MeasureLiquidationHorizon)
	if !ok {
		t.Fatal("LiquidationHorizon is not served — the registration did not happen")
	}
	return float64(m.Value.GetCoefficient()) * math.Pow10(int(m.Value.GetExponent()))
}

// ===========================================================================
// The venue is required, and its absence is a decision rather than a default.
// ===========================================================================

// AN UNSET VENUE REGISTERS NOTHING, AND THAT IS NOT AN ERROR.
//
// There is no default venue and there must not be one: ADV summed across venues
// reports a liquidation horizon three times shorter than any single book
// supports. A deployment that named no venue did not ask for liquidity, so the
// engine starts without it.
func TestAnUnsetVenueRegistersNoLiquidityMeasure(t *testing.T) {
	reg := prometheus.NewRegistry()
	registry := compute.DefaultRegistry()
	if err := registerLiquidityRisk(context.Background(), config.Config{}, registry,
		newRecordingBars(), reg, liqLogger()); err != nil {
		t.Fatalf("an unset venue must not fail startup: %v", err)
	}
	if liqRegistered(registry, compute.MeasureLiquidationHorizon) {
		t.Error("LiquidationHorizon was registered with no venue configured — its ADV would be " +
			"read at whatever venue the provider defaulted to, and there is no honest default")
	}
}

// AND IT LEAVES NO COUNTERS BEHIND.
//
// A counter sitting at zero beside a measure that was never registered reads as
// "wired, and quiet" — which is precisely the state this family of metrics
// exists to distinguish from "not wired at all". When the family is dark,
// kanz_risk_measure_live{family="liquidity"} is the signal, and these series are
// absent.
func TestAnUnsetVenueLeavesNoLiquidityCountersReadingHealthy(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := registerLiquidityRisk(context.Background(), config.Config{}, compute.DefaultRegistry(),
		newRecordingBars(), reg, liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}
	for _, name := range []string{"kanz_risk_liquidity_skipped_total", "kanz_risk_liquidity_unresolved_total"} {
		if got := counters(t, reg, name); got != nil {
			t.Errorf("%s exists with no liquidity measure registered (%v) — a zeroed counter "+
				"beside an absent measure reads as wired-and-quiet", name, got)
		}
	}
}

// A NAMED VENUE WITH NO BAR STORE REFUSES TO START.
//
// This is the one liquidity misconfiguration that is unambiguous: somebody
// explicitly asked for a liquidity family and there is no series to measure it
// from. Degrading here would make that deployment indistinguishable from one
// that never wanted liquidity at all.
func TestANamedVenueWithNoBarStoreRefusesToStart(t *testing.T) {
	err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, compute.DefaultRegistry(), nil,
		prometheus.NewRegistry(), liqLogger())
	if err == nil {
		t.Fatal("a configured venue with no market-data store started cleanly — the operator " +
			"asked for liquidity and would have got a dark family with nothing saying so")
	}
	// AND THE REFUSAL NAMES THE CONFIGURATION, not the seam. liquiditysource
	// refuses a nil store too, so this property is doubly held — but its message
	// says "nil bar store", which sends the operator into a risk library rather
	// than to the two environment variables that disagree.
	if !strings.Contains(err.Error(), "RISK_ENGINE_LIQUIDITY_VENUE") ||
		!strings.Contains(err.Error(), "RISK_ENGINE_MARKETDATA_DATABASE_URL") {
		t.Errorf("startup refusal does not name both variables that disagree: %v", err)
	}
}

// ===========================================================================
// The spread posture, and the measure it deliberately withholds.
// ===========================================================================

// ONE MEASURE IS REGISTERED, NOT TWO.
//
// Nothing in this repository persists a bid or an ask, so every spread this
// provider could serve is zero, so LVaR99 would equal VaR99 exactly on every
// book forever. compute declines to register it and reports the omission once.
func TestOnlyTheHorizonIsRegisteredBecauseNothingHereServesASpread(t *testing.T) {
	reg := prometheus.NewRegistry()
	registry := compute.DefaultRegistry()
	if err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, registry, newRecordingBars(), reg, liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}
	if !liqRegistered(registry, compute.MeasureLiquidationHorizon) {
		t.Error("LiquidationHorizon is absent — it reads ADV only and is the half of liquidity " +
			"this platform can measure")
	}
	if liqRegistered(registry, compute.MeasureLVaR99) {
		t.Error("LVaR99 was registered on a deployment that serves no spread. It would equal " +
			"VaR99 exactly on every book forever — plausible, in range, tracking VaR perfectly, " +
			"and nothing downstream could tell")
	}
}

// THE OMITTED MEASURE IS COUNTED, because an ABSENT measure is otherwise
// silent: engine.filterMeasures drops unknown names, so a client asking for
// LVaR99 gets 200 without it.
func TestTheOmittedLVaRIsCountedAtRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, compute.DefaultRegistry(),
		newRecordingBars(), reg, liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}
	got := counters(t, reg, "kanz_risk_liquidity_skipped_total")
	if got[compute.SkipNoSpreadSource] != 1 {
		t.Errorf("kanz_risk_liquidity_skipped_total{reason=%q} = %v, want 1 — the measure-level "+
			"observer is not wired, so the missing LVaR99 reaches no signal at all",
			compute.SkipNoSpreadSource, got[compute.SkipNoSpreadSource])
	}
}

// ===========================================================================
// Both observers, and every reason seeded at zero.
// ===========================================================================

// EVERY REASON HAS A SERIES BEFORE IT FIRES.
//
// A counter that only appears on its first increment reads as no-data to an
// alert, so the alert cannot fire on the transition from none to some — the
// transition that matters. Both closed reason sets are asserted whole, so a
// reason added to either package without a series here is a red build.
func TestEveryLiquidityReasonHasASeriesAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, compute.DefaultRegistry(),
		newRecordingBars(), reg, liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}

	skipped := counters(t, reg, "kanz_risk_liquidity_skipped_total")
	for _, reason := range []string{
		compute.SkipNoSpreadSource, compute.SkipIlliquid,
		compute.SkipNoLiquidHorizon, compute.SkipNoLiquidationCost,
	} {
		if _, ok := skipped[reason]; !ok {
			t.Errorf("kanz_risk_liquidity_skipped_total has no series for reason %q — an alert on "+
				"the none-to-some transition cannot fire on a series that does not exist", reason)
		}
	}

	unresolved := counters(t, reg, "kanz_risk_liquidity_unresolved_total")
	for _, reason := range []string{
		liquiditysource.ReasonNoAsOf, liquiditysource.ReasonNoAssumedSpread,
		liquiditysource.ReasonSpreadUnavailable, liquiditysource.ReasonNoBars,
		liquiditysource.ReasonCoarseSeriesEmpty, liquiditysource.ReasonCoarseSeriesShort,
		liquiditysource.ReasonInsufficientHistory, liquiditysource.ReasonUnusableVolume,
		liquiditysource.ReasonStoreError, liquiditysource.ReasonWindowNotWhole,
	} {
		v, ok := unresolved[reason]
		if !ok {
			t.Errorf("kanz_risk_liquidity_unresolved_total has no series for reason %q", reason)
			continue
		}
		if v != 0 {
			t.Errorf("kanz_risk_liquidity_unresolved_total{reason=%q} = %v before any evaluation, want 0",
				reason, v)
		}
	}
}

// AN EMPTY STORE REFUSES EVERY INSTRUMENT, AND THE REFUSAL IS COUNTED.
//
// Upstream this is anonymous — liquidity.Profile simply drops the position — so
// LiquidationHorizon reports 0.0000 days, the MOST LIQUID answer the measure can
// give, for a book nobody measured. The provider-level counter is the only thing
// that separates that from a book that genuinely liquidates instantly.
func TestAnEmptySeriesIsCountedRatherThanReportedAsPerfectLiquidity(t *testing.T) {
	reg := prometheus.NewRegistry()
	registry := compute.DefaultRegistry()
	bars := newRecordingBars()
	if err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, registry, bars, reg, liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}

	if days := evaluate(t, registry, liqBook()); days != 0 {
		t.Fatalf("horizon over an empty store = %v, want 0 — the premise of this test", days)
	}

	unresolved := counters(t, reg, "kanz_risk_liquidity_unresolved_total")
	if unresolved[liquiditysource.ReasonNoBars] < 1 {
		t.Errorf("kanz_risk_liquidity_unresolved_total{reason=%q} = %v, want >= 1. The measure "+
			"reported 0.0000 days for a book it knew nothing about, and the provider-level "+
			"observer is what makes that distinguishable from perfect liquidity",
			liquiditysource.ReasonNoBars, unresolved[liquiditysource.ReasonNoBars])
	}

	skipped := counters(t, reg, "kanz_risk_liquidity_skipped_total")
	if skipped[compute.SkipNoLiquidHorizon] < 1 {
		t.Errorf("kanz_risk_liquidity_skipped_total{reason=%q} = %v, want >= 1 — the "+
			"measure-level observer is not wired, so a whole-book zero reaches no signal",
			compute.SkipNoLiquidHorizon, skipped[compute.SkipNoLiquidHorizon])
	}
}

// ===========================================================================
// Which series, and at which venue.
// ===========================================================================

// THE SEARCH IS COARSEST-FIRST, NOT A PIN ONTO 1d.
//
// A pin is a bet that the rollup exists and has backfilled the whole window.
// Lose it and every instrument is refused and LiquidationHorizon reports 0.0000
// days — an ACTIVE claim of perfect liquidity, with no downstream symptom. The
// preference degrades to the base series instead, for the same ADV.
func TestTheProviderTriesTheDailyRollupFirstAndFallsBackToTheBaseSeries(t *testing.T) {
	registry := compute.DefaultRegistry()
	bars := newRecordingBars()
	if err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, registry, bars,
		prometheus.NewRegistry(), liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}
	evaluate(t, registry, liqBook())

	var order []store.Resolution
	for _, q := range bars.seen() {
		order = append(order, q.Resolution)
	}
	want := []store.Resolution{store.Resolution1d, store.Resolution1h, store.Resolution1m}
	if len(order) != len(want) {
		t.Fatalf("provider issued %d reads (%v), want the 3 of the coarsest-first order %v.\n"+
			"One read means a PINNED resolution: onto 1d that is a bet the rollup is populated, "+
			"and losing it reports 0.0000 days for every book", len(order), order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("read order = %v, want %v — a fine-first order makes the preference dead "+
				"configuration, because 1m always resolves and is chosen every time", order, want)
		}
	}
}

// A FALLTHROUGH IS "SERIES SKIPPED", NOT "INSTRUMENT REFUSED", and the two are
// separate counters for that reason: the day the 1d rollup is deployed to an
// environment that has not backfilled it, every instrument still resolves.
func TestACoarseSeriesThatCannotAnswerIsCountedSeparatelyFromARefusal(t *testing.T) {
	reg := prometheus.NewRegistry()
	registry := compute.DefaultRegistry()
	bars := newRecordingBars()
	if err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, registry, bars, reg, liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}
	evaluate(t, registry, liqBook())

	unresolved := counters(t, reg, "kanz_risk_liquidity_unresolved_total")
	if unresolved[liquiditysource.ReasonCoarseSeriesEmpty] != 2 {
		t.Errorf("kanz_risk_liquidity_unresolved_total{reason=%q} = %v, want 2 (1d and 1h both "+
			"fell through to the base series) — without the provider observer the fallback is "+
			"invisible and a missing rollup reads exactly like a present one",
			liquiditysource.ReasonCoarseSeriesEmpty,
			unresolved[liquiditysource.ReasonCoarseSeriesEmpty])
	}
}

// THE CONFIGURED VENUE IS THE ONE READ, AND ONLY THAT ONE.
//
// A book of bars at another venue must not resolve: summing or substituting
// venues asserts a multi-venue unwind nothing here can perform.
func TestOnlyTheConfiguredVenueIsRead(t *testing.T) {
	registry := compute.DefaultRegistry()
	bars := newRecordingBars()
	bars.putBars(t, liqOtherVenue, store.Resolution1d, 7, 1_000_000)
	if err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, registry, bars,
		prometheus.NewRegistry(), liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}

	if days := evaluate(t, registry, liqBook()); days != 0 {
		t.Errorf("horizon = %v with bars at %s only and %s configured, want 0 — a provider that "+
			"read another venue's tape would report a horizon for a book it cannot trade there",
			days, liqOtherVenue, liqVenue)
	}
	for _, q := range bars.seen() {
		if q.Venue != liqVenue {
			t.Errorf("provider read venue %q, want %q", q.Venue, liqVenue)
		}
	}
}

// ===========================================================================
// End to end: a real store, a real ADV, a real horizon.
// ===========================================================================

// THE WHOLE CHAIN PRODUCES THE ARITHMETIC IT CLAIMS.
//
// Seven complete daily buckets of 1,000,000 give an ADV of 1,000,000; 100,000
// units at the 20% default participation rate is 0.5 days. If the day fold were
// wrong — averaging bars instead of days — the answer would be off by the number
// of buckets and still look like a plausible horizon.
func TestABookWithARealTapeGetsARealHorizon(t *testing.T) {
	reg := prometheus.NewRegistry()
	registry := compute.DefaultRegistry()
	bars := newRecordingBars()
	bars.putBars(t, liqVenue, store.Resolution1d, 7, 1_000_000)
	if err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, registry, bars, reg, liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}

	if days := evaluate(t, registry, liqBook()); math.Abs(days-0.5) > 1e-9 {
		t.Fatalf("LiquidationHorizon = %.4f days, want 0.5000", days)
	}

	// THE DAILY ROLLUP ANSWERED, so the base series was never read — the whole
	// point of the preference, and the reason a 28-day window costs 7 rows here
	// instead of ~40,000.
	if got := len(bars.seen()); got != 1 {
		t.Errorf("provider issued %d reads, want 1 — the 1d series answered, so nothing finer "+
			"should have been touched", got)
	}

	// AND THE SUCCESS IS STILL REPORTED, because it is a DEGRADED success: the
	// spread served was zero, which is the degradation with no downstream symptom.
	unresolved := counters(t, reg, "kanz_risk_liquidity_unresolved_total")
	if unresolved[liquiditysource.ReasonSpreadUnavailable] < 1 {
		t.Errorf("kanz_risk_liquidity_unresolved_total{reason=%q} = %v, want >= 1 — this is the "+
			"one reason that accompanies a SUCCESS, and losing it makes an ADV-only spec "+
			"indistinguishable from a fully measured one",
			liquiditysource.ReasonSpreadUnavailable,
			unresolved[liquiditysource.ReasonSpreadUnavailable])
	}
}

// A HALF-BACKFILLED ROLLUP MUST NOT REFUSE THE INSTRUMENT.
//
// This is the case a pinned resolution gets catastrophically wrong. A 1d rollup
// that has backfilled three days where the base series has twenty-eight is a job
// working correctly and not yet finished; honouring it would refuse every
// instrument and report 0.0000 days — perfect liquidity, caused entirely by a
// healthy job. The preference falls through to the base series, the ADV is the
// same number, and the shortfall is a counter rather than an outage.
func TestAHalfBackfilledRollupFallsThroughInsteadOfRefusingTheBook(t *testing.T) {
	reg := prometheus.NewRegistry()
	registry := compute.DefaultRegistry()
	bars := newRecordingBars()
	bars.putBars(t, liqVenue, store.Resolution1d, liquiditysource.DefaultMinDays-1, 1_000_000)
	bars.putBars(t, liqVenue, store.Resolution1m, 7, 1_000_000)
	if err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, registry, bars, reg, liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}

	if days := evaluate(t, registry, liqBook()); math.Abs(days-0.5) > 1e-9 {
		t.Fatalf("LiquidationHorizon = %.4f days, want 0.5000 off the base series. A short "+
			"coarse series must fall through: refusing on it reports perfect liquidity for a "+
			"book the 1m tape covers fully", days)
	}
	unresolved := counters(t, reg, "kanz_risk_liquidity_unresolved_total")
	if unresolved[liquiditysource.ReasonCoarseSeriesShort] != 1 {
		t.Errorf("kanz_risk_liquidity_unresolved_total{reason=%q} = %v, want 1 — how far the "+
			"rollup has backfilled is readable only from this counter",
			liquiditysource.ReasonCoarseSeriesShort,
			unresolved[liquiditysource.ReasonCoarseSeriesShort])
	}
}

// AN INSTRUMENT WITH TOO LITTLE HISTORY IS REFUSED AND COUNTED, rather than
// given an ADV measured off four days. The horizon is LINEAR in ADV, and a noisy
// one does not look provisional — it is a finite number of days, in range,
// indistinguishable at the measure from an ADV averaged over a month.
func TestANewListingIsRefusedRatherThanMisSized(t *testing.T) {
	reg := prometheus.NewRegistry()
	registry := compute.DefaultRegistry()
	bars := newRecordingBars()
	// THE BASE SERIES IS THE SHORT ONE, so the search reaches its last resolution
	// and reports the TERMINAL reason. A short COARSE series is a fallthrough, not
	// a refusal — that is the test above, and conflating the two is exactly what
	// the two reason constants exist to prevent.
	bars.putBars(t, liqVenue, store.Resolution1m, liquiditysource.DefaultMinDays-1, 1_000_000)
	if err := registerLiquidityRisk(context.Background(),
		config.Config{LiquidityVenue: liqVenue}, registry, bars, reg, liqLogger()); err != nil {
		t.Fatalf("registerLiquidityRisk: %v", err)
	}

	if days := evaluate(t, registry, liqBook()); days != 0 {
		t.Errorf("horizon = %v off fewer than %d complete days, want the instrument refused",
			days, liquiditysource.DefaultMinDays)
	}
	unresolved := counters(t, reg, "kanz_risk_liquidity_unresolved_total")
	if unresolved[liquiditysource.ReasonInsufficientHistory] != 1 {
		t.Errorf("kanz_risk_liquidity_unresolved_total{reason=%q} = %v, want 1 — a refused "+
			"new listing is absent from the weighted horizon entirely, and only this counter says so",
			liquiditysource.ReasonInsufficientHistory,
			unresolved[liquiditysource.ReasonInsufficientHistory])
	}
}

// ===========================================================================
// The call site itself.
// ===========================================================================

// runEngine MUST CALL THIS, AND NOTHING ELSE IN THE ESTATE CHECKS THAT.
//
// Everything above grades registerLiquidityRisk in isolation, which is the half
// a unit test can reach — and a helper the composition root never calls is a
// measure the platform does not serve, tested to four decimal places.
//
// test/arch/no_dark_measure_seam_test.go cannot see it. It resolves callers of
// compute.RegisterLiquidityRisk, and liquidity.go is one; deleting the line in
// main.go leaves that guard GREEN with the family dark. That is the weakness its
// own doc names — "a seam called only by ANOTHER dark seam counts as live here" —
// and it is real: it was confirmed by deleting the call and watching the guard
// pass.
//
// PARSED, NOT GREPPED. A regexp over main.go would match this file's own comments
// and the log lines beside the call — the failure mode that has already produced
// three green guards in this repository checking nothing. A CallExpr cannot be a
// comment.
func TestRunEngineCallsTheLiquidityWiring(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var runEngineDecl *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "runEngine" {
			runEngineDecl = fn
		}
	}
	// NON-VACUITY: a renamed or moved runEngine would leave this guard inspecting
	// nothing and passing, which is the failure it exists to prevent one level up.
	if runEngineDecl == nil {
		t.Fatal("runEngine was not found in main.go — this guard is asserting nothing")
	}

	called := false
	ast.Inspect(runEngineDecl, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "registerLiquidityRisk" {
			called = true
		}
		return true
	})
	if !called {
		t.Error("runEngine does not call registerLiquidityRisk. Every test above still passes: " +
			"the wiring is correct and unreachable, LiquidationHorizon is absent from the registry, " +
			"and engine.filterMeasures drops it from a 200 response so a client cannot tell it from " +
			"a book with nothing to liquidate (#509)")
	}
}
