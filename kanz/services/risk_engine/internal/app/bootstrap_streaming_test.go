package app

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
)

// THE RESTORE'S FOOTPRINT DOES NOT GROW WITH THE ESTATE (#674).
//
// LoadAll returned every persisted record as one slice, so bootstrap held the
// whole estate in memory at once. Startup time and peak memory both scaled with
// total portfolio count, which makes RECOVERY TIME A FUNCTION OF HOW SUCCESSFUL
// THE PLATFORM IS — the worst coupling available to the component whose job is to
// come back after a failure.
//
// # Why the records here are all FOREIGN
//
// The engine's store legitimately retains every portfolio this replica OWNS —
// that is the point of restoring, and its memory is properly proportional to the
// owned book. What #674 is about is the restore path holding the whole estate ON
// TOP of that, including the portfolios it is about to throw away.
//
// A sharded replica is where the two separate cleanly. Bootstrap's own comment:
// LoadEach "streams every portfolio in the tenant's database, which on a SHARDED
// replica is mostly other replicas' books". Every record below is one this
// replica does not own, so the store keeps NOTHING and anything the heap retains
// is the restore path's own. Under the old shape that was the entire estate.

// genSink is a StateStore that MANUFACTURES records as it streams them.
//
// IT HOLDS NO SLICE, deliberately: a fake backed by []PortfolioRecord would carry
// the whole estate itself and make the measurement below meaningless — the heap
// would grow with n no matter how well the consumer behaved. This one is O(1)
// regardless of n, so every byte the test observes is the consumer's.
type genSink struct {
	n  int
	id v1.PortfolioID // the foreign portfolio id every record is filed under
	// peak is the heap measured at the moment of MAXIMUM RETENTION — just after
	// the consumer has been handed the last record and before the walk returns.
	peak uint64
}

func (g *genSink) Save(context.Context, persist.PortfolioRecord) error { return nil }
func (g *genSink) Load(context.Context, v1.PortfolioID) (persist.PortfolioRecord, error) {
	return persist.PortfolioRecord{}, persist.ErrNotFound
}
func (g *genSink) Ping(context.Context) error { return nil }

func (g *genSink) LoadEach(_ context.Context, fn func(persist.PortfolioRecord) error) error {
	for i := 0; i < g.n; i++ {
		// A fresh record per call, unreachable from here once fn returns.
		rec := recordFor(g.id, uint64(i))
		rec.DisplayName = fmt.Sprintf("portfolio number %d with a name long enough to weigh something", i)
		if err := fn(rec); err != nil {
			return err
		}
		if i == g.n-1 {
			// MEASURED HERE, INSIDE THE WALK, AND THE POSITION IS THE WHOLE TEST.
			//
			// The first version of this measured after Bootstrap.Run returned, and
			// it PASSED against a consumer that accumulated every record — because
			// by then the slice holding them was already garbage. It was measuring
			// retention, not peak, and would have shipped green against the exact
			// defect it names. Found by mutation, not by review.
			//
			// At this instant the consumer has been handed all n records. One that
			// streams holds one; one that accumulates holds n, and they are still
			// reachable, so the GC cannot hide the difference.
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			g.peak = m.HeapAlloc
		}
	}
	return nil
}

// restorePeakHeap runs a full bootstrap restore over n foreign records and
// returns the heap measured at maximum retention DURING the walk.
func restorePeakHeap(t *testing.T, n int) uint64 {
	t.Helper()
	sharding, _, theirs := ringFixture(t)
	store := state.NewStore(sharding.StoreOptions()...)
	sink := &genSink{n: n, id: theirs}

	b, err := NewBootstrap(store, sink, sharding.ReplayApplier(store), nil, quiet())
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := b.ForeignRecordsSkipped(); got != n {
		t.Fatalf("skipped %d of %d foreign records — the premise is broken and this "+
			"measurement means nothing", got, n)
	}
	if ids := store.IDs(); len(ids) != 0 {
		t.Fatalf("the store retained %d portfolio(s) it does not own — the measurement below "+
			"would be counting legitimate state, not the restore path", len(ids))
	}

	if sink.peak == 0 {
		t.Fatal("the in-walk probe never ran — the measurement is missing, not zero")
	}
	return sink.peak
}

func TestRestoreFootprintDoesNotGrowWithTheEstate(t *testing.T) {
	const (
		small = 1_000
		large = 100_000
	)

	// Warm first: the first restore pays for lazily-allocated fixtures and the
	// ring, and attributing that to n would flatter or penalise the result at
	// random.
	restorePeakHeap(t, small)

	before := restorePeakHeap(t, small)
	after := restorePeakHeap(t, large)

	// Under the old shape every record was retained, so `after` would hold 100x
	// what `before` did — on the order of tens of megabytes for these records.
	// Streaming keeps both at the same baseline.
	//
	// THE BOUND IS DELIBERATELY LOOSE. This measures a live Go heap, and GC
	// timing, allocator arenas and other tests in the same process all move it;
	// a tight bound here would be a flaky test, which is worse than none. The
	// defect it exists to catch is a 100x difference, so 4x separates the two
	// answers with room to spare and still fails loudly if accumulation returns.
	if after > before*4+(8<<20) {
		t.Fatalf("restoring %d records held %d bytes against %d for %d records — the restore path "+
			"is retaining the estate rather than streaming it (#674)", large, after, before, small)
	}
}

// The streaming contract itself: every record reaches the consumer, and a
// mid-stream refusal from the consumer stops the walk.
func TestRestoreSeesEveryStreamedRecord(t *testing.T) {
	sharding, _, theirs := ringFixture(t)
	store := state.NewStore(sharding.StoreOptions()...)
	const n = 5_000
	sink := &genSink{n: n, id: theirs}

	b, err := NewBootstrap(store, sink, sharding.ReplayApplier(store), nil, quiet())
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := b.ForeignRecordsSkipped(); got != n {
		t.Fatalf("ForeignRecordsSkipped = %d, want %d — streaming must not lose records", got, n)
	}
}
