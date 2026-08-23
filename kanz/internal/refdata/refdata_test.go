package refdata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource is a scripted Source. It counts calls per instrument so the tests
// can assert the cache does NOT re-ask for something it has already been told
// about — the property that keeps one unknown instrument from becoming a
// permanent request loop against datamaster.
type fakeSource struct {
	mu      sync.Mutex
	recs    map[string]Record
	errs    map[string]error
	calls   map[string]int
	order   []string
	blocked chan struct{} // when non-nil, Fetch waits on it before returning
}

func newFakeSource() *fakeSource {
	return &fakeSource{recs: map[string]Record{}, errs: map[string]error{}, calls: map[string]int{}}
}

func (f *fakeSource) Fetch(ctx context.Context, id string) (Record, bool, error) {
	f.mu.Lock()
	f.calls[id]++
	f.order = append(f.order, id)
	err, hasErr := f.errs[id]
	rec, found := f.recs[id]
	blocked := f.blocked
	f.mu.Unlock()

	if blocked != nil {
		select {
		case <-blocked:
		case <-ctx.Done():
			return Record{}, false, ctx.Err()
		}
	}
	if hasErr {
		return Record{}, false, err
	}
	return rec, found, nil
}

func (f *fakeSource) put(id string, rec Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec.InstrumentID = id
	f.recs[id] = rec
}

func (f *fakeSource) fail(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[id] = err
}

func (f *fakeSource) callCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

func (f *fakeSource) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

// clock is a hand-wound time source. Reference-data staleness is measured in
// hours, and a test that actually slept them would not run.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testCache builds a cache on a pinned clock with short, legible bounds.
func testCache(t *testing.T, src Source, mutate func(*Options)) (*Cache, *clock) {
	t.Helper()
	clk := newClock()
	opts := Options{
		RefreshTTL:       time.Minute,
		MaxAge:           10 * time.Minute,
		IdleTTL:          30 * time.Minute,
		MaxEntries:       100,
		MaxFetchPerCycle: 50,
		Now:              clk.now,
	}
	if mutate != nil {
		mutate(&opts)
	}
	c, err := NewCache(src, opts)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	return c, clk
}

func TestAMissIsRecordedAsAWantAndTheNextRefreshResolvesIt(t *testing.T) {
	src := newFakeSource()
	src.put("AAPL", Record{AssetClass: "EQUITY", Sector: Sector{Taxonomy: "GICS", Code: "45"}, IssuerID: "LEI-APPLE"})
	c, _ := testCache(t, src, nil)

	// A COLD CACHE REFUSES. This is the state a freshly started pod is in, and it
	// must be a refusal rather than a confident empty classification.
	if _, ok := c.Lookup("AAPL", time.Time{}); ok {
		t.Fatal("a cold cache answered a lookup — it holds nothing, so ok must be false")
	}
	if got := c.Stats(); got.Wanted != 1 || got.Resolved != 0 {
		t.Fatalf("after a miss: Stats = %+v, want Wanted=1 Resolved=0", got)
	}

	n, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if n != 1 {
		t.Fatalf("Refresh installed %d, want 1", n)
	}

	rec, ok := c.Lookup("AAPL", time.Time{})
	if !ok {
		t.Fatal("the instrument was fetched and the cache still refuses it")
	}
	if rec.Sector.Key() != "GICS:45" || rec.IssuerID != "LEI-APPLE" || rec.AssetClass != "EQUITY" {
		t.Fatalf("resolved record = %+v, want the fetched classification", rec)
	}
	if got := c.Stats(); got.Wanted != 0 || got.Resolved != 1 {
		t.Fatalf("after a refresh: Stats = %+v, want Wanted=0 Resolved=1", got)
	}
}

func TestARecordPastMaxAgeStopsBeingServed(t *testing.T) {
	src := newFakeSource()
	src.put("AAPL", Record{AssetClass: "EQUITY", Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	c, clk := testCache(t, src, nil)

	c.Lookup("AAPL", time.Time{})
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := c.Lookup("AAPL", time.Time{}); !ok {
		t.Fatal("a freshly fetched record must be served")
	}

	// The bound is MaxAge, not RefreshTTL: between the two the record is stale
	// enough to re-fetch and still good enough to answer with, which is what
	// stops one failed cycle from refusing every sector mandate on the estate.
	clk.advance(2 * time.Minute) // past RefreshTTL (1m), inside MaxAge (10m)
	if _, ok := c.Lookup("AAPL", time.Time{}); !ok {
		t.Fatal("a record past RefreshTTL but inside MaxAge must still be served — otherwise one " +
			"missed cycle is a total outage of every classified dimension")
	}

	clk.advance(10 * time.Minute) // now past MaxAge
	if _, ok := c.Lookup("AAPL", time.Time{}); ok {
		t.Fatal("a record past MaxAge was served — a sustained datamaster outage must degrade to " +
			"refusals, not to a classification nobody has confirmed")
	}
	if got := c.Stats(); got.Stale != 1 || got.Resolved != 0 {
		t.Fatalf("past MaxAge: Stats = %+v, want Stale=1 Resolved=0 — a stale entry is invisible in "+
			"Resolved and must still be counted somewhere", got)
	}
}

func TestAsOfNeverGuessesBackwards(t *testing.T) {
	snapshot := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	src := newFakeSource()
	src.put("AAPL", Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}, AsOf: snapshot})
	src.put("UNDATED", Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	c, _ := testCache(t, src, nil)

	c.Lookup("AAPL", time.Time{})
	c.Lookup("UNDATED", time.Time{})
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   string
		asOf time.Time
		want bool
		why  string
	}{
		{"zero asOf means now", "AAPL", time.Time{}, true,
			"the live question, which the latest snapshot always answers"},
		{"asOf after the snapshot", "AAPL", snapshot.Add(24 * time.Hour), true,
			"the snapshot was already in effect at the asked-for time"},
		{"asOf exactly at the snapshot", "AAPL", snapshot, true,
			"the snapshot is effective AT its as_of, so the boundary answers"},
		{"asOf before the snapshot", "AAPL", snapshot.Add(-time.Second), false,
			"the master holds one snapshot; serving it for an earlier time would apply a " +
				"reclassification that had not happened yet"},
		{"undated record, live question", "UNDATED", time.Time{}, true,
			"an undated record still answers the now question"},
		{"undated record, point-in-time question", "UNDATED", snapshot, true,
			"an undated record makes no claim about when it was effective, so it cannot be shown " +
				"to be wrong for a given time. Refusing it would refuse EVERY evaluation the " +
				"estate makes — the compliance engine always passes the evaluation time as " +
				"Candidate.AsOf — over a vendor extract that lacked a date column, and would " +
				"report it as missing reference data that is in fact loaded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := c.Lookup(tc.id, tc.asOf); ok != tc.want {
				t.Fatalf("Lookup(%s, %v) ok = %v, want %v — %s", tc.id, tc.asOf, ok, tc.want, tc.why)
			}
		})
	}
}

func TestANotFoundEvictsAndIsNotAskedAgainUntilSomebodyAsks(t *testing.T) {
	src := newFakeSource()
	src.put("AAPL", Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	c, clk := testCache(t, src, nil)

	c.Lookup("AAPL", time.Time{})
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := c.Lookup("AAPL", time.Time{}); !ok {
		t.Fatal("setup: the record should be resident")
	}

	// The master withdraws it. The clock has to move past RefreshTTL for the
	// resident entry to be RE-ASKED — a withdrawal is only ever discovered by the
	// cycle that re-fetches, which is itself the reason MaxAge exists as a second
	// bound underneath it.
	src.mu.Lock()
	delete(src.recs, "AAPL")
	src.mu.Unlock()
	clk.advance(2 * time.Minute)

	c.Lookup("DELISTED", time.Time{}) // an id the master never had
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if _, ok := c.Lookup("AAPL", time.Time{}); ok {
		t.Fatal("a record the master no longer holds is still being classified from")
	}

	// The unknown id must not become a permanent per-cycle request. The Lookup
	// above re-wants it, so allow exactly that one extra call.
	before := src.callCount("DELISTED")
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if after := src.callCount("DELISTED"); after > before+1 {
		t.Fatalf("DELISTED fetched %d times across two cycles (was %d) — a 404 must clear the want, "+
			"or one unknown instrument is a permanent request loop at datamaster", after, before)
	}
}

func TestAFetchErrorKeepsTheResidentRecordAndIsReported(t *testing.T) {
	src := newFakeSource()
	src.put("AAPL", Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	src.put("MSFT", Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	c, clk := testCache(t, src, nil)

	c.Lookup("AAPL", time.Time{})
	c.Lookup("MSFT", time.Time{})
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	firstOK := c.Stats().LastRefresh

	src.fail("AAPL", errors.New("datamaster answered 503"))
	clk.advance(2 * time.Minute) // both entries are now past RefreshTTL

	n, err := c.Refresh(context.Background())
	if err == nil {
		t.Fatal("a cycle with a failed lookup reported success — a partial cycle must be visible")
	}
	if !strings.Contains(err.Error(), "1 of 2") {
		t.Fatalf("error %q does not say how much of the cycle failed", err)
	}
	// THE CYCLE IS NOT ALL-OR-NOTHING: MSFT was fetchable and must have been
	// installed, or one sick instrument refuses every mandate on the book.
	if n != 1 {
		t.Fatalf("installed %d despite a partial failure, want 1 (MSFT)", n)
	}
	if _, ok := c.Lookup("AAPL", time.Time{}); !ok {
		t.Fatal("a fetch ERROR withdrew a resident classification — only a 404 may do that, " +
			"because \"could not ask\" and \"the master does not hold it\" are different facts")
	}
	if got := c.Stats().LastRefresh; !got.Equal(firstOK) {
		t.Fatalf("LastRefresh advanced to %v across a failed cycle (was %v) — the gauge that says "+
			"reference data is current must not be set by a cycle that was not", got, firstOK)
	}
}

func TestAnIdleRecordIsEvictedSoTheResidentSetTracksTheBook(t *testing.T) {
	src := newFakeSource()
	src.put("AAPL", Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	src.put("MSFT", Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	c, clk := testCache(t, src, nil)

	c.Lookup("AAPL", time.Time{})
	c.Lookup("MSFT", time.Time{})
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	clk.advance(20 * time.Minute) // inside IdleTTL (30m)
	c.Lookup("AAPL", time.Time{}) // AAPL stays in demand; MSFT does not
	clk.advance(20 * time.Minute) // MSFT now idle for 40m, AAPL for 20m

	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// AAPL was re-fetched by this cycle (it was past RefreshTTL) and remains
	// answerable; MSFT was evicted as idle, so it is a miss again.
	if _, ok := c.Lookup("AAPL", time.Time{}); !ok {
		t.Fatal("an instrument still being asked about was evicted")
	}
	if got := c.Stats(); got.Resolved != 1 {
		t.Fatalf("Stats = %+v, want Resolved=1 — the resident set must converge on the active book", got)
	}
}

func TestOneCycleIsBoundedAndFetchesWantsBeforeStaleResidents(t *testing.T) {
	src := newFakeSource()
	for i := range 10 {
		src.put(fmt.Sprintf("W%02d", i), Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	}
	c, _ := testCache(t, src, func(o *Options) { o.MaxFetchPerCycle = 4 })

	for i := 9; i >= 0; i-- { // asked out of order on purpose
		c.Lookup(fmt.Sprintf("W%02d", i), time.Time{})
	}
	n, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if n != 4 {
		t.Fatalf("installed %d, want 4 — MaxFetchPerCycle must bound one cycle so a cold start "+
			"against a large book is not a single burst at datamaster", n)
	}
	// DETERMINISTIC, not map-ordered: two replicas evaluating the same book must
	// come to know the same instruments, or a mandate passes on one pod and
	// refuses on the other.
	got := src.fetched()
	want := []string{"W00", "W01", "W02", "W03"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cycle fetched %v, want the sorted prefix %v — a truncated cycle must be "+
				"deterministic across replicas", got, want)
		}
	}
}

func TestTheResidentSetAndTheWantSetAreBothBoundedAndOverflowIsCounted(t *testing.T) {
	src := newFakeSource()
	for i := range 8 {
		src.put(fmt.Sprintf("I%02d", i), Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	}
	c, _ := testCache(t, src, func(o *Options) { o.MaxEntries = 3; o.MaxFetchPerCycle = 50 })

	for i := range 8 {
		c.Lookup(fmt.Sprintf("I%02d", i), time.Time{})
	}
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	got := c.Stats()
	if got.Resolved != 3 {
		t.Fatalf("Resolved = %d, want 3 — MaxEntries must bound the resident set", got.Resolved)
	}
	if got.Overflowed == 0 {
		t.Fatal("the cache refused admissions and counted none — a cache smaller than the working " +
			"set refuses dimensions for a reason that is NOT a reference-data gap, and an " +
			"operator cannot tell the two apart without this counter")
	}
}

func TestConstructionRefusesConfigurationsThatWouldRefuseEverything(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  Source
		opts Options
		want string
	}{
		{"a nil source", nil, Options{}, "needs a Source"},
		{"MaxAge inside RefreshTTL", newFakeSource(),
			Options{RefreshTTL: time.Hour, MaxAge: time.Minute, IdleTTL: time.Hour}, "must exceed RefreshTTL"},
		{"IdleTTL below MaxAge", newFakeSource(),
			Options{RefreshTTL: time.Minute, MaxAge: time.Hour, IdleTTL: time.Minute}, "at least MaxAge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCache(tc.src, tc.opts)
			if err == nil {
				t.Fatal("accepted a configuration that would refuse every classified dimension")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the problem (%q)", err, tc.want)
			}
		})
	}
}

func TestTheDefaultsAreSelfConsistent(t *testing.T) {
	// The defaults are what almost every deployment runs on, so the relationship
	// the constructor enforces must hold for them without anyone passing Options.
	if _, err := NewCache(newFakeSource(), Options{}); err != nil {
		t.Fatalf("the package defaults are not a valid configuration: %v", err)
	}
}

// TestLookupDoesNotBlockBehindARefresh is the property the whole design exists
// for: the compliance rule evaluators call Classify with context.TODO, so a
// Lookup that waited on datamaster would be an unbounded, uncancellable stall
// on the order-admission path.
func TestLookupDoesNotBlockBehindARefresh(t *testing.T) {
	src := newFakeSource()
	src.put("AAPL", Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	release := make(chan struct{})
	src.blocked = release
	c, _ := testCache(t, src, nil)

	c.Lookup("AAPL", time.Time{}) // want it, so the cycle has work

	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		_, _ = c.Refresh(context.Background())
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			c.Lookup("AAPL", time.Time{})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Lookup blocked while a Refresh was in flight — I/O must happen outside the lock, " +
			"or a slow datamaster stalls every order evaluation in the process")
	}
	close(release)
	<-refreshed
}

// TestConcurrentLookupAndRefreshShareOneMap exercises the mutex under load. It
// proves no panic and no lost update; the DATA RACE itself is only provable
// under -race, which needs cgo and does not run on the usual Windows box — CI
// is the detector for that half.
func TestConcurrentLookupAndRefreshShareOneMap(t *testing.T) {
	src := newFakeSource()
	for i := range 32 {
		src.put(fmt.Sprintf("I%02d", i), Record{Sector: Sector{Taxonomy: "GICS", Code: "45"}})
	}
	c, _ := testCache(t, src, nil)

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 200 {
				c.Lookup(fmt.Sprintf("I%02d", (w*7+i)%32), time.Time{})
				c.Stats()
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 20 {
				if _, err := c.Refresh(context.Background()); err != nil {
					t.Errorf("Refresh: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()

	if got := c.Stats(); got.Resolved == 0 {
		t.Fatalf("Stats = %+v: nothing resolved after 80 concurrent refresh cycles", got)
	}
}

func TestAnEmptyInstrumentIDIsRefusedWithoutBecomingAWant(t *testing.T) {
	c, _ := testCache(t, newFakeSource(), nil)
	if _, ok := c.Lookup("", time.Time{}); ok {
		t.Fatal("an empty instrument id resolved")
	}
	if got := c.Stats().Wanted; got != 0 {
		t.Fatalf("Wanted = %d after an empty-id lookup, want 0 — an unkeyable position must not "+
			"put an unfetchable entry in the cycle's work list", got)
	}
}
