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
	// The floor is not off by one in the other direction either: five days is a
	// clean resolution, so the only report is the spread posture.
	r.only(t, ReasonSpreadUnavailable, 5)
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
	if len(r.reason) != 0 {
		t.Errorf("observations = %v, want none — an entered assumption is not a degradation", r.reason)
	}
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

	// And the degeneracy is COUNTED, not inferred from a suspiciously round LVaR.
	r.only(t, ReasonSpreadUnavailable, 5)
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
