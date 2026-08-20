// THE PRODUCTION liquidity.Provider (#509).
//
// UNGATED — no TEST_POSTGRES_URL, no database, no skip. The provider depends on
// BarStore, which is one method, so the failure paths are proven against a
// four-field fake and the two properties that must not be taken on trust — the
// bitemporal knowledge horizon and the complete-day boundary — are proven
// against store.Memory, the real in-memory implementation of the MODEL-01b
// contract, restatements and all. A gated test that skipped silently on every
// developer machine would leave this exactly as unproven as it was before it
// existed.
package liquiditysource

import (
	"context"
	"errors"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/liquidity"
)

const (
	instr = "AAPL"
	venue = "XNAS"
)

// day0 is a Monday, so a five-day window is a full Mon-Fri — the span
// DefaultMinDays is sized for.
var day0 = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

// fakeBars answers one canned series and records the query it was asked.
type fakeBars struct {
	bars []store.Bar
	err  error

	calls int
	got   store.BarQuery
}

func (f *fakeBars) Bars(_ context.Context, q store.BarQuery) ([]store.Bar, error) {
	f.calls++
	f.got = q
	return f.bars, f.err
}

func dec(coefficient int64, exponent int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coefficient, Exponent: exponent}
}

// testBar is a valid candle — store.Memory validates on Put, so a shortcut here
// would prove a store the database does not implement.
func testBar(bucket time.Time, volume int64, known time.Time) store.Bar {
	return store.Bar{
		InstrumentID:  instr,
		Venue:         venue,
		Resolution:    store.Resolution1m,
		BucketStart:   bucket,
		Open:          dec(100, 0),
		High:          dec(100, 0),
		Low:           dec(100, 0),
		Close:         dec(100, 0),
		Volume:        dec(volume, 0),
		KnowledgeTime: known,
	}
}

// oneBarPerDay builds n consecutive days holding a single bar each, volume[i]
// for day i. Enough for every property that is not about the minute grid.
func oneBarPerDay(volumes ...int64) []store.Bar {
	out := make([]store.Bar, 0, len(volumes))
	for i, v := range volumes {
		b := day0.AddDate(0, 0, i)
		out = append(out, testBar(b, v, b))
	}
	return out
}

// fullDay builds a complete 1440-bar minute grid for one day, every bar carrying
// perBar volume.
func fullDay(d time.Time, perBar int64) []store.Bar {
	out := make([]store.Bar, 0, 1440)
	for i := 0; i < 1440; i++ {
		bucket := d.Add(time.Duration(i) * time.Minute)
		out = append(out, testBar(bucket, perBar, bucket))
	}
	return out
}

func mustProvider(t *testing.T, bars BarStore, opts ...Option) *Provider {
	t.Helper()
	p, err := FromBars(bars, Config{Venue: venue}, opts...)
	if err != nil {
		t.Fatalf("FromBars: %v", err)
	}
	return p
}

// recorder captures resolution reports so a test can assert the degradation was
// made visible, not merely that it happened.
type recorder struct {
	instr  []string
	reason []string
	days   []int
}

func (r *recorder) observe() Option {
	return WithObserver(func(id, reason string, days int) {
		r.instr = append(r.instr, id)
		r.reason = append(r.reason, reason)
		r.days = append(r.days, days)
	})
}

// exactly pins the WHOLE observation sequence, in order.
//
// A SUCCESSFUL RESOLUTION REPORTS TWICE SINCE #591. The seam is
// (LiquiditySpec, bool), so the window's coverage cannot ride on the value the
// way v1.InputCoverage rides on a risk measure — it comes out of the observer
// beside the spread posture. A helper that asserts one observation can no longer
// say what a caller sees on the success path.
func (r *recorder) exactly(t *testing.T, want ...string) {
	t.Helper()
	if len(r.reason) != len(want) {
		t.Fatalf("observations = %v, want %v", r.reason, want)
	}
	for i := range want {
		if r.reason[i] != want[i] {
			t.Fatalf("observations = %v, want %v", r.reason, want)
		}
	}
}

func (r *recorder) only(t *testing.T, wantReason string, wantDays int) {
	t.Helper()
	if len(r.reason) != 1 {
		t.Fatalf("want exactly one observation, got %v", r.reason)
	}
	if r.reason[0] != wantReason {
		t.Errorf("reason = %q, want %q", r.reason[0], wantReason)
	}
	if r.days[0] != wantDays {
		t.Errorf("days = %d, want %d", r.days[0], wantDays)
	}
	if r.instr[0] != instr {
		t.Errorf("instrument = %q, want %q", r.instr[0], instr)
	}
}

// ===== ADV is a DAILY mean, and the factor between the two is 1440 =====

// The single most consequential arithmetic in this package. The stored series is
// 1-minute bars; ADV is AVERAGE DAILY volume. Averaging bar volumes yields a
// per-minute mean — 1440x too small — and DaysToLiquidate divides by it, so the
// reported liquidation horizon would be 1440x too long while staying positive,
// finite and monotonic in size. Nothing downstream could tell.
func TestADVIsTheMeanDailyVolumeNotTheMeanBarVolume(t *testing.T) {
	// Five complete days of full minute grids. Four days at 5 per bar, one at 10,
	// so the assertion exercises the MEAN over days rather than a single value
	// that a sum-only bug would also reproduce.
	var bars []store.Bar
	for i, perBar := range []int64{5, 5, 5, 5, 10} {
		bars = append(bars, fullDay(day0.AddDate(0, 0, i), perBar)...)
	}
	f := &fakeBars{bars: bars}
	p := mustProvider(t, f, WithNoSpread())

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5))
	if !ok {
		t.Fatalf("Liquidity: not ok")
	}

	// Daily totals are 7200, 7200, 7200, 7200 and 14400; their mean is 8640.
	const wantADV = 8640.0
	if spec.ADV != wantADV {
		t.Errorf("ADV = %v, want %v", spec.ADV, wantADV)
	}
	// The mean BAR volume over the same 7200 bars is 6 — the number a
	// per-observation average would report, and exactly wantADV/1440.
	const meanBarVolume = 6.0
	if spec.ADV == meanBarVolume {
		t.Errorf("ADV = %v, which is the mean BAR volume — the daily fold is not happening", spec.ADV)
	}
	if wantADV != meanBarVolume*1440 {
		t.Fatalf("test arithmetic is wrong: %v != %v", wantADV, meanBarVolume*1440)
	}
}

// A day on which nothing traded is a real observation, not a gap — store.Bar's
// TradeCount doc makes that ruling one field over. Dropping it would report the
// ADV of the days the instrument happened to trade, which is higher, which
// shortens every liquidation horizon.
func TestAQuietDayCountsAsADayAndLowersTheADV(t *testing.T) {
	f := &fakeBars{bars: oneBarPerDay(10, 10, 0, 10, 10)}
	p := mustProvider(t, f, WithNoSpread())

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5))
	if !ok {
		t.Fatalf("Liquidity: not ok")
	}
	// 40 over FIVE days. Dropping the quiet day would give 40 over four = 10.
	if spec.ADV != 8 {
		t.Errorf("ADV = %v, want 8 — a zero-volume day must still count as a day", spec.ADV)
	}
}

// ===== Point-in-time, asserted in BOTH directions =====

// Proven against store.Memory rather than a fake, because the property under
// test is the STORE's knowledge collapse and a fake asserting it would be
// asserting itself.
func TestARestatementIsInvisibleUntilTheValuationTimePassesIt(t *testing.T) {
	mem := store.NewMemory()
	ctx := context.Background()

	original := oneBarPerDay(10, 10, 10)
	if err := mem.PutBars(ctx, original); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	// The venue restates day 0's candle with a hundredfold volume, and Kanz learns
	// it on 2026-03-10 — a new KnowledgeTime, never an overwrite.
	restated := testBar(day0, 1000, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err := mem.PutBars(ctx, []store.Bar{restated}); err != nil {
		t.Fatalf("PutBars restatement: %v", err)
	}

	p := mustProvider(t, mem, WithNoSpread(), WithMinDays(1))

	// BEFORE: the correction had not arrived, so it must not be visible.
	before, ok := p.Liquidity(ctx, instr, time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatalf("Liquidity before: not ok")
	}
	if before.ADV != 10 {
		t.Errorf("ADV as of 2026-03-05 = %v, want 10 — a correction stamped 2026-03-10 leaked "+
			"backwards into a valuation that could not have known it", before.ADV)
	}

	// AFTER: the same buckets, the same window, and now the correction IS the
	// bar. Asserting only the first direction would pass against a provider that
	// ignored the restatement entirely.
	after, ok := p.Liquidity(ctx, instr, time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatalf("Liquidity after: not ok")
	}
	if after.ADV != 340 {
		t.Errorf("ADV as of 2026-03-11 = %v, want 340 — the restatement is not being read at all, "+
			"so the point-in-time assertion above proves nothing", after.ADV)
	}
}

// The knowledge horizon is only half of it: the OBSERVATION window must stop at
// the last complete day, or an intraday valuation folds a half-finished day into
// the mean as if it were a whole one. Directional and invisible: every intraday
// run reports an ADV slightly low and a horizon slightly long.
func TestTodaysPartialDayIsExcludedFromTheADV(t *testing.T) {
	mem := store.NewMemory()
	ctx := context.Background()
	if err := mem.PutBars(ctx, oneBarPerDay(10, 10, 10)); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	// 2026-03-05 is in progress at the valuation time and has traded heavily.
	partial := day0.AddDate(0, 0, 3).Add(9 * time.Hour)
	if err := mem.PutBars(ctx, []store.Bar{testBar(partial, 100000, partial)}); err != nil {
		t.Fatalf("PutBars partial: %v", err)
	}

	p := mustProvider(t, mem, WithNoSpread(), WithMinDays(1))
	spec, ok := p.Liquidity(ctx, instr, day0.AddDate(0, 0, 3).Add(14*time.Hour))
	if !ok {
		t.Fatalf("Liquidity: not ok")
	}
	// Three complete days at 10. Including the partial day would give
	// 100030 over four days = 25007.5.
	if spec.ADV != 10 {
		t.Errorf("ADV = %v, want 10 — the in-progress day is being folded in as a whole day", spec.ADV)
	}
}

// The query itself, asserted structurally: To stops at the day boundary while
// AsOf still binds the knowledge axis at the valuation instant. Bounding one and
// not the other leaks, and leaks invisibly.
func TestTheQueryStopsAtTheDayBoundaryWhileKnowledgeStaysAtTheValuationTime(t *testing.T) {
	f := &fakeBars{bars: oneBarPerDay(1, 1, 1, 1, 1)}
	p := mustProvider(t, f, WithNoSpread())

	asOf := time.Date(2026, 3, 7, 14, 30, 0, 0, time.UTC)
	if _, ok := p.Liquidity(context.Background(), instr, asOf); !ok {
		t.Fatalf("Liquidity: not ok")
	}
	wantTo := time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC)
	if !f.got.To.Equal(wantTo) {
		t.Errorf("query To = %v, want %v (the last complete day)", f.got.To, wantTo)
	}
	if !f.got.AsOf.Equal(asOf) {
		t.Errorf("query AsOf = %v, want %v — the knowledge horizon must stay at the valuation "+
			"instant even though the observation window stops earlier", f.got.AsOf, asOf)
	}
	if !f.got.From.Equal(wantTo.Add(-DefaultWindow)) {
		t.Errorf("query From = %v, want %v", f.got.From, wantTo.Add(-DefaultWindow))
	}
	if f.got.Venue != venue || f.got.Resolution != store.Resolution1m {
		t.Errorf("query series = (%q, %q), want (%q, %q)", f.got.Venue, f.got.Resolution,
			venue, store.Resolution1m)
	}
}

// ===== Absent and partial history =====

func TestAnInstrumentWithNoBarsIsRefusedAndReported(t *testing.T) {
	var r recorder
	f := &fakeBars{}
	p := mustProvider(t, f, WithNoSpread(), r.observe())

	if _, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5)); ok {
		t.Fatalf("Liquidity: ok=true for an instrument with no bars")
	}
	if f.calls != 1 {
		t.Errorf("store calls = %d, want 1", f.calls)
	}
	r.only(t, ReasonNoBars, 0)
}

// The partial-window ruling, at its boundary in both directions. One day short
// is refused with the count reported, so a new listing is VISIBLY excluded; at
// the floor it resolves.
func TestAWindowOneDayShortOfTheFloorIsRefusedAndTheDayCountIsReported(t *testing.T) {
	var r recorder
	p := mustProvider(t, &fakeBars{bars: oneBarPerDay(10, 10, 10, 10)}, WithNoSpread(), r.observe())

	if _, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 4)); ok {
		t.Fatalf("Liquidity: ok=true on %d complete days, floor is %d", 4, DefaultMinDays)
	}
	r.only(t, ReasonInsufficientHistory, 4)
}

func TestAWindowExactlyAtTheFloorResolves(t *testing.T) {
	var r recorder
	p := mustProvider(t, &fakeBars{bars: oneBarPerDay(10, 10, 10, 10, 10)}, WithNoSpread(), r.observe())

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5))
	if !ok {
		t.Fatalf("Liquidity: ok=false on exactly %d complete days", DefaultMinDays)
	}
	if spec.ADV != 10 {
		t.Errorf("ADV = %v, want 10", spec.ADV)
	}
	// The floor is not off by one in the other direction either: five days
	// resolves. It reports twice — the 28-day window holds five 1-minute bars, so
	// its coverage is counted (#591) — and the day count on both is the divisor
	// the ADV was actually taken over.
	r.exactly(t, ReasonWindowNotWhole, ReasonSpreadUnavailable)
	if r.days[0] != 5 || r.days[1] != 5 {
		t.Errorf("days = %v, want [5 5] — the observer's count is the ADV's divisor", r.days)
	}
}

// All-or-nothing rather than best-effort: a window with holes reports a mean
// over fewer days than it claims, and nothing says so.
func TestAnUnusableVolumeRefusesTheInstrumentRatherThanShorteningTheWindow(t *testing.T) {
	var r recorder
	bars := oneBarPerDay(10, 10, 10, 10, 10)
	bars[2].Volume = nil
	p := mustProvider(t, &fakeBars{bars: bars}, WithNoSpread(), r.observe())

	if _, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5)); ok {
		t.Fatalf("Liquidity: ok=true with an unusable volume in the window")
	}
	r.only(t, ReasonUnusableVolume, 0)
}

func TestAStoreErrorIsRefusedRatherThanPanicking(t *testing.T) {
	var r recorder
	p := mustProvider(t, &fakeBars{err: errors.New("connection refused")}, WithNoSpread(), r.observe())

	if _, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5)); ok {
		t.Fatalf("Liquidity: ok=true on a store error")
	}
	r.only(t, ReasonStoreError, 0)
}

func TestAZeroValuationTimeIsRefusedWithoutReadingTheStore(t *testing.T) {
	var r recorder
	f := &fakeBars{bars: oneBarPerDay(10, 10, 10, 10, 10)}
	p := mustProvider(t, f, WithNoSpread(), r.observe())

	if _, ok := p.Liquidity(context.Background(), instr, time.Time{}); ok {
		t.Fatalf("Liquidity: ok=true for a zero valuation time")
	}
	if f.calls != 0 {
		t.Errorf("store calls = %d, want 0 — a zero asOf means no knowledge horizon and must be "+
			"refused before the read", f.calls)
	}
	r.only(t, ReasonNoAsOf, 0)
}

// ===== The spread decision, asserted explicitly =====

// Nothing in this repository measures a spread, so a provider that was not told
// which posture to take has not been configured — it has been left to default
// into one of two different lies. Refused at startup, where it is cheap.
func TestAProviderWithNoSpreadPostureIsRefusedAtConstruction(t *testing.T) {
	_, err := FromBars(&fakeBars{}, Config{Venue: venue})
	if !errors.Is(err, errNoSpreadPosture) {
		t.Fatalf("FromBars with no spread posture: err = %v, want errNoSpreadPosture", err)
	}
}

// A desk that entered spreads has declared it supplies them. A name missing from
// its own table is a gap in the table, not a name that trades at zero cost.
func TestAnInstrumentOutsideTheAssumedSpreadTableIsRefused(t *testing.T) {
	var r recorder
	f := &fakeBars{bars: oneBarPerDay(10, 10, 10, 10, 10)}
	p := mustProvider(t, f, WithAssumedSpreads(map[string]float64{"MSFT": 0.0005}), r.observe())

	if _, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5)); ok {
		t.Fatalf("Liquidity: ok=true for an instrument with no assumed spread")
	}
	if f.calls != 0 {
		t.Errorf("store calls = %d, want 0 — the posture settles before the read", f.calls)
	}
	r.only(t, ReasonNoAssumedSpread, 0)
}

// The assumed spread reaches the spec unchanged and is NOT reported as a
// degradation — this is the one path where the number came from somewhere.
func TestAnAssumedSpreadIsServedAndIsNotReportedAsDegraded(t *testing.T) {
	var r recorder
	p := mustProvider(t, &fakeBars{bars: oneBarPerDay(10, 10, 10, 10, 10)},
		WithAssumedSpreads(map[string]float64{instr: 0.0005}), r.observe())

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5))
	if !ok {
		t.Fatalf("Liquidity: not ok")
	}
	if spec.Spread != 0.0005 {
		t.Errorf("Spread = %v, want 0.0005", spec.Spread)
	}
	// AN ENTERED ASSUMPTION IS NOT A DEGRADATION, so no spread reason fires. The
	// window's coverage is an independent fact about the same answer and still
	// does: five 1-minute bars do not cover a 28-day window, and the ADV served is
	// a floor whichever posture supplied the spread (#591).
	r.exactly(t, ReasonWindowNotWhole)
	// And it is an assumption that BITES: the liquidation cost is non-zero, which
	// is the entire difference between LVaR99 and VaR99.
	days, liquid := liquidity.DefaultModel().DaysToLiquidate(100, spec)
	if !liquid {
		t.Fatalf("DaysToLiquidate: not liquid")
	}
	if cost := liquidity.DefaultModel().CostFraction(days, spec.Spread); cost <= 0 {
		t.Errorf("CostFraction = %v, want > 0 — an assumed spread that does not widen VaR is not "+
			"an assumption, it is decoration", cost)
	}
}

// THE DEGENERACY, ASSERTED RATHER THAN ARGUED. WithNoSpread is the posture that
// keeps LiquidationHorizon honest at the price of making LVaR99 equal VaR99
// exactly. Both halves of that sentence are checked here, because a reader who
// only saw the doc comment would have to take it on trust.
func TestTheNoSpreadPostureServesARealADVAndACostOfExactlyZero(t *testing.T) {
	var r recorder
	p := mustProvider(t, &fakeBars{bars: oneBarPerDay(10, 10, 10, 10, 10)}, WithNoSpread(), r.observe())

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5))
	if !ok {
		t.Fatalf("Liquidity: not ok")
	}
	if spec.ADV != 10 {
		t.Errorf("ADV = %v, want 10 — the measurable half must still be measured", spec.ADV)
	}
	if spec.Spread != 0 {
		t.Errorf("Spread = %v, want 0", spec.Spread)
	}

	m := liquidity.DefaultModel()
	days, liquid := m.DaysToLiquidate(100, spec)
	if !liquid {
		t.Fatalf("DaysToLiquidate: not liquid — the ADV-only horizon is the measure this posture exists to keep")
	}
	if days <= 0 {
		t.Errorf("days = %v, want > 0", days)
	}
	if cost := m.CostFraction(days, spec.Spread); cost != 0 {
		t.Errorf("CostFraction = %v, want exactly 0 — this is the LVaR99 == VaR99 degeneracy the "+
			"posture accepts, and if it ever stops holding the doc is wrong", cost)
	}

	// And the degeneracy is COUNTED, not inferred from a suspiciously round LVaR —
	// alongside the window coverage the ADV cannot carry itself (#591).
	r.exactly(t, ReasonWindowNotWhole, ReasonSpreadUnavailable)
}

// A spread is a fraction of price. A value at or above 1 is a unit error — basis
// points where a fraction was wanted — and CostFraction's cap at 1 would absorb
// it silently, charging every position its entire notional to liquidate.
func TestAnAssumedSpreadAtOrAboveOneIsRefusedAtConstruction(t *testing.T) {
	for _, bad := range []float64{-0.001, MaxAssumedSpread, 5.0} {
		_, err := FromBars(&fakeBars{}, Config{Venue: venue},
			WithAssumedSpreads(map[string]float64{instr: bad}))
		if !errors.Is(err, errAssumedSpreadOutOfRange) {
			t.Errorf("FromBars with spread %v: err = %v, want errAssumedSpreadOutOfRange", bad, err)
		}
	}
}

// ===== Construction =====

func TestAProviderWithNoVenueIsRefusedRatherThanBlendingBooks(t *testing.T) {
	_, err := FromBars(&fakeBars{}, Config{}, WithNoSpread())
	if !errors.Is(err, errNoVenue) {
		t.Fatalf("FromBars with no venue: err = %v, want errNoVenue", err)
	}
}

func TestAnUnboundedWindowOrAZeroFloorIsRefused(t *testing.T) {
	if _, err := FromBars(&fakeBars{}, Config{Venue: venue}, WithNoSpread(), WithWindow(-time.Hour)); !errors.Is(err, errNonPositiveWindow) {
		t.Errorf("negative window: err = %v, want errNonPositiveWindow", err)
	}
	if _, err := FromBars(&fakeBars{}, Config{Venue: venue}, WithNoSpread(), WithMinDays(0)); !errors.Is(err, errNonPositiveMinDays) {
		t.Errorf("zero min days: err = %v, want errNonPositiveMinDays", err)
	}
}

// The assumption table is copied, so a config reloader mutating its map cannot
// change what a live risk engine assumes mid-run.
func TestTheAssumedSpreadTableIsCopiedAtConstruction(t *testing.T) {
	m := map[string]float64{instr: 0.0005}
	p := mustProvider(t, &fakeBars{bars: oneBarPerDay(10, 10, 10, 10, 10)}, WithAssumedSpreads(m))
	m[instr] = 0.5

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5))
	if !ok {
		t.Fatalf("Liquidity: not ok")
	}
	if spec.Spread != 0.0005 {
		t.Errorf("Spread = %v, want 0.0005 — the caller's map is live inside the provider", spec.Spread)
	}
}

// ===== THE COARSEST SERIES THAT CAN ANSWER =====
//
// A 28-day window on the 1-minute series is ~40,000 rows per instrument, and
// compute walks the book TWICE per risk request. A daily rollup makes the same
// number out of 28 rows — the same number, not an approximation, because
// dailyTotals sums volume and counts distinct UTC days and both are invariant
// under the bar interval.
//
// The whole risk of taking that optimisation is reading a series that is EMPTY
// or HALF-BUILT: no bars refuses the instrument, liquidity.Profile drops it, and
// LiquidationHorizon reports 0.0000 days — an active claim of perfect liquidity
// for a book nobody measured. Every test below is about that failure and not
// about the saving.

// seriesBars answers a DIFFERENT canned series per resolution and records the
// order it was asked, so "the coarse one was preferred" and "the fine one was
// not read" are separate, checkable facts.
type seriesBars struct {
	series map[store.Resolution][]store.Bar
	errs   map[store.Resolution]error
	asked  []store.Resolution
}

func (s *seriesBars) Bars(_ context.Context, q store.BarQuery) ([]store.Bar, error) {
	s.asked = append(s.asked, q.Resolution)
	if err, hit := s.errs[q.Resolution]; hit {
		return nil, err
	}
	return s.series[q.Resolution], nil
}

// dailyBars builds n consecutive DAILY candles, volume[i] for day i — the shape
// a 1d rollup writes.
func dailyBars(volumes ...int64) []store.Bar {
	out := make([]store.Bar, 0, len(volumes))
	for i, v := range volumes {
		b := day0.AddDate(0, 0, i)
		bar := testBar(b, v, b)
		bar.Resolution = store.Resolution1d
		out = append(out, bar)
	}
	return out
}

func mustProviderCfg(t *testing.T, bars BarStore, cfg Config, opts ...Option) *Provider {
	t.Helper()
	cfg.Venue = venue
	p, err := FromBars(bars, cfg, opts...)
	if err != nil {
		t.Fatalf("FromBars: %v", err)
	}
	return p
}

func askedEqual(got []store.Resolution, want ...store.Resolution) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The saving, and the only test here that is about it. 5 rows read instead of
// 7200, and the finer series is never touched.
func TestThePopulatedDailySeriesIsUsedAndTheMinuteSeriesIsNotRead(t *testing.T) {
	var r recorder
	f := &seriesBars{series: map[store.Resolution][]store.Bar{
		store.Resolution1d: dailyBars(14400, 14400, 14400, 14400, 14400),
		store.Resolution1m: fullDay(day0, 10),
	}}
	p := mustProviderCfg(t, f, Config{ResolutionPreference: CoarsestFirst()}, WithNoSpread(), r.observe())

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5))
	if !ok {
		t.Fatalf("Liquidity: not ok")
	}
	if !askedEqual(f.asked, store.Resolution1d) {
		t.Errorf("series read = %v, want only [1d] — a preference that still reads the base "+
			"series has saved nothing", f.asked)
	}
	if spec.ADV != 14400 {
		t.Errorf("ADV = %v, want 14400", spec.ADV)
	}
	// Five daily candles in a 28-day window is still 23 days nobody can speak
	// for, and the saving does not buy silence about that.
	r.exactly(t, ReasonWindowNotWhole, ReasonSpreadUnavailable)
}

// THE PREMISE OF THE WHOLE OPTIMISATION, ASSERTED RATHER THAN ARGUED. If the ADV
// off a rollup differed from the ADV off the base series, this would be a
// different measure wearing the same name and every horizon would shift the day
// the rollup landed.
func TestTheSameADVComesOutOfTheDailyRollupAsOutOfTheMinuteSeries(t *testing.T) {
	// Five days of full minute grids: 1440 bars x 10 = 14400 per day.
	var minute []store.Bar
	for i := 0; i < 5; i++ {
		minute = append(minute, fullDay(day0.AddDate(0, 0, i), 10)...)
	}
	asOf := day0.AddDate(0, 0, 5)

	fine := mustProviderCfg(t, &seriesBars{series: map[store.Resolution][]store.Bar{
		store.Resolution1m: minute,
	}}, Config{Resolution: store.Resolution1m}, WithNoSpread())
	coarse := mustProviderCfg(t, &seriesBars{series: map[store.Resolution][]store.Bar{
		store.Resolution1d: dailyBars(14400, 14400, 14400, 14400, 14400),
	}}, Config{Resolution: store.Resolution1d}, WithNoSpread())

	fineSpec, ok := fine.Liquidity(context.Background(), instr, asOf)
	if !ok {
		t.Fatalf("1m: not ok")
	}
	coarseSpec, ok := coarse.Liquidity(context.Background(), instr, asOf)
	if !ok {
		t.Fatalf("1d: not ok")
	}
	if fineSpec.ADV != coarseSpec.ADV {
		t.Fatalf("ADV differs: 1m = %v, 1d = %v — the statistic is supposed to be invariant under the "+
			"series it is measured from, and a liquidation horizon would move when the rollup lands",
			fineSpec.ADV, coarseSpec.ADV)
	}
	if fineSpec.ADV != 14400 {
		t.Errorf("ADV = %v, want 14400", fineSpec.ADV)
	}
}

// The state of this estate on the day the preference is first configured: the
// rollup job is not running yet, so 1d and 1h hold nothing. The instrument must
// still resolve, off the base series, at the old cost.
func TestAnEmptyCoarseSeriesFallsBackToTheBaseSeriesAndIsReported(t *testing.T) {
	var r recorder
	f := &seriesBars{series: map[store.Resolution][]store.Bar{
		store.Resolution1m: oneBarPerDay(10, 10, 10, 10, 10),
	}}
	p := mustProviderCfg(t, f, Config{ResolutionPreference: CoarsestFirst()}, WithNoSpread(), r.observe())

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5))
	if !ok {
		t.Fatalf("Liquidity: ok=false — an absent rollup must not refuse the instrument, which " +
			"would report a zero liquidation horizon for a book that is fine")
	}
	if spec.ADV != 10 {
		t.Errorf("ADV = %v, want 10", spec.ADV)
	}
	if !askedEqual(f.asked, store.Resolution1d, store.Resolution1h, store.Resolution1m) {
		t.Errorf("series read = %v, want [1d 1h 1m]", f.asked)
	}
	// AND IT IS COUNTED. "The rollup job is not running" and "the rollup job is
	// running" must not look the same from outside.
	r.exactly(t, ReasonCoarseSeriesEmpty, ReasonCoarseSeriesEmpty,
		ReasonWindowNotWhole, ReasonSpreadUnavailable)
}

// THE SUBTLE ONE, AND THE REASON THE FALLBACK CANNOT KEY ON EMPTINESS ALONE. A
// rollup backfilling from today forward has three days where the base series has
// five. Honouring it would refuse the instrument for insufficient history — a
// perfectly-liquid-looking zero horizon caused entirely by a job that is working
// correctly and has not finished.
func TestAHalfBackfilledRollupFallsBackRatherThanRefusingTheInstrument(t *testing.T) {
	var r recorder
	f := &seriesBars{series: map[store.Resolution][]store.Bar{
		store.Resolution1d: dailyBars(10, 10, 10), // 3 days, floor is 5
		store.Resolution1m: oneBarPerDay(10, 10, 10, 10, 10),
	}}
	p := mustProviderCfg(t, f, Config{ResolutionPreference: []store.Resolution{store.Resolution1d, store.Resolution1m}},
		WithNoSpread(), r.observe())

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5))
	if !ok {
		t.Fatalf("Liquidity: ok=false — a half-built rollup must not refuse an instrument the " +
			"base series can answer for")
	}
	if spec.ADV != 10 {
		t.Errorf("ADV = %v, want 10 (the base series' answer)", spec.ADV)
	}
	r.exactly(t, ReasonCoarseSeriesShort, ReasonWindowNotWhole, ReasonSpreadUnavailable)
	// The day count is how far the backfill has got, which is the operational
	// number somebody actually wants.
	if r.days[0] != 3 {
		t.Errorf("reported days = %d, want 3 — the count is what says how far the rollup has "+
			"backfilled", r.days[0])
	}
}

// Exhausting the preference is still a terminal refusal, and it reports the
// FINEST series' reason rather than a fallback reason — the fallback ones say
// "series skipped", and reading them as "instrument refused" would show every
// instrument failing on the day the rollup is deployed.
func TestAnInstrumentMissingFromEverySeriesIsRefusedWithTheTerminalReason(t *testing.T) {
	var r recorder
	f := &seriesBars{}
	p := mustProviderCfg(t, f, Config{ResolutionPreference: CoarsestFirst()}, WithNoSpread(), r.observe())

	if _, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5)); ok {
		t.Fatal("Liquidity: ok=true with no bars in any series")
	}
	if !askedEqual(f.asked, store.Resolution1d, store.Resolution1h, store.Resolution1m) {
		t.Errorf("series read = %v, want all three tried", f.asked)
	}
	if len(r.reason) != 3 || r.reason[2] != ReasonNoBars {
		t.Errorf("observations = %v, want the terminal %q last", r.reason, ReasonNoBars)
	}
}

// A CORRUPT ROLLUP MUST NOT BE PAPERED OVER BY THE BASE SERIES. Falling through
// here would let a broken job keep emitting bad bars with every risk run looking
// healthy — the whole value of a rollup is that its output can be trusted.
func TestACorruptCoarseSeriesRefusesRatherThanFallingBackToTheBaseSeries(t *testing.T) {
	var r recorder
	corrupt := dailyBars(10, 10, 10, 10, 10)
	corrupt[2].Volume = nil
	f := &seriesBars{series: map[store.Resolution][]store.Bar{
		store.Resolution1d: corrupt,
		store.Resolution1m: oneBarPerDay(10, 10, 10, 10, 10),
	}}
	p := mustProviderCfg(t, f, Config{ResolutionPreference: []store.Resolution{store.Resolution1d, store.Resolution1m}},
		WithNoSpread(), r.observe())

	if _, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5)); ok {
		t.Fatal("Liquidity: ok=true off a corrupt rollup — a defect must refuse, not fall back")
	}
	if !askedEqual(f.asked, store.Resolution1d) {
		t.Errorf("series read = %v, want only [1d] — falling back would hide the defect", f.asked)
	}
	r.only(t, ReasonUnusableVolume, 0)
}

// A store that cannot be read is not a rollup that is missing.
func TestAStoreErrorOnACoarseSeriesDoesNotFallBack(t *testing.T) {
	var r recorder
	f := &seriesBars{
		errs:   map[store.Resolution]error{store.Resolution1d: errors.New("boom")},
		series: map[store.Resolution][]store.Bar{store.Resolution1m: oneBarPerDay(10, 10, 10, 10, 10)},
	}
	p := mustProviderCfg(t, f, Config{ResolutionPreference: []store.Resolution{store.Resolution1d, store.Resolution1m}},
		WithNoSpread(), r.observe())

	if _, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5)); ok {
		t.Fatal("Liquidity: ok=true after a store error")
	}
	if !askedEqual(f.asked, store.Resolution1d) {
		t.Errorf("series read = %v, want only [1d]", f.asked)
	}
	r.only(t, ReasonStoreError, 0)
}

// The default is unchanged and deliberately so: a silent default onto a series
// no job in this repository is known to write would refuse every instrument and
// report a zero liquidation horizon, dressed as an optimisation.
func TestTheDefaultProviderStillReadsOnlyTheBaseSeries(t *testing.T) {
	f := &seriesBars{series: map[store.Resolution][]store.Bar{
		store.Resolution1m: oneBarPerDay(10, 10, 10, 10, 10),
	}}
	p := mustProviderCfg(t, f, Config{}, WithNoSpread())

	if _, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5)); !ok {
		t.Fatal("Liquidity: not ok")
	}
	if !askedEqual(f.asked, store.Resolution1m) {
		t.Errorf("series read = %v, want only [1m] — the coarse search must be opt-in", f.asked)
	}
}

// ===== THE PREFERENCE IS VALIDATED, NOT TRUSTED =====

func TestAFineFirstOrDuplicatedResolutionPreferenceIsRefusedAtConstruction(t *testing.T) {
	// 1m always resolves, so a fine-first list would choose it every time and the
	// rest of the preference would be dead configuration that reads as live.
	for _, pref := range [][]store.Resolution{
		{store.Resolution1m, store.Resolution1d},
		{store.Resolution1d, store.Resolution1d},
		{store.Resolution1h, store.Resolution1d, store.Resolution1m},
	} {
		_, err := FromBars(&seriesBars{}, Config{Venue: venue, ResolutionPreference: pref}, WithNoSpread())
		if !errors.Is(err, errPreferenceNotCoarsestFirst) {
			t.Errorf("FromBars(%v) error = %v, want errPreferenceNotCoarsestFirst", pref, err)
		}
	}
}

func TestSettingBothResolutionFormsIsRefusedAtConstruction(t *testing.T) {
	_, err := FromBars(&seriesBars{}, Config{
		Venue:                venue,
		Resolution:           store.Resolution1m,
		ResolutionPreference: CoarsestFirst(),
	}, WithNoSpread())
	if !errors.Is(err, errBothResolutionForms) {
		t.Errorf("error = %v, want errBothResolutionForms — one decision, one spelling", err)
	}
}

// THE ERROR IS ASSERTED BY IDENTITY, NOT BY "SOMETHING WENT WRONG". A test that
// accepts any non-nil error here passes when the unstored entry is silently
// DROPPED and the remaining list happens to trip the ordering rule instead —
// which is a real bug reading as a pass. Both placements are checked because
// only the non-leading one exposes the drop by returning nil at all.
func TestAnUnstoredResolutionInThePreferenceIsRefusedAtConstruction(t *testing.T) {
	for _, pref := range [][]store.Resolution{
		{"5m", store.Resolution1m},
		{store.Resolution1d, "5m", store.Resolution1m},
	} {
		_, err := FromBars(&seriesBars{}, Config{Venue: venue, ResolutionPreference: pref}, WithNoSpread())
		if !errors.Is(err, errUnstoredResolution) {
			t.Errorf("FromBars(%v) error = %v, want errUnstoredResolution — a resolution this "+
				"platform does not store must be named as such, not silently dropped", pref, err)
		}
	}
}

func TestAnUnstoredPinnedResolutionIsRefusedAtConstruction(t *testing.T) {
	_, err := FromBars(&seriesBars{}, Config{Venue: venue, Resolution: "5m"}, WithNoSpread())
	if !errors.Is(err, errUnstoredResolution) {
		t.Errorf("error = %v, want errUnstoredResolution", err)
	}
}

func TestCoarsestFirstIsOrderedCoarsestFirstAndIsAccepted(t *testing.T) {
	if _, err := FromBars(&seriesBars{}, Config{Venue: venue, ResolutionPreference: CoarsestFirst()}, WithNoSpread()); err != nil {
		t.Fatalf("CoarsestFirst is refused by the order rule it is supposed to satisfy: %v", err)
	}
	// A shared mutable default would let one caller reorder every other caller's
	// preference; CoarsestFirst returns a fresh slice each call.
	a, b := CoarsestFirst(), CoarsestFirst()
	a[0] = store.Resolution1m
	if b[0] != store.Resolution1d {
		t.Error("CoarsestFirst hands out shared backing storage — one caller can reorder another's")
	}
}

// ===== THE SPREAD POSTURE, ANSWERED TO THE MEASURE REGISTRY =====

// compute.RegisterLiquidityRisk asks this before deciding whether LVaR99 is a
// measure worth putting on the wire. The relationship is duck-typed so this
// package does not import the registry, and THIS ASSERTION IS WHAT HOLDS IT: a
// rename on either side would otherwise silently put the degenerate LVaR99 back
// on the wire, because compute treats "does not implement" as "no claim".
var _ compute.SpreadServing = (*Provider)(nil)

func TestTheNoSpreadPostureDeclaresItServesNoSpread(t *testing.T) {
	p := mustProvider(t, &fakeBars{}, WithNoSpread())
	if p.ServesSpread() {
		t.Error("ServesSpread = true under WithNoSpread — every LVaR99 it could produce equals VaR99")
	}
}

func TestADeskEnteredSpreadTableDeclaresItServesASpread(t *testing.T) {
	p := mustProvider(t, &fakeBars{}, WithAssumedSpreads(map[string]float64{instr: 0.0005}))
	if !p.ServesSpread() {
		t.Error("ServesSpread = false with a desk-entered spread table — LVaR99 would be dropped " +
			"from a deployment that can actually compute it")
	}
}

// Both postures together still serve a real spread for the names the table
// covers, so the measure is worth registering.
func TestABothPosturesProviderStillDeclaresItServesASpread(t *testing.T) {
	p := mustProvider(t, &fakeBars{}, WithAssumedSpreads(map[string]float64{instr: 0.0005}), WithNoSpread())
	if !p.ServesSpread() {
		t.Error("ServesSpread = false with a non-empty spread table")
	}
}

// ===== THE WINDOW THE ADV WAS MEASURED OVER IS ACCOUNTED FOR (#591) =====

// THE COUNTER IS NOT STUCK ON, which is the half that makes the other half worth
// reading. A daily series covering every day of the window is WHOLE, and a
// resolution over it reports the spread posture and nothing else.
//
// This is the shape the production preference aims at: CoarsestFirst tries 1d
// first, so a populated rollup over the configured window is the silent case and
// the rate at which ReasonWindowNotWhole starts firing is a rollup losing days.
func TestAWholeWindowReportsNoMissingBuckets(t *testing.T) {
	var r recorder
	f := &seriesBars{series: map[store.Resolution][]store.Bar{
		store.Resolution1d: dailyBars(10, 10, 10, 10, 10),
	}}
	p := mustProviderCfg(t, f, Config{Resolution: store.Resolution1d},
		WithWindow(5*24*time.Hour), WithNoSpread(), r.observe())

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 5))
	if !ok {
		t.Fatalf("Liquidity: not ok")
	}
	if spec.ADV != 10 {
		t.Errorf("ADV = %v, want 10", spec.ADV)
	}
	r.exactly(t, ReasonSpreadUnavailable)
}

// THE UNDERSTATEMENT, MEASURED RATHER THAN ARGUED — and the ADV is still served.
//
// Day one holds a complete 1440-bar grid at volume 1. Day two holds ONE minute of
// the same tape. dailyTotals counts both as days, so the mean is 1441/2 = 720.5
// against a real daily volume of 1440: an ADV understated by half, which through
// DaysToLiquidate DOUBLES the reported liquidation horizon. Conservative, in
// range, and invisible — which is why it is counted rather than refused.
//
// REFUSING WOULD BE THE UNSAFE REPAIR. liquidity.Profile drops a refused
// position, and a book of dropped positions reports LiquidationHorizon 0.0000
// days: an active claim of perfect liquidity. So ok stays true and the coverage
// comes out beside it.
func TestAPartiallyCoveredDayUnderstatesTheADVAndIsCounted(t *testing.T) {
	var r recorder
	bars := fullDay(day0, 1)
	next := day0.AddDate(0, 0, 1)
	bars = append(bars, testBar(next, 1, next))

	p := mustProvider(t, &fakeBars{bars: bars},
		WithWindow(2*24*time.Hour), WithMinDays(2), WithNoSpread(), r.observe())

	spec, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 2))
	if !ok {
		t.Fatalf("Liquidity: ok=false — a partially covered window must not refuse the " +
			"instrument, which reports a ZERO liquidation horizon for a book that is fine")
	}
	if spec.ADV != 720.5 {
		t.Errorf("ADV = %v, want 720.5 — 1441 units over two TOUCHED days, against a real "+
			"daily volume of 1440. If this ever equals 1440 the measure changed and the "+
			"doc on dailyTotals is wrong", spec.ADV)
	}
	r.exactly(t, ReasonWindowNotWhole, ReasonSpreadUnavailable)
	if r.days[0] != 2 {
		t.Errorf("days = %d, want 2 — the count is days TOUCHED, which is exactly the number "+
			"this reason exists to qualify", r.days[0])
	}
}

// A WINDOW HOLDING NO WHOLE BUCKET IS NOT A COVERED ONE. store.Window.Whole() is
// VACUOUSLY true over zero buckets and says so in its own doc, so the guard is
// against inheriting that silence — a window shorter than one bar would otherwise
// publish as fully covered, which is the defect this closes arriving by the back
// door.
//
// THE FAKE ANSWERS A WINDOW THE REAL STORE WOULD NOT, deliberately: BarQuery
// bounds on BucketStart, so a one-hour window over a daily series selects nothing
// and the branch is unreachable from a real read. That is what makes it a GUARD
// rather than a path, and a guard nothing exercises is a guard nobody knows the
// direction of.
func TestAWindowTooShortToHoldABucketIsNotReportedAsCovered(t *testing.T) {
	var r recorder
	p := mustProviderCfg(t, &fakeBars{bars: dailyBars(10)}, Config{Resolution: store.Resolution1d},
		WithWindow(time.Hour), WithMinDays(1), WithNoSpread(), r.observe())

	if _, ok := p.Liquidity(context.Background(), instr, day0.AddDate(0, 0, 1)); !ok {
		t.Fatalf("Liquidity: not ok")
	}
	r.exactly(t, ReasonWindowNotWhole, ReasonSpreadUnavailable)
}
