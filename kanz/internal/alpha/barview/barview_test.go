package barview

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/pkg/alpha"
)

// origin is a minute boundary, so a test that means "mid-bar" has to say so.
var origin = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

const (
	instrument = "BTC-USDT"
	venue      = "XBIN"
)

func dec(f float64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: int64(math.Round(f * 100)), Exponent: -2}
}

// bar builds minute i, KNOWN AT ITS OWN OPEN.
//
// THAT STAMP IS DELIBERATE AND IT IS WHAT MAKES THESE TESTS TEST WHAT THEY SAY.
// The realistic stamp — learned when the bucket closed — makes the KNOWLEDGE
// bound exclude the in-flight bar all by itself, so a test named "does not read
// the bar containing the decision time" would pass with the OBSERVATION bound
// deleted. It did: the first mutation run against these fixtures moved `To`
// forward a whole bucket and that test still passed, because AsOf was quietly
// doing its work. Stamping knowledge at the open removes AsOf from the picture,
// leaving only the bound each test names. The knowledge axis has its own test,
// which sets KnowledgeTime itself.
func bar(i int, closePx float64) store.Bar {
	start := origin.Add(time.Duration(i) * time.Minute)
	return store.Bar{
		InstrumentID: instrument, Venue: venue, Resolution: store.Resolution1m,
		BucketStart: start,
		Open:        dec(closePx), High: dec(closePx), Low: dec(closePx), Close: dec(closePx),
		Volume: dec(1), KnowledgeTime: start,
	}
}

func attestation(i int, observed time.Duration) store.Coverage {
	start := origin.Add(time.Duration(i) * time.Minute)
	return store.Coverage{
		InstrumentID: instrument, Venue: venue, Resolution: store.Resolution1m,
		BucketStart: start, Observed: observed, Attestor: "sub-1",
		RecordedAt: start.Add(time.Minute),
	}
}

func memWith(t *testing.T, bars []store.Bar, cov []store.Coverage) *store.Memory {
	t.Helper()
	m := store.NewMemory()
	if len(bars) > 0 {
		if err := m.PutBars(context.Background(), bars); err != nil {
			t.Fatal(err)
		}
	}
	if len(cov) > 0 {
		if err := m.PutCoverage(context.Background(), cov); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// viewAt builds a view pinned to `decision`, with a lookback short enough that a
// fixture can fill the whole window.
func viewAt(t *testing.T, m *store.Memory, decision time.Time, lookback int) *View {
	t.Helper()
	v, err := New(m, m, Config{
		InstrumentID: instrument, Venue: venue, Lookback: lookback, DecisionTime: decision,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func run(t *testing.T, v *View, at time.Time) alpha.Reading {
	t.Helper()
	r, err := v.At(context.Background(), at)
	if err != nil {
		t.Fatalf("At(%s): %v", at.Format(time.RFC3339), err)
	}
	return r
}

// series builds minutes [0, n) with an ascending close, so the last completed
// bar's close names the minute it came from: minute i closes at 100+i.
func series(n int) []store.Bar {
	out := make([]store.Bar, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, bar(i, 100+float64(i)))
	}
	return out
}

// ---------------------------------------------------------------------------
// THE LOOK-AHEAD REFUSAL. This is the property the whole seam exists for: a
// leak here invalidates every backtest built on it, and it never fails loudly.
// ---------------------------------------------------------------------------

func TestViewRefusesAReadPastItsDecisionTime(t *testing.T) {
	m := memWith(t, series(30), nil)
	v := viewAt(t, m, origin.Add(10*time.Minute), 10)

	if _, err := v.At(context.Background(), origin.Add(11*time.Minute)); !errors.Is(err, alpha.ErrLookahead) {
		t.Fatalf("a read one minute past the decision time must be refused with "+
			"alpha.ErrLookahead — a truncated answer is a window the caller did not ask for and "+
			"cannot tell apart; got %v", err)
	}
	// Exactly at the decision time is NOT look-ahead: it is the decision.
	if _, err := v.At(context.Background(), origin.Add(10*time.Minute)); err != nil {
		t.Fatalf("a read AT the decision time is the decision itself and must be answered: %v", err)
	}
}

func TestViewDoesNotReadTheBarContainingTheDecisionTime(t *testing.T) {
	// Minutes 0..9 exist. A read 30 seconds into minute 9 must see minute 8's
	// close (108), never minute 9's (109) — minute 9 had not finished.
	m := memWith(t, series(10), nil)
	at := origin.Add(9*time.Minute + 30*time.Second)
	v := viewAt(t, m, at, 10)

	r := run(t, v, at)
	if !r.CloseOK {
		t.Fatal("expected a close from the completed history")
	}
	if math.Abs(r.Close-108) > 1e-9 {
		t.Fatalf("the in-flight bar leaked into the read: close = %v, want 108 (minute 8, the "+
			"last bar to COMPLETE at or before the read). A mid-bar close already contains the "+
			"move the engine reacted to, which is the flattering direction nobody audits.", r.Close)
	}
	if r.Contiguous != 9 {
		t.Fatalf("contiguous = %d, want 9 — minutes 0..8, with the in-flight minute 9 excluded", r.Contiguous)
	}
}

func TestViewDoesNotReadABarThatHadNotPrinted(t *testing.T) {
	// The store holds thirty minutes. A read on the minute-10 boundary must see
	// minutes 0..9 and nothing after.
	m := memWith(t, series(30), nil)
	at := origin.Add(10 * time.Minute)
	v := viewAt(t, m, at, 30)

	r := run(t, v, at)
	if !r.CloseOK || math.Abs(r.Close-109) > 1e-9 {
		t.Fatalf("close = %v (ok=%v), want 109 — minute 9 is the last COMPLETED bar at the "+
			"minute-10 boundary, and every later bar in the store is a bar that had not printed",
			r.Close, r.CloseOK)
	}
	if r.Contiguous != 10 {
		t.Fatalf("contiguous = %d, want 10 — bars from the future must not extend the run", r.Contiguous)
	}
}

func TestViewDoesNotReadACorrectionThatArrivedAfterTheDecision(t *testing.T) {
	// Minute 25 was restated to 999 a day later. A view deciding at minute 30
	// must read the ORIGINAL: the restatement had not been learned. The bars are
	// from the right minutes either way, which is exactly what makes this leak
	// invisible — nothing about the window looks wrong.
	bars := series(30)
	restated := bar(25, 999)
	restated.KnowledgeTime = origin.Add(24 * time.Hour)
	bars = append(bars, restated)

	m := memWith(t, bars, nil)
	at := origin.Add(30 * time.Minute)
	v := viewAt(t, m, at, 30)

	r := run(t, v, at)
	got, ok := r.Indicators["sma_20"]
	if !ok {
		t.Fatal("expected sma_20 over an unbroken thirty-bar window")
	}
	// The 20-period SMA covers minutes 10..29, closes 110..129, mean 119.5. The
	// restatement would move it to 163.2 — a leak here is not subtle in the
	// fixture and is invisible in production.
	if math.Abs(got-119.5) > 1e-9 {
		t.Fatalf("a correction learned after the decision time leaked into the read: sma_20 = "+
			"%v, want 119.5. BarQuery.To bounds the OBSERVATION window; only BarQuery.AsOf bounds "+
			"the KNOWLEDGE one, and a read that sets the first without the second returns bars "+
			"from the right minutes saying what the venue decided later they should have said.", got)
	}

	// The fixture must actually HOLD the restatement, or the assertion above is
	// vacuous — it would pass against a store that had simply lost the write.
	all, err := m.Bars(context.Background(), store.BarQuery{
		InstrumentID: instrument, Venue: venue, Resolution: store.Resolution1m,
		From: origin, To: origin.Add(30 * time.Minute), AsOf: origin.Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawRestatement bool
	for _, b := range all {
		if b.BucketStart.Equal(origin.Add(25*time.Minute)) && b.Close.GetCoefficient() == 99900 {
			sawRestatement = true
		}
	}
	if !sawRestatement {
		t.Fatal("the fixture never stored the restatement, so the assertion above proved nothing " +
			"about the knowledge horizon")
	}
}

// ---------------------------------------------------------------------------
// THE #594 ASYMMETRY: a signal provable from a partial window is still provable.
// ---------------------------------------------------------------------------

func TestAGappedWindowStillYieldsTheReadingsItCanProve(t *testing.T) {
	// Minutes 0..4 exist, minutes 5..9 are missing, minutes 10..40 exist. The
	// window is holed, and the contiguous tail is still 31 bars long.
	var bars []store.Bar
	for i := 0; i < 5; i++ {
		bars = append(bars, bar(i, 100+float64(i)))
	}
	for i := 10; i < 41; i++ {
		bars = append(bars, bar(i, 100+float64(i)))
	}
	m := memWith(t, bars, nil)
	at := origin.Add(41 * time.Minute)
	v := viewAt(t, m, at, 41)

	r := run(t, v, at)
	if _, ok := r.Indicators["sma_20"]; !ok {
		t.Fatal("a 20-period SMA is provable from an unbroken 31-bar tail and must be returned: " +
			"refusing every gapped window throws away every provable signal, and fold.go emits NO " +
			"BAR for a minute in which nothing traded, so a gapped window is the normal input here")
	}
	if r.Contiguous != 31 {
		t.Fatalf("contiguous = %d, want 31 — the readings must claim the unbroken tail, not the "+
			"whole holed window", r.Contiguous)
	}
	// And the hole is REPORTED rather than hidden: the engine can tell.
	if r.Coverage.Missing() != 5 {
		t.Fatalf("coverage.Missing() = %d, want 5", r.Coverage.Missing())
	}
	if r.Coverage.Sound() {
		t.Fatal("five buckets have no bar and no attestation — the window cannot support a claim " +
			"ABOUT THE WHOLE WINDOW, and Sound() saying otherwise is the confident number this " +
			"seam exists to refuse")
	}
}

func TestCoverageSeparatesAQuietMarketFromAnUnobservedOne(t *testing.T) {
	// Minutes 0..9 exist except 5 and 6. The attestor vouches for minute 5 for
	// the whole minute (quiet), for minute 6 for only part of it (unknown).
	var bars []store.Bar
	for i := 0; i < 10; i++ {
		if i == 5 || i == 6 {
			continue
		}
		bars = append(bars, bar(i, 100+float64(i)))
	}
	cov := []store.Coverage{
		attestation(5, time.Minute),
		attestation(6, 59*time.Second),
	}
	m := memWith(t, bars, cov)
	at := origin.Add(10 * time.Minute)
	v := viewAt(t, m, at, 10)

	r := run(t, v, at)
	if r.Coverage.Buckets != 10 {
		t.Fatalf("buckets = %d, want 10", r.Coverage.Buckets)
	}
	if r.Coverage.Observed != 8 {
		t.Fatalf("observed = %d, want 8", r.Coverage.Observed)
	}
	if r.Coverage.Quiet != 1 {
		t.Fatalf("quiet = %d, want 1 — minute 5 is a MEASURED absence: the attestor was watching "+
			"the whole bucket and nothing traded", r.Coverage.Quiet)
	}
	if r.Coverage.Unknown != 1 {
		t.Fatalf("unknown = %d, want 1 — minute 6 was watched for 59 of 60 seconds, and a short "+
			"attestation cannot rule out a print in the second nobody saw", r.Coverage.Unknown)
	}
	if r.Coverage.Sound() {
		t.Fatal("one bucket is unaccounted for; Sound() must be false")
	}
}

func TestAWholeWindowWithAttestedQuietMinutesIsSound(t *testing.T) {
	var bars []store.Bar
	for i := 0; i < 10; i++ {
		if i == 5 {
			continue
		}
		bars = append(bars, bar(i, 100+float64(i)))
	}
	m := memWith(t, bars, []store.Coverage{attestation(5, time.Minute)})
	at := origin.Add(10 * time.Minute)
	v := viewAt(t, m, at, 10)

	r := run(t, v, at)
	if !r.Coverage.Sound() {
		t.Fatalf("every absence is attested; Sound() must be true. quiet=%d unknown=%d",
			r.Coverage.Quiet, r.Coverage.Unknown)
	}
	if r.Coverage.Missing() != 1 {
		t.Fatalf("missing = %d, want 1 — Sound does not mean Whole, and collapsing the two "+
			"reintroduces the conflation the coverage record exists to break", r.Coverage.Missing())
	}
}

// ---------------------------------------------------------------------------
// A MISSING READING IS AN ABSENT KEY, NEVER A ZERO.
// ---------------------------------------------------------------------------

func TestInsufficientHistoryOmitsTheKeyRatherThanReturningZero(t *testing.T) {
	m := memWith(t, series(5), nil)
	at := origin.Add(5 * time.Minute)
	v := viewAt(t, m, at, 10)

	r := run(t, v, at)
	for _, key := range []string{"sma_20", "rsi_14", "atr_14", "macd"} {
		if got, ok := r.Indicators[key]; ok {
			t.Fatalf("%s must be ABSENT over five bars, not %v — 0 is a valid RSI meaning "+
				"'sold off hard' and a valid SMA meaning 'the price is zero', and either written "+
				"for 'not enough history' is indistinguishable from a real reading", key, got)
		}
	}
	if r.Contiguous != 5 {
		t.Fatalf("contiguous = %d, want 5", r.Contiguous)
	}
}

func TestAnEmptyWindowIsAReadingNotAnError(t *testing.T) {
	m := memWith(t, nil, nil)
	at := origin.Add(10 * time.Minute)
	v := viewAt(t, m, at, 10)

	r := run(t, v, at)
	if len(r.Indicators) != 0 || r.Contiguous != 0 || r.CloseOK {
		t.Fatalf("an empty window must answer with nothing rather than fail: %+v", r)
	}
	if r.Coverage.Unknown != 10 {
		t.Fatalf("unknown = %d, want 10 — an empty window is not a quiet market, and nothing "+
			"attests it", r.Coverage.Unknown)
	}
	if r.Coverage.Sound() {
		t.Fatal("no bar and no attestation for any bucket; Sound() must be false")
	}
}

// ---------------------------------------------------------------------------
// CONSTRUCTION REFUSALS. "Nothing configured" must not look like "checked, and
// fine".
// ---------------------------------------------------------------------------

func TestNewRefusesAMisconfiguredView(t *testing.T) {
	m := store.NewMemory()
	base := Config{InstrumentID: instrument, Venue: venue, DecisionTime: origin}

	cases := []struct {
		name string
		bars store.BarStore
		cov  store.CoverageStore
		cfg  Config
	}{
		{"nil bar store", nil, m, base},
		{"nil coverage store", m, nil, base},
		{"no instrument", m, m, Config{Venue: venue, DecisionTime: origin}},
		{"no venue", m, m, Config{InstrumentID: instrument, DecisionTime: origin}},
		{"no decision time", m, m, Config{InstrumentID: instrument, Venue: venue}},
		{"unstored resolution", m, m, Config{
			InstrumentID: instrument, Venue: venue, DecisionTime: origin, Resolution: "47s",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.bars, tc.cov, tc.cfg); err == nil {
				t.Fatal("expected a refusal at construction — a view that starts on a defaulted " +
					"decision time or a nil coverage store runs green while answering a different " +
					"question")
			}
		})
	}
}

func TestNewDefaultsResolutionAndLookbackButNeverTheDecisionTime(t *testing.T) {
	m := store.NewMemory()
	v, err := New(m, m, Config{InstrumentID: instrument, Venue: venue, DecisionTime: origin})
	if err != nil {
		t.Fatal(err)
	}
	if v.cfg.Resolution != store.Resolution1m {
		t.Fatalf("resolution = %q, want the 1m base series", v.cfg.Resolution)
	}
	if v.cfg.Lookback <= 34 {
		t.Fatalf("lookback = %d — it must exceed the longest period any C1 indicator uses by "+
			"enough that the recursive averages have decayed their seed", v.cfg.Lookback)
	}
	if !v.DecisionTime().Equal(origin) {
		t.Fatalf("decision time = %s, want %s", v.DecisionTime(), origin)
	}
	if v.InstrumentID() != instrument || v.MIC() != venue {
		t.Fatalf("identity is wrong: %s / %s", v.InstrumentID(), v.MIC())
	}
}

func TestZeroReadTimeIsRefused(t *testing.T) {
	m := store.NewMemory()
	v := viewAt(t, m, origin, 10)
	if _, err := v.At(context.Background(), time.Time{}); err == nil {
		t.Fatal("a zero read time must be refused rather than silently meaning the epoch")
	}
}

// TestTheSeamIsSatisfied pins the implementation to the interface. It is not
// redundant with the compile-time assertion in barview.go: this fails with a
// named test when a method is renamed, rather than only when somebody rebuilds.
func TestTheSeamIsSatisfied(t *testing.T) {
	m := store.NewMemory()
	v := viewAt(t, m, origin, 10)
	var seam alpha.BarView = v
	if seam.MIC() != venue {
		t.Fatalf("MIC through the seam = %q", seam.MIC())
	}
}
