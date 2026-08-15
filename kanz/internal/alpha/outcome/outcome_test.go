package outcome

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/alpha/score"
	"github.com/eighred/kanz/internal/marketdata/store"
)

var origin = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func dec(f float64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: int64(math.Round(f * 100)), Exponent: -2}
}

// bar builds minute i with an explicit high and low, so a test can put the move
// exactly where it means to.
func bar(i int, closePx, high, low float64) store.Bar {
	start := origin.Add(time.Duration(i) * time.Minute)
	return store.Bar{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: store.Resolution1m,
		BucketStart: start,
		Open:        dec(closePx), High: dec(high), Low: dec(low), Close: dec(closePx),
		Volume: dec(1), KnowledgeTime: start.Add(time.Minute),
	}
}

func storeWith(t *testing.T, bars ...store.Bar) *store.Memory {
	t.Helper()
	m := store.NewMemory()
	if err := m.PutBars(context.Background(), bars); err != nil {
		t.Fatal(err)
	}
	return m
}

func resolver(t *testing.T, m store.BarStore) *Resolver {
	t.Helper()
	r, err := NewResolver(m, Config{Venue: "XBIN"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func claim(t *testing.T, threshold float64, horizon time.Duration) score.Score {
	t.Helper()
	s, err := score.New(0.7, threshold, horizon, "m-1")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// far is a knowledge horizon well past every fixture, so "unresolved" in these
// tests is never about asOf unless the test is about asOf.
var far = origin.Add(24 * time.Hour)

// THE REFERENCE PRICE IS THE LAST COMPLETED BAR, NOT THE ONE IN PROGRESS.
//
// This is the lookahead that flatters. The signal fires at 12:03:30, inside bar
// 3, whose close (110) already contains the move the strategy reacted to. The
// honest reference is bar 2's close (100), which is what a decision at 12:03:30
// could actually have seen.
//
// Measured from 100, a +5% claim needs 105 and the window reaches 104 — a MISS.
// Measured from 110 it would need 115.5 and also miss, so the test uses a
// threshold that separates them: from 100 a +2% claim needs 102 and hits; from
// 110 it would need 112.2 and would not.
func TestResolve_TheReferenceIsTheLastCOMPLETEDBar(t *testing.T) {
	m := storeWith(t,
		bar(2, 100, 100, 100), // completes 12:03 — the last one knowable at 12:03:30
		bar(3, 110, 110, 100), // IN PROGRESS at 12:03:30; its close is not knowable
		bar(4, 103, 104, 102), // the horizon
		bar(5, 103, 104, 102),
	)
	at := origin.Add(3*time.Minute + 30*time.Second)

	out, ok, err := resolver(t, m).Resolve(context.Background(),
		claim(t, 0.02, 2*time.Minute), "BTC-USDT", at, far)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("unresolved — the reference bar was not found")
	}
	if !out.Hit {
		t.Error("marked a MISS. A +2% claim from the last completed close (100) needs 102 and " +
			"the window reached 104. Marking it missed means the reference came from bar 3's " +
			"close (110), which had not happened yet — lookahead, in the direction that " +
			"flatters the model")
	}
}

// "WITHIN THE HORIZON" IS A TOUCH, NOT THE TERMINAL CLOSE.
//
// The price reaches the threshold and reverts. The claim said "return >= X within
// H" and it did; a terminal reading answers "was it still there at the end",
// which is a different question and would mark this wrong.
func TestResolve_AMoveThatRevertsStillCountsAsATouch(t *testing.T) {
	m := storeWith(t,
		bar(0, 100, 100, 100),
		bar(1, 100, 106, 99), // touched +6%, closed back at 100
		bar(2, 100, 100, 99),
	)
	out, ok, err := resolver(t, m).Resolve(context.Background(),
		claim(t, 0.05, 2*time.Minute), "BTC-USDT", origin.Add(time.Minute), far)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("unresolved")
	}
	if !out.Hit {
		t.Error("a +5% claim was marked a miss on a window whose high reached +6% — the " +
			"resolver is reading terminal closes, which answers a different question than the " +
			"claim asks")
	}
}

// A NEGATIVE THRESHOLD IS READ AGAINST THE LOWS.
//
// P(return >= -2%) is the claim a stop-loss model makes. Reading it against the
// highs would make it true the instant the price ROSE, inverting it.
func TestResolve_ANegativeThresholdReadsTheLows(t *testing.T) {
	// Price rises hard and never falls: a "return >= -2%" TOUCH claim asks
	// whether it ever got down to 98, and it did not.
	m := storeWith(t,
		bar(0, 100, 100, 100),
		bar(1, 120, 125, 100),
		bar(2, 130, 135, 120),
	)
	out, ok, err := resolver(t, m).Resolve(context.Background(),
		claim(t, -0.02, 2*time.Minute), "BTC-USDT", origin.Add(time.Minute), far)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("unresolved")
	}
	if out.Hit {
		t.Error("a 'return >= -2%' claim was marked TRUE on a series that only rose — the " +
			"threshold is being read against the highs, which makes every downside claim true " +
			"the moment the price goes up")
	}

	// And it IS true when the price actually falls that far.
	m2 := storeWith(t,
		bar(0, 100, 100, 100),
		bar(1, 99, 100, 97), // low 97 is -3%
		bar(2, 99, 100, 98),
	)
	out2, ok2, err := resolver(t, m2).Resolve(context.Background(),
		claim(t, -0.02, 2*time.Minute), "BTC-USDT", origin.Add(time.Minute), far)
	if err != nil || !ok2 {
		t.Fatalf("unresolved: %v", err)
	}
	if !out2.Hit {
		t.Error("a 'return >= -2%' claim was marked a miss on a window whose low reached -3%")
	}
}

// A CLAIM WHOSE HORIZON HAS NOT ELAPSED IS UNRESOLVED, NOT A MISS.
//
// This is the bias that would hit a live model hardest: its newest scores are
// exactly the ones whose windows are still open, and counting them as misses
// makes a good model look like it just stopped working.
func TestResolve_AnOpenHorizonIsUnresolved(t *testing.T) {
	m := storeWith(t, bar(0, 100, 100, 100), bar(1, 100, 100, 100))
	// asOf sits INSIDE the horizon.
	out, ok, err := resolver(t, m).Resolve(context.Background(),
		claim(t, 0.05, time.Hour), "BTC-USDT", origin.Add(time.Minute), origin.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("a claim whose horizon has not finished was resolved as Hit=%v — an open "+
			"window marked as a miss biases calibration downward, and does it worst for the "+
			"newest scores", out.Hit)
	}
}

// A DATA GAP IS UNRESOLVED, NOT A MISS.
//
// A venue outage is not a market that failed to move, and attributing one to the
// model is how an infrastructure incident becomes evidence against a strategy.
func TestResolve_AGapInTheSeriesIsUnresolved(t *testing.T) {
	// A reference bar exists, and then nothing for the whole horizon.
	m := storeWith(t, bar(0, 100, 100, 100))
	_, ok, err := resolver(t, m).Resolve(context.Background(),
		claim(t, 0.05, 5*time.Minute), "BTC-USDT", origin.Add(time.Minute), far)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a horizon with no bars was resolved — a venue outage is not a market that " +
			"did not move")
	}
}

// NO REFERENCE PRICE IS UNRESOLVED.
func TestResolve_NoPriorBarIsUnresolved(t *testing.T) {
	// Bars exist only AFTER the signal: nothing completed before it.
	m := storeWith(t, bar(5, 100, 105, 100), bar(6, 100, 105, 100))
	_, ok, err := resolver(t, m).Resolve(context.Background(),
		claim(t, 0.01, 2*time.Minute), "BTC-USDT", origin, far)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("resolved with no bar completed before the signal — there was no price to " +
			"measure a return from")
	}
}

// A CORRECTION STAMPED AFTER asOf DOES NOT CHANGE A RESOLVED OUTCOME.
//
// The bar store is bitemporal, so a calibration run pinned to a knowledge horizon
// must read the same bars next month. Without that, re-running last quarter's
// calibration silently produces different numbers and nobody can say which run
// was right.
func TestResolve_IsReproducibleAcrossARestatement(t *testing.T) {
	ctx := context.Background()
	m := storeWith(t, bar(0, 100, 100, 100), bar(1, 100, 101, 99), bar(2, 100, 101, 99))
	asOf := origin.Add(30 * time.Minute)
	s := claim(t, 0.05, 2*time.Minute)

	before, ok, err := resolver(t, m).Resolve(ctx, s, "BTC-USDT", origin.Add(time.Minute), asOf)
	if err != nil || !ok {
		t.Fatalf("unresolved: %v", err)
	}
	if before.Hit {
		t.Fatal("fixture should miss: the window never reaches +5%")
	}

	// The venue restates bar 1 with a huge high, learned an hour after asOf.
	restated := bar(1, 100, 200, 99)
	restated.KnowledgeTime = asOf.Add(time.Hour)
	if err := m.PutBars(ctx, []store.Bar{restated}); err != nil {
		t.Fatal(err)
	}

	after, ok, err := resolver(t, m).Resolve(ctx, s, "BTC-USDT", origin.Add(time.Minute), asOf)
	if err != nil || !ok {
		t.Fatalf("unresolved after restatement: %v", err)
	}
	if after.Hit != before.Hit {
		t.Error("the outcome changed after a restatement stamped past asOf — a calibration run " +
			"pinned to a knowledge horizon must reproduce")
	}

	// AND THE RESTATEMENT IS VISIBLE at a later horizon, or the test above would
	// pass on a store that simply drops corrections.
	later, ok, err := resolver(t, m).Resolve(ctx, s, "BTC-USDT", origin.Add(time.Minute),
		asOf.Add(2*time.Hour))
	if err != nil || !ok {
		t.Fatalf("unresolved at the later horizon: %v", err)
	}
	if !later.Hit {
		t.Error("the restated bar never became visible — the reproducibility test above would " +
			"pass on a store that drops corrections entirely")
	}
}

// AN INVALID SCORE IS AN ERROR, not an unresolved outcome.
//
// Unresolved means "we could not mark it"; a malformed claim means the caller is
// holding something that should never have been built, and quietly skipping it
// would hide that.
func TestResolve_RefusesAMalformedScore(t *testing.T) {
	m := storeWith(t, bar(0, 100, 100, 100))
	var zero score.Score
	if _, _, err := resolver(t, m).Resolve(context.Background(), zero, "BTC-USDT", origin, far); err == nil {
		t.Error("the zero Score was resolved rather than refused")
	}
}

// A RESOLVER WITH NO VENUE IS REFUSED AT CONSTRUCTION.
func TestNewResolver_RequiresAVenue(t *testing.T) {
	if _, err := NewResolver(store.NewMemory(), Config{}); err == nil {
		t.Error("a resolver with no venue was constructed")
	}
	if _, err := NewResolver(nil, Config{Venue: "XBIN"}); err == nil {
		t.Error("a resolver with no store was constructed")
	}
	r, err := NewResolver(store.NewMemory(), Config{Venue: "XBIN"})
	if err != nil {
		t.Fatal(err)
	}
	if r.cfg.Resolution != store.Resolution1m {
		t.Errorf("default resolution = %q, want the 1m base series", r.cfg.Resolution)
	}
}

// THE RESOLVED OUTCOMES FEED Calibrate, which is the point of the package.
func TestResolve_FeedsCalibration(t *testing.T) {
	ctx := context.Background()
	m := storeWith(t,
		bar(0, 100, 100, 100),
		bar(1, 100, 110, 99), // a +10% touch
		bar(2, 100, 101, 99),
	)
	r := resolver(t, m)

	var outs []score.Outcome
	for _, th := range []float64{0.05, 0.20} { // one hits, one does not
		o, ok, err := r.Resolve(ctx, claim(t, th, 2*time.Minute), "BTC-USDT",
			origin.Add(time.Minute), far)
		if err != nil || !ok {
			t.Fatalf("threshold %v unresolved: %v", th, err)
		}
		outs = append(outs, o)
	}
	rep, err := score.Calibrate(outs, score.DefaultBins)
	if err != nil {
		t.Fatal(err)
	}
	if rep.N != 2 {
		t.Fatalf("N = %d, want 2", rep.N)
	}
	if rep.BaseRate != 0.5 {
		t.Errorf("BaseRate = %v, want 0.5 — one claim of the two came true", rep.BaseRate)
	}
}
