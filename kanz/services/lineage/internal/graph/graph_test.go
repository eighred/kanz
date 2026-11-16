package graph

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func ds(ns, name string) DatasetID { return DatasetID{Namespace: ns, Name: name} }

func TestUpstreamIsTransitiveAndDeterministic(t *testing.T) {
	g := newGraph(t)
	now := time.Now()
	// e1 (market.MarketDataEvent) → e2 (risk.ExposureSet) → e3 (risk.Measures)
	mustObserve(t, g, "e1", ds("kanz.market", "MarketDataEvent"), "market", "market.v1.MarketDataEvent:1", now, "")
	mustObserve(t, g, "e2", ds("kanz.risk", "ExposureSet"), "risk", "risk.v1.ExposureSet:1", now, "e1")
	mustObserve(t, g, "e3", ds("kanz.risk", "Measures"), "risk", "risk.v1.Measures:1", now, "e2")

	got := g.Upstream("acme", ds("kanz.risk", "Measures"))
	want := []DatasetID{ds("kanz.market", "MarketDataEvent"), ds("kanz.risk", "ExposureSet")}
	ids := make([]DatasetID, len(got))
	for i := range got {
		ids[i] = got[i].ID
	}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("Upstream = %v, want %v", got, want)
	}
}

func TestDatasetOfAndSelfLoopSkipped(t *testing.T) {
	g := newGraph(t)
	now := time.Now()
	mustObserve(t, g, "s1", ds("kanz.risk", "PortfolioState"), "risk", "risk.v1.PortfolioState:1", now, "")
	// A snapshot of the same entity, caused by the prior state event — a self
	// loop, not provenance.
	mustObserve(t, g, "s2", ds("kanz.risk", "PortfolioState"), "risk", "risk.v1.PortfolioState:1", now, "s1")

	if got, look := g.DatasetOf("acme", "s2"); look != LookupRetained || got != ds("kanz.risk", "PortfolioState") {
		t.Errorf("DatasetOf(s2) = %v,%v", got, look)
	}
	if up := g.Upstream("acme", ds("kanz.risk", "PortfolioState")); len(up) != 0 {
		t.Errorf("self-loop should not be provenance, got upstream %v", up)
	}
}

func TestUpstreamCycleSafe(t *testing.T) {
	g := newGraph(t)
	now := time.Now()
	// Construct a malformed cycle a→b→a across two datasets.
	mustObserve(t, g, "a", ds("kanz.x", "A"), "x", "", now, "")
	mustObserve(t, g, "b", ds("kanz.x", "B"), "x", "", now, "a")
	mustObserve(t, g, "a2", ds("kanz.x", "A"), "x", "", now, "b") // A now also derived from B
	// Upstream(A) must terminate (not loop) and include B.
	up := g.Upstream("acme", ds("kanz.x", "A"))
	if len(up) != 1 || up[0].ID != ds("kanz.x", "B") {
		t.Errorf("Upstream(A) = %v, want [kanz.x.B]", up)
	}
}

func TestDatasetsCatalogListing(t *testing.T) {
	g := newGraph(t)
	now := time.Now()
	mustObserve(t, g, "e1", ds("kanz.risk", "ExposureSet"), "risk", "risk.v1.ExposureSet:2", now, "")
	mustObserve(t, g, "e2", ds("kanz.risk", "ExposureSet"), "risk", "risk.v1.ExposureSet:2", now.Add(time.Hour), "")
	list := g.Datasets("acme")
	if len(list) != 1 || list[0].Events != 2 || list[0].SchemaRef != "risk.v1.ExposureSet:2" {
		t.Fatalf("catalog = %+v", list)
	}
	if !list[0].LastSeen.Equal(now.Add(time.Hour)) {
		t.Errorf("LastSeen = %v, want latest", list[0].LastSeen)
	}
}

func fixtureLimits() Limits {
	return Limits{EventIndexMax: DefaultEventIndexCapacity, DatasetMax: 128, EdgeMax: 512, StringBytesMax: 64 << 20}
}
func newGraph(t *testing.T, opts ...Option) *Memory {
	t.Helper()
	g, err := NewMemory(fixtureLimits(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func fixtureObservation(id string, dataset DatasetID, domain, schema string, at time.Time, cause string) Observation {
	if schema == "" {
		schema = "fixture.v1." + dataset.Name + ":1"
	}
	return Observation{Owner: "acme", EventID: id, Dataset: dataset, Domain: domain, SchemaRef: schema,
		At: at, ParentEventID: cause, Source: domain, CorrelationID: "corr-" + id}
}
func mustObserve(t *testing.T, g *Memory, id string, dataset DatasetID, domain, schema string, at time.Time, cause string) ObservationSnapshot {
	t.Helper()
	snapshot, err := g.Observe(fixtureObservation(id, dataset, domain, schema, at, cause))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

type capturedState struct {
	datasets          map[datasetKey]datasetRecord
	upstream          map[datasetKey]map[datasetKey]struct{}
	events            map[eventKey]indexEntry
	ring              []indexEntry
	next              int
	limits            Limits
	edgeCount         int
	stringBytes       uint64
	evicted, unlinked uint64
	since             time.Time
	completeOwner     Owner
}

func capture(g *Memory) capturedState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	s := capturedState{datasets: map[datasetKey]datasetRecord{}, upstream: map[datasetKey]map[datasetKey]struct{}{},
		events: map[eventKey]indexEntry{}, next: g.next, limits: g.limits, edgeCount: g.edgeCount, stringBytes: g.stringBytes,
		evicted: g.evicted, unlinked: g.unlinked, since: g.since, completeOwner: g.completeOwner}
	for k, v := range g.datasets {
		s.datasets[k] = *v
	}
	for k, edges := range g.upstream {
		s.upstream[k] = map[datasetKey]struct{}{}
		for p := range edges {
			s.upstream[k][p] = struct{}{}
		}
	}
	for k, v := range g.eventDS {
		s.events[k] = *v
	}
	for _, v := range g.ring {
		s.ring = append(s.ring, *v)
	}
	return s
}
func unchanged(t *testing.T, g *Memory, before capturedState) {
	t.Helper()
	if !reflect.DeepEqual(before, capture(g)) {
		t.Fatal("refused/duplicate/read operation changed retained graph state")
	}
}
func admitted(t *testing.T, g *Memory, o Observation) ObservationSnapshot {
	t.Helper()
	s, err := g.Observe(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func smallObservation() Observation {
	return Observation{Owner: "a", EventID: "i", Dataset: DatasetID{Namespace: "n", Name: "d"},
		Domain: "x", SchemaRef: "r", At: time.Unix(1, 0).UTC(), Source: "s", CorrelationID: "c"}
}
func TestGlobalAdmissionLimitsAreAtomic(t *testing.T) {
	t.Run("dataset-global-across-owners", func(t *testing.T) {
		l := fixtureLimits()
		l.DatasetMax = 1
		g, err := NewMemory(l)
		if err != nil {
			t.Fatal(err)
		}
		o := smallObservation()
		admitted(t, g, o)
		before := capture(g)
		o.Owner = "b"
		o.EventID = "j"
		if _, err := g.Observe(o); !errors.Is(err, ErrCapacity) {
			t.Fatalf("want capacity refusal, got %v", err)
		}
		unchanged(t, g, before)
	})
	t.Run("edge-and-new-node", func(t *testing.T) {
		l := fixtureLimits()
		l.EdgeMax = 1
		g, err := NewMemory(l)
		if err != nil {
			t.Fatal(err)
		}
		o := smallObservation()
		admitted(t, g, o)
		child := o
		child.EventID = "j"
		child.Dataset.Name = "e"
		child.ParentEventID = "i"
		child.At = child.At.Add(time.Second)
		admitted(t, g, child)
		before := capture(g)
		child.EventID = "k"
		child.Dataset.Name = "f"
		child.At = child.At.Add(time.Second)
		if _, err := g.Observe(child); !errors.Is(err, ErrCapacity) {
			t.Fatalf("want capacity refusal, got %v", err)
		}
		unchanged(t, g, before)
	})
	t.Run("string-exact-and-one-byte-short", func(t *testing.T) {
		// Independent literal accounting: key a/n/d=3; node x/r/i=3;
		// retained event i + empty cause + x/r/s/c=5. Total=11.
		for _, limit := range []uint64{10, 11} {
			l := fixtureLimits()
			l.StringBytesMax = limit
			g, err := NewMemory(l)
			if err != nil {
				t.Fatal(err)
			}
			before := capture(g)
			_, err = g.Observe(smallObservation())
			if limit == 10 {
				if !errors.Is(err, ErrCapacity) {
					t.Fatalf("limit10: %v", err)
				}
				unchanged(t, g, before)
			} else if err != nil || g.Stats().StringBytes != 11 {
				t.Fatalf("limit11: stats=%+v err=%v", g.Stats(), err)
			}
		}
	})
	t.Run("replacement-and-eviction-credit", func(t *testing.T) {
		// After replacing metadata with x/rr/jj, and evicting the first event:
		// key3 + metadata(1+2+2)=5 + projection(2+0+1+2+1+1)=7 =>15.
		for _, limit := range []uint64{14, 15} {
			l := fixtureLimits()
			l.EventIndexMax = 1
			l.StringBytesMax = limit
			g, err := NewMemory(l)
			if err != nil {
				t.Fatal(err)
			}
			o := smallObservation()
			admitted(t, g, o)
			before := capture(g)
			o.EventID = "jj"
			o.SchemaRef = "rr"
			o.At = o.At.Add(time.Second)
			_, err = g.Observe(o)
			if limit == 14 {
				if !errors.Is(err, ErrCapacity) {
					t.Fatalf("limit14: %v", err)
				}
				unchanged(t, g, before)
			} else if err != nil || g.Stats().StringBytes != 15 || g.Stats().Evicted != 1 {
				t.Fatalf("limit15: %+v %v", g.Stats(), err)
			}
		}
	})
}
func TestRetainedIdentityIsOwnedIdempotentAndConflictRefusing(t *testing.T) {
	g := newGraph(t)
	o := smallObservation()
	original := admitted(t, g, o)
	before := capture(g)
	again, err := g.Observe(o)
	if err != nil || again != original {
		t.Fatalf("retained duplicate changed snapshot: %v", err)
	}
	unchanged(t, g, before)
	mutations := map[string]func(*Observation){
		"dataset":     func(o *Observation) { o.Dataset.Name = "other" },
		"domain":      func(o *Observation) { o.Domain = "other" },
		"schema":      func(o *Observation) { o.SchemaRef = "other" },
		"time":        func(o *Observation) { o.At = o.At.Add(time.Nanosecond) },
		"cause":       func(o *Observation) { o.ParentEventID = "other" },
		"source":      func(o *Observation) { o.Source = "other" },
		"correlation": func(o *Observation) { o.CorrelationID = "other" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := o
			mutate(&changed)
			if _, err := g.Observe(changed); !errors.Is(err, ErrConflict) {
				t.Fatalf("conflict accepted: %v", err)
			}
			unchanged(t, g, before)
		})
	}
	// Adversarial storage-layer identity collision, NOT a valid ordinary pair of
	// globally unique canonical FACTs. The independent stored owner still wins.
	foreign := o
	foreign.Owner = "b"
	foreign.SchemaRef = "foreign"
	admitted(t, g, foreign)
	a, _ := g.Get("a", o.Dataset)
	b, _ := g.Get("b", o.Dataset)
	if a.Owner() != "a" || b.Owner() != "b" || a.SchemaRef != "r" || b.SchemaRef != "foreign" {
		t.Fatal("composite dataset ownership collapsed")
	}
}
func TestEvictedHistoryRefusesWithoutRecountAndIsOwnerScoped(t *testing.T) {
	g := newGraph(t, WithEventIndexCapacity(1))
	old := smallObservation()
	admitted(t, g, old)
	newer := old
	newer.EventID = "j"
	newer.At = newer.At.Add(time.Second)
	admitted(t, g, newer)
	before := capture(g)
	for _, at := range []time.Time{old.At, old.At.Add(-time.Nanosecond)} {
		late := old
		late.At = at
		if _, err := g.Observe(late); !errors.Is(err, ErrHistoryUnknown) {
			t.Fatalf("history floor bypass: %v", err)
		}
		unchanged(t, g, before)
	}
	late := old
	late.EventID = "legitimate-late"
	if _, err := g.Observe(late); !errors.Is(err, ErrHistoryUnknown) {
		t.Fatalf("unknown old input guessed healthy: %v", err)
	}
	unchanged(t, g, before)
	foreign := old
	foreign.Owner = "b"
	admitted(t, g, foreign)
	if c := g.Coverage("b"); c.Evicted != 0 || c.Horizon != nil || c.Retained != 1 {
		t.Fatalf("foreign coverage contaminated: %+v", c)
	}
	if c := g.Coverage("a"); c.Horizon == nil || !c.Horizon.Equal(newer.At) {
		t.Fatalf("owner floor was not max evicted time: %+v", c)
	}
}
func TestRetainedDuplicatePrecedesTheHistoryFloor(t *testing.T) {
	g := newGraph(t, WithEventIndexCapacity(2))
	o := smallObservation()
	o.At = time.Unix(10, 0)
	admitted(t, g, o)
	middle := o
	middle.EventID = "j"
	middle.At = time.Unix(1, 0)
	want := admitted(t, g, middle)
	latest := o
	latest.EventID = "k"
	latest.At = time.Unix(20, 0)
	admitted(t, g, latest)
	before := capture(g)
	got, err := g.Observe(middle)
	if err != nil || got != want {
		t.Fatalf("retained old-time duplicate refused/changed: %v", err)
	}
	unchanged(t, g, before)
}
func TestSnapshotDoesNotAcquireAParentOnRetry(t *testing.T) {
	g := newGraph(t)
	child := smallObservation()
	child.EventID = "child"
	child.ParentEventID = "parent"
	first := admitted(t, g, child)
	parent := child
	parent.EventID = "parent"
	parent.ParentEventID = ""
	parent.Dataset.Name = "parent"
	parent.At = parent.At.Add(-time.Second)
	admitted(t, g, parent)
	before := capture(g)
	again, err := g.Observe(child)
	if err != nil || first != again {
		t.Fatalf("duplicate parent view changed: %v", err)
	}
	if _, known := again.Input(); known {
		t.Fatal("later parent fabricated an original input")
	}
	unchanged(t, g, before)
	if g.Coverage("a").UnlinkedCauses != 1 {
		t.Fatal("retry recounted missing cause")
	}
}
func TestLatestMetadataUsesTimeAndIdentityOrdering(t *testing.T) {
	a := smallObservation()
	a.EventID = "a"
	a.SchemaRef = "older"
	b := a
	b.EventID = "b"
	b.SchemaRef = "winner"
	for _, order := range [][]Observation{{a, b}, {b, a}} {
		g := newGraph(t)
		for _, o := range order {
			admitted(t, g, o)
		}
		late := a
		late.EventID = "z"
		late.SchemaRef = "late"
		late.At = late.At.Add(-time.Nanosecond)
		admitted(t, g, late)
		info, _ := g.Get("a", a.Dataset)
		if info.SchemaRef != "winner" || !info.LastSeen.Equal(a.At) || info.Events != 3 {
			t.Fatalf("metadata rollback/tie dependence: %+v", info)
		}
	}
}
func TestUnknownOwnerReadsRetainNothing(t *testing.T) {
	g := newGraph(t)
	admitted(t, g, smallObservation())
	before := capture(g)
	for i := 0; i < 100; i++ {
		owner := Owner(fmt.Sprintf("unknown-%d", i))
		if _, ok := g.Get(owner, DatasetID{Namespace: "n", Name: "d"}); ok {
			t.Fatal("foreign Get disclosed")
		}
		if _, look := g.DatasetOf(owner, "i"); look != LookupUnknown {
			t.Fatal("foreign event disclosed")
		}
		if len(g.Datasets(owner)) != 0 || len(g.Upstream(owner, DatasetID{Namespace: "n", Name: "d"})) != 0 {
			t.Fatal("foreign topology disclosed")
		}
		c := g.Coverage(owner)
		if c.Retained != 0 || c.Evicted != 0 || c.Complete || c.Horizon != nil || c.UnlinkedCauses != 0 {
			t.Fatalf("foreign coverage: %+v", c)
		}
	}
	unchanged(t, g, before)
	if _, err := g.Observe(Observation{}); !errors.Is(err, ErrOwnerUnknown) {
		t.Fatalf("unknown owner admitted: %v", err)
	}
	unchanged(t, g, before)
}
func TestScopedCompleteHistoryCannotCertifyAnotherOwner(t *testing.T) {
	g := newGraph(t, WithCompleteHistory("a"))
	admitted(t, g, smallObservation())
	if _, look := g.DatasetOf("a", "absent"); look != LookupNotObserved {
		t.Fatal("explicit scoped finding disappeared")
	}
	if _, look := g.DatasetOf("b", "i"); look != LookupUnknown {
		t.Fatal("complete declaration escaped owner scope")
	}
	if g.Coverage("b").Complete {
		t.Fatal("foreign complete coverage fabricated")
	}
}
func TestAdmissionOverflowRefusesAtomically(t *testing.T) {
	for _, kind := range []string{"events", "unlinked", "evicted", "owner-evicted", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			g := newGraph(t, WithEventIndexCapacity(1))
			o := smallObservation()
			admitted(t, g, o)
			rec := g.datasets[datasetKey{Owner: "a", ID: o.Dataset}]
			switch kind {
			case "events":
				rec.info.Events = ^uint64(0)
			case "unlinked":
				g.unlinked = ^uint64(0)
			case "evicted":
				g.evicted = ^uint64(0)
			case "owner-evicted":
				rec.evicted = ^uint64(0)
			case "bytes":
				g.stringBytes = ^uint64(0)
				g.limits.StringBytesMax = ^uint64(0)
			}
			before := capture(g)
			o.EventID = "j"
			o.At = o.At.Add(time.Second)
			if kind == "unlinked" {
				o.ParentEventID = "absent"
			}
			if kind == "bytes" {
				o.Source = "longer"
			}
			if _, err := g.Observe(o); !errors.Is(err, ErrCapacity) {
				t.Fatalf("overflow did not refuse: %v", err)
			}
			unchanged(t, g, before)
		})
	}
}
func TestAdmissionClonesRetainedSubstringBacking(t *testing.T) {
	backing := strings.Repeat("x", 1<<20)
	part := backing[len(backing)-8:]
	o := smallObservation()
	o.Owner = Owner(part)
	o.EventID = part
	parent := backing[len(backing)-9 : len(backing)-1]
	o.ParentEventID = parent
	o.Dataset = DatasetID{Namespace: part, Name: part}
	o.Domain = part
	o.SchemaRef = part
	o.Source = part
	o.CorrelationID = part
	g := newGraph(t)
	admitted(t, g, o)
	rec := g.datasets[datasetKey{Owner: o.Owner, ID: o.Dataset}]
	entry := g.eventDS[eventKey{Owner: o.Owner, ID: o.EventID}]
	if unsafe.StringData(entry.observation.ParentEventID) == unsafe.StringData(parent) {
		t.Fatal("admission retained the large parent backing allocation")
	}
	values := []string{string(rec.info.owner), rec.info.ID.Namespace, rec.info.ID.Name, rec.info.Domain, rec.info.SchemaRef, rec.lastEventID,
		entry.observation.EventID, entry.observation.Domain, entry.observation.SchemaRef, entry.observation.Source, entry.observation.CorrelationID}
	for _, value := range values {
		if unsafe.StringData(value) == unsafe.StringData(part) {
			t.Fatal("admission retained the large inbound backing allocation")
		}
	}
}

func TestDatasetTupleOrderDoesNotCollapseDisplayStringCollisions(t *testing.T) {
	first := DatasetID{Namespace: "a", Name: "b.c"}
	second := DatasetID{Namespace: "a.b", Name: "c"}
	if first.String() != second.String() {
		t.Fatal("collision premise disappeared")
	}
	if !datasetLess(first, second) || datasetLess(second, first) {
		t.Fatal("tuple comparison collapsed equal display strings")
	}
	for _, order := range [][]DatasetID{{first, second}, {second, first}} {
		g := newGraph(t)
		for i, id := range order {
			admitted(t, g, fixtureObservation(fmt.Sprintf("parent-%d", i), id, "fixture", "fixture.v1.Parent:1", time.Unix(1, 0), ""))
		}
		root := DatasetID{Namespace: "z", Name: "root"}
		for i := range order {
			admitted(t, g, fixtureObservation(fmt.Sprintf("root-%d", i), root, "fixture", "fixture.v1.Root:1", time.Unix(2, 0), fmt.Sprintf("parent-%d", i)))
		}
		datasets := g.Datasets("acme")
		upstream := g.Upstream("acme", root)
		if len(datasets) != 3 || datasets[0].ID != first || datasets[1].ID != second {
			t.Fatal("catalog tuple order is nondeterministic")
		}
		if len(upstream) != 2 || upstream[0].ID != first || upstream[1].ID != second {
			t.Fatal("upstream tuple order is nondeterministic")
		}
	}
}

func TestOwnerBoundaryAndGlobalBudgetsCrossTenant(t *testing.T) {
	t.Run("foreign-parent-never-links", func(t *testing.T) {
		g := newGraph(t)
		parent := smallObservation()
		parent.EventID = "only-a"
		parent.Dataset.Name = "parent"
		admitted(t, g, parent)
		child := smallObservation()
		child.Owner = "b"
		child.EventID = "b-child"
		child.ParentEventID = parent.EventID
		child.At = child.At.Add(time.Second)
		snapshot := admitted(t, g, child)
		if _, known := snapshot.Input(); known {
			t.Fatal("foreign retained parent entered the owned snapshot")
		}
		if len(g.Upstream("b", child.Dataset)) != 0 {
			t.Fatal("foreign retained parent entered owned topology")
		}
		if g.Coverage("a").UnlinkedCauses != 0 || g.Coverage("b").UnlinkedCauses != 1 {
			t.Fatal("missing-parent coverage crossed owners")
		}
	})
	t.Run("edge-capacity-is-global", func(t *testing.T) {
		limits := fixtureLimits()
		limits.EdgeMax = 1
		g, err := NewMemory(limits)
		if err != nil {
			t.Fatal(err)
		}
		parent := smallObservation()
		parent.EventID = "a-parent"
		parent.Dataset.Name = "parent"
		admitted(t, g, parent)
		child := parent
		child.EventID = "a-child"
		child.Dataset.Name = "child"
		child.ParentEventID = parent.EventID
		child.At = child.At.Add(time.Second)
		admitted(t, g, child)
		parent.Owner = "b"
		parent.EventID = "b-parent"
		admitted(t, g, parent)
		before := capture(g)
		child.Owner = "b"
		child.EventID = "b-child"
		child.ParentEventID = parent.EventID
		if _, err := g.Observe(child); !errors.Is(err, ErrCapacity) {
			t.Fatalf("second owner's edge bypassed global capacity: %v", err)
		}
		unchanged(t, g, before)
	})
	t.Run("string-capacity-is-global", func(t *testing.T) {
		limits := fixtureLimits()
		limits.StringBytesMax = 21
		g, err := NewMemory(limits)
		if err != nil {
			t.Fatal(err)
		}
		a := smallObservation()
		admitted(t, g, a)
		if g.Stats().StringBytes != 11 {
			t.Fatal("literal first-owner 11-byte premise changed")
		}
		before := capture(g)
		b := a
		b.Owner = "b"
		// Each owner requires 11 retained bytes, independently counted in the
		// earlier literal oracle. 11+11=22 cannot fit ONE global budget of21.
		if _, err := g.Observe(b); !errors.Is(err, ErrCapacity) {
			t.Fatalf("second owner received a fresh string budget: %v", err)
		}
		unchanged(t, g, before)
	})
}
