package app

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/internal/risk/compute/factor"
)

// stubSource is a Source over a fixed map — the cache's transport seam, so
// these tests need no datamaster and no server.
type stubSource map[string]refdata.Record

func (s stubSource) Fetch(_ context.Context, id string) (refdata.Record, bool, error) {
	rec, ok := s[id]
	return rec, ok, nil
}

// warmCache builds the production shape: a cache filled by one Refresh cycle.
func warmCache(t *testing.T, src stubSource) *refdata.Cache {
	t.Helper()
	c, err := refdata.NewCache(src, refdata.Options{})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	for id := range src {
		c.Lookup(id, time.Time{}) // record the want
	}
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return c
}

func TestTheFactorClassifierRenamesTheCachesVocabulary(t *testing.T) {
	cache := warmCache(t, stubSource{
		"AAPL": {
			AssetClass: "EQUITY",
			Sector:     refdata.Sector{Taxonomy: "GICS", Code: "45", Name: "Information Technology"},
			IssuerID:   "LEI-APPLE",
		},
	})
	cl := NewFactorClassifier(cache)

	got, ok := cl.Classify(context.Background(), "AAPL", time.Time{})
	if !ok {
		t.Fatal("the factor classifier refused an instrument the cache resolves")
	}
	if got.AssetClass != "EQUITY" {
		t.Errorf("AssetClass = %q, want EQUITY", got.AssetClass)
	}
	// THE SAME BUCKET STRING THE COMPLIANCE PROJECTION PRODUCES. The two read one
	// Cache precisely so a sector a mandate buckets on and a sector a stress
	// shocks cannot drift apart, and factor.Sector.Key is the join both sides use.
	if got.Sector.Key() != "GICS:45" {
		t.Errorf("Sector.Key = %q, want GICS:45 — the same string compliance.Attributes.Sector "+
			"carries, or a mandate and a stress test are bucketing on different keys",
			got.Sector.Key())
	}

	rec, _ := cache.Lookup("AAPL", time.Time{})
	if got.Sector.Key() != rec.Sector.Key() {
		t.Errorf("the adapter produced %q from a record holding %q", got.Sector.Key(), rec.Sector.Key())
	}
}

// A NIL CACHE MUST YIELD A NIL Classifier, not one that answers "unknown" to
// everything. engine.WithClassifier reads nil as "this deployment has no
// reference-data source" — and a non-nil classifier that resolves nothing would
// look identical to a wired one whose master is empty, which is the conflation
// the refusal vocabulary exists to prevent.
func TestANilCacheYieldsANilClassifier(t *testing.T) {
	if cl := NewFactorClassifier(nil); cl != nil {
		t.Fatalf("NewFactorClassifier(nil) = %#v, want a nil factor.Classifier", cl)
	}
}

func TestAnUnresolvedInstrumentIsRefusedRatherThanBucketedEmpty(t *testing.T) {
	cl := NewFactorClassifier(warmCache(t, stubSource{
		"AAPL": {AssetClass: "EQUITY", Sector: refdata.Sector{Taxonomy: "GICS", Code: "45"}},
	}))

	got, ok := cl.Classify(context.Background(), "PRIVATE-CO", time.Time{})
	if ok {
		t.Fatalf("an instrument the master does not hold resolved to %+v", got)
	}
	// factor.SectorExposure puts these in UnclassifiedSector so the sector totals
	// still reconcile with gross; EvaluateScenario refuses. Both depend on ok
	// being false rather than on an empty Sector coming back as resolved.
	if !got.Sector.IsZero() {
		t.Errorf("a refused classification carried a sector: %+v", got.Sector)
	}
}

var _ factor.Classifier = NewFactorClassifier(&refdata.Cache{})
