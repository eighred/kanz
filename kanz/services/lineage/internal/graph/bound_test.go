package graph_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/lineage/internal/graph"
)

// THE DEFECT, PINNED (#244).
//
// Before the cap this loop left exactly 1,000,000 live map entries and
// DatasetOf("evt-0") still answered, because the file contained no delete( at
// all. On a ">" subscription that is the estate's entire event rate accumulating
// until the pod is OOMKilled, after which the durable consumer resumes at last
// ack and the graph is never rebuilt.
//
// A million observations run in well under a second here, so this is the real
// number the issue names rather than a stand-in.
func TestEventIndexIsBoundedUnderAMillionObservations(t *testing.T) {
	g := graph.NewMemory()
	now := time.Now()
	const n = 1_000_000
	for i := 0; i < n; i++ {
		g.Observe("evt-"+strconv.Itoa(i), ds("kanz.risk", "ExposureSet"), "risk",
			"risk.v1.ExposureSet:1", now, "")
	}

	cov := g.Coverage()
	if cov.Retained != graph.DefaultEventIndexCapacity {
		t.Errorf("retained = %d after %d observations, want the cap %d",
			cov.Retained, n, graph.DefaultEventIndexCapacity)
	}
	if cov.Retained > cov.Capacity {
		t.Errorf("retained %d exceeds capacity %d", cov.Retained, cov.Capacity)
	}
	if want := uint64(n - graph.DefaultEventIndexCapacity); cov.Evicted != want {
		t.Errorf("evicted = %d, want %d", cov.Evicted, want)
	}

	// The oldest is gone and the newest is kept: eviction is insertion-ordered.
	if _, look := g.DatasetOf("evt-0"); look != graph.LookupUnknown {
		t.Errorf("DatasetOf(evt-0) = %q, want %q — the first of a million is still indexed",
			look, graph.LookupUnknown)
	}
	if _, look := g.DatasetOf("evt-" + strconv.Itoa(n-1)); look != graph.LookupRetained {
		t.Errorf("DatasetOf(newest) = %q, want %q", look, graph.LookupRetained)
	}
}

// The bound does not depend on how many events arrive. Retained is min(distinct,
// capacity) because the ring is allocated to capacity and thereafter every
// insert OVERWRITES a slot — there is no path that grows it. Proven at three
// wildly different N against one small cap; the million-event case above is the
// same code path with the shipped cap.
func TestBoundIsIndependentOfHowManyEventsArrive(t *testing.T) {
	const cap = 1000
	for _, n := range []int{100, 5_000, 1_000_000} {
		g := graph.NewMemory(graph.WithEventIndexCapacity(cap))
		now := time.Now()
		for i := 0; i < n; i++ {
			g.Observe("e"+strconv.Itoa(i), ds("kanz.x", "A"), "x", "", now, "")
		}
		want := n
		if want > cap {
			want = cap
		}
		if got := g.Coverage().Retained; got != want {
			t.Errorf("n=%d: retained = %d, want %d", n, got, want)
		}
	}
}

// Re-observing an event must not consume a second ring slot, or at-least-once
// redelivery would evict the index out from under itself: len(ring) and
// len(eventDS) have to stay equal. Observed indirectly — a cap-sized set of ids
// replayed many times evicts nothing.
func TestRedeliveryDoesNotConsumeSlots(t *testing.T) {
	const cap = 64
	g := graph.NewMemory(graph.WithEventIndexCapacity(cap))
	now := time.Now()
	for round := 0; round < 50; round++ {
		for i := 0; i < cap; i++ {
			g.Observe("e"+strconv.Itoa(i), ds("kanz.x", "A"), "x", "", now, "")
		}
	}
	cov := g.Coverage()
	if cov.Evicted != 0 {
		t.Errorf("evicted = %d after replaying the same %d ids, want 0", cov.Evicted, cap)
	}
	if cov.Retained != cap {
		t.Errorf("retained = %d, want %d", cov.Retained, cap)
	}
	if _, look := g.DatasetOf("e0"); look != graph.LookupRetained {
		t.Errorf("e0 = %q after redelivery, want retained", look)
	}
}

// THE HALF THAT MATTERS. A bounded index that reports a miss as "not found" is a
// provenance service that lies with the confidence of an answer. The default
// graph — the deployed shape, whose consumer resumes at last ack — can never
// say "never observed".
func TestAMissOnABoundedIndexIsUnknownNotNeverObserved(t *testing.T) {
	g := graph.NewMemory()
	g.Observe("seen", ds("kanz.x", "A"), "x", "", time.Now(), "")

	if _, look := g.DatasetOf("never-published-by-anyone"); look != graph.LookupUnknown {
		t.Errorf("miss on a graph that does not cover history = %q, want %q: "+
			"'I have no record' was reported as 'there is no record'", look, graph.LookupUnknown)
	}
	if cov := g.Coverage(); cov.Complete {
		t.Error("a graph built by a last-ack-resuming consumer must never report complete coverage")
	}
}

// ...and the state is reachable, or the distinction would be decoration. A graph
// declared to have observed all of history answers a miss conclusively.
func TestACompleteGraphCanSayNeverObserved(t *testing.T) {
	g := graph.NewMemory(graph.WithCompleteHistory())
	g.Observe("seen", ds("kanz.x", "A"), "x", "", time.Now(), "")

	if _, look := g.DatasetOf("absent"); look != graph.LookupNotObserved {
		t.Errorf("miss on a complete graph = %q, want %q", look, graph.LookupNotObserved)
	}
	if !g.Coverage().Complete {
		t.Error("coverage.Complete = false on a complete graph that has evicted nothing")
	}
}

// The declaration is not a licence: the first eviction retires it. A graph that
// once held all of history and has since dropped part of it cannot tell an
// evicted id from an absent one, and must stop claiming it can.
func TestTheFirstEvictionRetiresCompleteness(t *testing.T) {
	g := graph.NewMemory(graph.WithCompleteHistory(), graph.WithEventIndexCapacity(2))
	now := time.Now()
	g.Observe("a", ds("kanz.x", "A"), "x", "", now, "")
	g.Observe("b", ds("kanz.x", "A"), "x", "", now, "")
	if _, look := g.DatasetOf("absent"); look != graph.LookupNotObserved {
		t.Fatalf("before eviction: %q, want %q", look, graph.LookupNotObserved)
	}

	g.Observe("c", ds("kanz.x", "A"), "x", "", now, "") // evicts "a"

	if cov := g.Coverage(); cov.Complete || cov.Evicted != 1 {
		t.Errorf("coverage after one eviction = %+v, want complete=false evicted=1", cov)
	}
	if _, look := g.DatasetOf("a"); look != graph.LookupUnknown {
		t.Errorf("the evicted id = %q, want %q", look, graph.LookupUnknown)
	}
	if _, look := g.DatasetOf("absent"); look != graph.LookupUnknown {
		t.Errorf("an absent id = %q, want %q — eviction makes EVERY miss inconclusive, "+
			"because the index cannot tell which kind it is", look, graph.LookupUnknown)
	}
}

// An eviction can cost an EDGE as well as an entry, which shortens an upstream
// list that otherwise looks complete. That has to be counted, or a truncated
// answer is indistinguishable from a root.
func TestAnEvictedCauseIsCountedNotShruggedOff(t *testing.T) {
	g := graph.NewMemory(graph.WithEventIndexCapacity(1))
	now := time.Now()
	g.Observe("parent", ds("kanz.market", "MarketDataEvent"), "market", "", now, "")
	// Evicts "parent", so the edge below can never be drawn.
	g.Observe("filler", ds("kanz.x", "Filler"), "x", "", now, "")
	g.Observe("child", ds("kanz.risk", "ExposureSet"), "risk", "", now, "parent")

	if up := g.Upstream(ds("kanz.risk", "ExposureSet")); len(up) != 0 {
		t.Fatalf("upstream = %v, want empty (the cause was evicted)", up)
	}
	if got := g.Coverage().UnlinkedCauses; got != 1 {
		t.Errorf("UnlinkedCauses = %d, want 1: a lost edge left no trace, so a truncated "+
			"upstream reads as a root", got)
	}
}

// Horizon tells an operator roughly how far back the index still answers. It is
// the event_time of the most recently evicted entry, and is ABSENT — not a zero
// time — until the first eviction: "nothing has been dropped" must not serialize
// as "dropped something dated the year 1", which is what a plain time.Time with
// omitempty puts on the wire.
func TestHorizonIsAbsentUntilSomethingIsEvicted(t *testing.T) {
	g := graph.NewMemory(graph.WithEventIndexCapacity(2))
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	g.Observe("a", ds("kanz.x", "A"), "x", "", base, "")
	g.Observe("b", ds("kanz.x", "A"), "x", "", base.Add(time.Minute), "")

	if h := g.Coverage().Horizon; h != nil {
		t.Errorf("horizon = %v before anything was evicted, want absent", h)
	}
	if body, err := json.Marshal(g.Coverage()); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(body), "horizon") {
		t.Errorf("an un-evicted index put a horizon on the wire: %s", body)
	}

	g.Observe("c", ds("kanz.x", "A"), "x", "", base.Add(2*time.Minute), "") // evicts "a"
	got := g.Coverage().Horizon
	if got == nil {
		t.Fatal("horizon absent after an eviction")
	}
	if !got.Equal(base) {
		t.Errorf("horizon = %v, want %v (the evicted entry's event_time)", got, base)
	}
}

// A capacity of zero is a config error, not a mode. Silently treating it as
// unbounded would put the OOMKill back one env var away.
func TestZeroCapacityRefusesToConstruct(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewMemory(WithEventIndexCapacity(0)) returned a graph instead of refusing")
		}
	}()
	graph.NewMemory(graph.WithEventIndexCapacity(0))
}
