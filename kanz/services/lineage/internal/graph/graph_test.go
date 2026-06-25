package graph_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/services/lineage/internal/graph"
)

func ds(ns, name string) graph.DatasetID { return graph.DatasetID{Namespace: ns, Name: name} }

func TestUpstreamIsTransitiveAndDeterministic(t *testing.T) {
	g := graph.NewMemory()
	now := time.Now()
	// e1 (market.MarketDataEvent) → e2 (risk.ExposureSet) → e3 (risk.Measures)
	g.Observe("e1", ds("kanz.market", "MarketDataEvent"), "market", "market.v1.MarketDataEvent:1", now, "")
	g.Observe("e2", ds("kanz.risk", "ExposureSet"), "risk", "risk.v1.ExposureSet:1", now, "e1")
	g.Observe("e3", ds("kanz.risk", "Measures"), "risk", "risk.v1.Measures:1", now, "e2")

	got := g.Upstream(ds("kanz.risk", "Measures"))
	want := []graph.DatasetID{ds("kanz.market", "MarketDataEvent"), ds("kanz.risk", "ExposureSet")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Upstream = %v, want %v", got, want)
	}
}

func TestDatasetOfAndSelfLoopSkipped(t *testing.T) {
	g := graph.NewMemory()
	now := time.Now()
	g.Observe("s1", ds("kanz.risk", "PortfolioState"), "risk", "risk.v1.PortfolioState:1", now, "")
	// A snapshot of the same entity, caused by the prior state event — a self
	// loop, not provenance.
	g.Observe("s2", ds("kanz.risk", "PortfolioState"), "risk", "risk.v1.PortfolioState:1", now, "s1")

	if got, ok := g.DatasetOf("s2"); !ok || got != ds("kanz.risk", "PortfolioState") {
		t.Errorf("DatasetOf(s2) = %v,%v", got, ok)
	}
	if up := g.Upstream(ds("kanz.risk", "PortfolioState")); len(up) != 0 {
		t.Errorf("self-loop should not be provenance, got upstream %v", up)
	}
}

func TestUpstreamCycleSafe(t *testing.T) {
	g := graph.NewMemory()
	now := time.Now()
	// Construct a malformed cycle a→b→a across two datasets.
	g.Observe("a", ds("kanz.x", "A"), "x", "", now, "")
	g.Observe("b", ds("kanz.x", "B"), "x", "", now, "a")
	g.Observe("a2", ds("kanz.x", "A"), "x", "", now, "b") // A now also derived from B
	// Upstream(A) must terminate (not loop) and include B.
	up := g.Upstream(ds("kanz.x", "A"))
	if len(up) != 1 || up[0] != ds("kanz.x", "B") {
		t.Errorf("Upstream(A) = %v, want [kanz.x.B]", up)
	}
}

func TestDatasetsCatalogListing(t *testing.T) {
	g := graph.NewMemory()
	now := time.Now()
	g.Observe("e1", ds("kanz.risk", "ExposureSet"), "risk", "risk.v1.ExposureSet:2", now, "")
	g.Observe("e2", ds("kanz.risk", "ExposureSet"), "risk", "risk.v1.ExposureSet:2", now.Add(time.Hour), "")
	list := g.Datasets()
	if len(list) != 1 || list[0].Events != 2 || list[0].SchemaRef != "risk.v1.ExposureSet:2" {
		t.Fatalf("catalog = %+v", list)
	}
	if !list[0].LastSeen.Equal(now.Add(time.Hour)) {
		t.Errorf("LastSeen = %v, want latest", list[0].LastSeen)
	}
}
