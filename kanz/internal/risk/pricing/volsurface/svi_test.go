package volsurface

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing"
)

// sviQuotes generates listed-option quotes whose smile IS an SVI slice, so the
// fit has a known ground truth: price each strike under vol √(w(k)/T).
func sviQuotes(t *testing.T, p SVIParams, expiry, spot, r, q float64, strikes []float64) []OptionQuote {
	t.Helper()
	fwd := spot * math.Exp((r-q)*expiry)
	var quotes []OptionQuote
	for _, K := range strikes {
		w := p.TotalVar(math.Log(K / fwd))
		if w <= 0 {
			t.Fatalf("ground-truth slice has non-positive variance at K=%.4g", K)
		}
		vol := math.Sqrt(w / expiry)
		quotes = append(quotes, OptionQuote{
			Type:   pricing.Call,
			Strike: K,
			Expiry: expiry,
			Price:  pricing.BlackScholesPrice(pricing.Call, spot, K, expiry, r, q, vol),
		})
	}
	return quotes
}

// FitSVI must recover a synthetic SVI smile: fitted vols within a few bp of
// the generating vols across the quoted strikes, and the surface arb-free.
func TestFitSVI_RecoversSyntheticSmile(t *testing.T) {
	truth := SVIParams{A: 0.02, B: 0.4, Rho: -0.4, M: 0.0, Sigma: 0.4}
	const spot, r, q, expiry = 100.0, 0.03, 0.0, 0.5
	strikes := []float64{70, 80, 90, 95, 100, 105, 110, 120, 130}
	quotes := sviQuotes(t, truth, expiry, spot, r, q, strikes)

	surf, err := FitSVI(quotes, spot, pricing.FlatCurve(r), q)
	if err != nil {
		t.Fatalf("FitSVI: %v", err)
	}
	fwd := spot * math.Exp((r-q)*expiry)
	for _, K := range strikes {
		want := math.Sqrt(truth.TotalVar(math.Log(K/fwd)) / expiry)
		got, ok := surf.Vol(K, expiry)
		if !ok {
			t.Fatalf("Vol(%v) not ok", K)
		}
		if math.Abs(got-want) > 2e-3 {
			t.Errorf("vol at K=%v: got %.5f want %.5f", K, got, want)
		}
	}
}

// A flat smile is legitimate SVI (b=0) — the fit must not degenerate.
func TestFitSVI_FlatSmile(t *testing.T) {
	const spot, r, vol, expiry = 100.0, 0.02, 0.25, 1.0
	var quotes []OptionQuote
	for _, K := range []float64{80, 90, 100, 110, 120} {
		quotes = append(quotes, OptionQuote{
			Type: pricing.Call, Strike: K, Expiry: expiry,
			Price: pricing.BlackScholesPrice(pricing.Call, spot, K, expiry, r, 0, vol),
		})
	}
	surf, err := FitSVI(quotes, spot, pricing.FlatCurve(r), 0)
	if err != nil {
		t.Fatalf("FitSVI: %v", err)
	}
	if got, _ := surf.Vol(100, expiry); math.Abs(got-vol) > 2e-3 {
		t.Errorf("flat smile: got %.5f want %.5f", got, vol)
	}
}

// The Axel Vogt SVI parameters are the classic butterfly-arbitrage example —
// the density goes negative, so ButterflyFree must reject them.
func TestButterflyFree_RejectsVogtParams(t *testing.T) {
	vogt := SVIParams{A: -0.0410, B: 0.1331, Rho: 0.3060, M: 0.3586, Sigma: 0.4153}
	if vogt.ButterflyFree(-1.5, 1.5) {
		t.Fatal("Vogt parameters admit butterfly arbitrage; ButterflyFree must be false")
	}
	// A tame smile passes.
	ok := SVIParams{A: 0.02, B: 0.1, Rho: -0.3, M: 0, Sigma: 0.3}
	if !ok.ButterflyFree(-1.5, 1.5) {
		t.Fatal("arb-free parameters wrongly rejected")
	}
}

// Total variance decreasing in expiry is calendar arbitrage: a 6M 40%-vol
// slice followed by a 1Y 20%-vol slice must be rejected as a set.
func TestFitSVI_RejectsCalendarArbitrage(t *testing.T) {
	const spot, r = 100.0, 0.0
	strikes := []float64{85, 95, 100, 105, 115}
	var quotes []OptionQuote
	for _, K := range strikes {
		quotes = append(quotes,
			OptionQuote{Type: pricing.Call, Strike: K, Expiry: 0.5,
				Price: pricing.BlackScholesPrice(pricing.Call, spot, K, 0.5, r, 0, 0.40)},
			OptionQuote{Type: pricing.Call, Strike: K, Expiry: 1.0,
				Price: pricing.BlackScholesPrice(pricing.Call, spot, K, 1.0, r, 0, 0.20)},
		)
	}
	if _, err := FitSVI(quotes, spot, pricing.FlatCurve(r), 0); !errors.Is(err, ErrArbitrage) {
		t.Fatalf("want ErrArbitrage, got %v", err)
	}
}

func TestFitSVI_Errors(t *testing.T) {
	if _, err := FitSVI(nil, 100, pricing.FlatCurve(0.02), 0); !errors.Is(err, ErrSVIFit) {
		t.Errorf("empty quotes: want ErrSVIFit, got %v", err)
	}
	two := []OptionQuote{
		{Type: pricing.Call, Strike: 100, Expiry: 1, Price: 10},
		{Type: pricing.Call, Strike: 110, Expiry: 1, Price: 6},
	}
	if _, err := FitSVI(two, 100, pricing.FlatCurve(0.02), 0); !errors.Is(err, ErrSVIFit) {
		t.Errorf("2-quote slice: want ErrSVIFit, got %v", err)
	}
}

type staticVolQuotes struct {
	quotes []OptionQuote
	spot   float64
	err    error
}

func (s staticVolQuotes) Quotes(context.Context, string, time.Time) ([]OptionQuote, float64, error) {
	return s.quotes, s.spot, s.err
}

// Refresh publishes point-in-time; a failed fit leaves the prior surface
// serving; the store resolves per as_of (the curve.Store contract).
func TestVolCalibrator_RefreshAndStore(t *testing.T) {
	ctx := context.Background()
	const spot, r, expiry = 100.0, 0.02, 0.5
	truth := SVIParams{A: 0.02, B: 0.3, Rho: -0.3, M: 0, Sigma: 0.4}
	quotes := sviQuotes(t, truth, expiry, spot, r, 0, []float64{80, 90, 100, 110, 120})

	store := NewStore()
	asOf := time.Date(2026, 7, 1, 16, 0, 0, 0, time.UTC)
	cal := &Calibrator{
		Source: staticVolQuotes{quotes: quotes, spot: spot},
		Store:  store,
		Disc:   pricing.FlatCurve(r),
	}
	if _, err := cal.Refresh(ctx, "SPX", asOf); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := store.Vol(ctx, "SPX", 100, expiry, asOf.Add(-time.Minute)); ok {
		t.Error("resolved a surface before the first version")
	}
	got, ok := store.Vol(ctx, "SPX", 100, expiry, asOf.Add(time.Hour))
	if !ok {
		t.Fatal("surface not resolved after publish")
	}
	fwd := spot * math.Exp(r*expiry)
	want := math.Sqrt(truth.TotalVar(math.Log(100/fwd)) / expiry)
	if math.Abs(got-want) > 2e-3 {
		t.Errorf("stored vol: got %.5f want %.5f", got, want)
	}

	bad := &Calibrator{Source: staticVolQuotes{err: errors.New("vendor down")}, Store: store, Disc: pricing.FlatCurve(r)}
	if _, err := bad.Refresh(ctx, "SPX", asOf.Add(2*time.Hour)); err == nil {
		t.Fatal("Refresh must surface a source error")
	}
	if v, ok := store.Vol(ctx, "SPX", 100, expiry, asOf.Add(3*time.Hour)); !ok || math.Abs(v-got) > 1e-12 {
		t.Error("a failed Refresh must leave the previous surface serving")
	}
}

// A CONCURRENT BACKFILL MUST NOT MAKE A READER RESOLVE THE WRONG VERSION (#800).
//
// Store.Vol used to release the read lock after sort.Search and dereference
// vs[i-1] afterwards. The slice header is a copy; the backing array is not.
// Put's out-of-order branch does copy(vs[i+1:], vs[i:]) IN PLACE whenever cap
// exceeds len, so an index resolved under the lock addresses a different
// version by the time the reader reads it — and the reader gets a valid-looking
// implied vol from the wrong as-of.
//
// THIS TEST HAS BEEN SEEN TO FAIL. Against the pre-#800 shape it reported 114
// wrong-version reads in 4,188,000 over 20s, and 2–180 in ~1.8M over the 2s
// budget below, on every one of five runs. After the fix it is structurally
// impossible, so it cannot flake — the read of vs[i-1] now happens under the
// same read lock that gated the search.
//
// WHAT IT IS NOT. It is a value oracle, not a race detector: it catches the
// wrong NUMBER, which is why it works at all on a box where -race needs cgo and
// does not run. Its sensitivity is a function of scheduling, so a slow or
// single-CPU runner may observe zero hits against a genuinely broken store —
// the deterministic guard is
// test/arch/pricing_store_read_holds_its_lock_test.go, and this is the
// executable evidence behind it.
func TestVolStore_AConcurrentBackfillNeverResolvesAStaleVersion(t *testing.T) {
	const versions = 64
	st := NewStore()
	// Pre-grown so append NEVER reallocates: every backfill then shifts the SAME
	// backing array a concurrent reader is holding a header into, which is the
	// only arrangement in which the defect is reachable. A reallocating append
	// leaves the reader on a private, consistent copy and hides it.
	st.byUnderlying["X"] = make([]surfaceVersion, 0, 4*1024)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for k := 1; k <= versions; k++ {
		st.Put("X", base.Add(time.Duration(k)*time.Minute), flatSurface(float64(k)))
	}
	// The newest version is published last and never replaced, so a read as of
	// any later instant has exactly one correct answer for the whole test.
	newest := math.Sqrt(float64(versions))
	future := base.Add(1000 * time.Hour)

	ctx := context.Background()
	var stop atomic.Bool
	var reads, wrong atomic.Int64
	var wg sync.WaitGroup

	for r := 0; r < 32; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				for j := 0; j < 1000; j++ {
					v, ok := st.Vol(ctx, "X", 100, 1, future)
					reads.Add(1)
					if !ok || math.Abs(v-newest) > 1e-12 {
						wrong.Add(1)
					}
				}
			}
		}()
	}
	// The writer only ever backfills EARLIER as-ofs, so it inserts at index 0 and
	// shifts the whole array right on every Put while never changing the answer
	// the readers are asserting.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 1; !stop.Load(); k++ {
			st.Put("X", base.Add(-time.Duration(k)*time.Minute), flatSurface(-1))
		}
	}()

	time.Sleep(2 * time.Second)
	stop.Store(true)
	wg.Wait()

	if n := wrong.Load(); n > 0 {
		t.Fatalf("%d of %d concurrent reads resolved a surface other than the one live at the "+
			"requested as_of. Store.Vol is reading the version array outside the read lock while "+
			"Put shifts it in place — that is a wrong implied vol reaching compute.Greeks and "+
			"revaluation, not a crash", n, reads.Load())
	}
	// NON-VACUITY: a reader loop that never ran, or a store that answered
	// ok=false throughout, would report zero wrong reads and prove nothing.
	if n := reads.Load(); n < 100_000 {
		t.Fatalf("only %d concurrent reads completed in 2s — too few for the interleaving to be "+
			"exercised at all, so a zero wrong-read count here is not evidence", n)
	}
	if v, ok := st.Vol(ctx, "X", 100, 1, future); !ok || math.Abs(v-newest) > 1e-12 {
		t.Fatalf("after the run the store resolves %v (ok=%v), want %v — the readers were "+
			"asserting against an answer the store never gave", v, ok, newest)
	}
}

// flatSurface is a one-slice surface whose vol at the forward is √a — the
// cheapest way to give each published version an identity a reader can check
// without running the calibration.
func flatSurface(a float64) *SVISurface {
	return &SVISurface{
		expiries: []float64{1},
		forwards: []float64{100},
		slices:   []SVIParams{{A: a}},
	}
}
