package refdata

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/compliance"
)

// warm builds a cache holding one record, the way both projections see it.
func warm(t *testing.T, id string, rec Record) *Cache {
	t.Helper()
	src := newFakeSource()
	src.put(id, rec)
	c, _ := testCache(t, src, nil)
	c.Lookup(id, time.Time{})
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return c
}

func TestTheProjectionCannotDisagreeWithItsRecord(t *testing.T) {
	c := warm(t, "AAPL", Record{
		AssetClass: "EQUITY",
		Sector:     Sector{Taxonomy: "GICS", Code: "45", Name: "Information Technology"},
		IssuerID:   "LEI-APPLE",
	})

	attrs, ok := c.Compliance().Classify(context.Background(), "AAPL", time.Time{})
	if !ok {
		t.Fatal("the compliance projection refused a resolved instrument")
	}
	if attrs.Sector != "GICS:45" || attrs.Issuer != "LEI-APPLE" || attrs.AssetClass != "EQUITY" {
		t.Fatalf("compliance.Attributes = %+v, want the resolved classification", attrs)
	}

	// The FACTOR half of this pairing is asserted in
	// services/risk-engine/internal/app, which is the only package the RISK-02
	// boundary lets name factor.Classification — see the note at the top of
	// classifier.go. Lookup is what both projections read, so this pins the
	// record they must agree about.
	rec, ok := c.Lookup("AAPL", time.Time{})
	if !ok {
		t.Fatal("the cache refused an instrument its own projection resolved")
	}
	if rec.Sector.Key() != attrs.Sector {
		t.Fatalf("Record sector %q != compliance sector %q — the projection must not be able to "+
			"disagree with the record it reads", rec.Sector.Key(), attrs.Sector)
	}
	if rec.AssetClass != attrs.AssetClass {
		t.Fatalf("Record asset class %q != compliance asset class %q", rec.AssetClass, attrs.AssetClass)
	}
}

// AN EMPTY FIELD ON A RESOLVED RECORD IS ok=true. The master genuinely says
// this instrument has no issuer — true for FX and a broad index — and it is
// bucketKey's empty string that compliance.unresolvedDimension keys its
// per-instrument refusal on. Returning ok=false here would report a
// reference-data gap that does not exist and send an operator to load data that
// is already loaded.
func TestAResolvedRecordWithNoIssuerStillResolves(t *testing.T) {
	c := warm(t, "EURUSD", Record{AssetClass: "FX"})

	attrs, ok := c.Compliance().Classify(context.Background(), "EURUSD", time.Time{})
	if !ok {
		t.Fatal("an instrument the master resolved with no issuer was reported UNRESOLVED — that " +
			"is a different fact, with a different operator action, from \"this instrument has " +
			"no issuer\"")
	}
	if attrs.Issuer != "" || attrs.Sector != "" {
		t.Fatalf("Attributes = %+v, want the empty issuer and sector the master actually holds", attrs)
	}
	if attrs.AssetClass != "FX" {
		t.Fatalf("AssetClass = %q, want FX", attrs.AssetClass)
	}
}

func TestAnUnresolvedInstrumentRefusesThroughTheProjection(t *testing.T) {
	c := warm(t, "AAPL", Record{AssetClass: "EQUITY"})

	if _, ok := c.Compliance().Classify(context.Background(), "NOTHELD", time.Time{}); ok {
		t.Error("the compliance projection resolved an instrument the cache does not hold")
	}
	if _, ok := c.Lookup("NOTHELD", time.Time{}); ok {
		t.Error("the cache resolved an instrument it does not hold")
	}
}

// The projection must satisfy the interface the composition roots pass it as,
// through a variable of the interface type — the compile-time assertions in
// classifier.go say the same thing, and this says it where a reader of the
// consuming package would look.
func TestTheProjectionIsAComplianceClassifier(t *testing.T) {
	var cl compliance.Classifier = warm(t, "AAPL", Record{AssetClass: "EQUITY"}).Compliance()
	if _, ok := cl.Classify(context.Background(), "AAPL", time.Time{}); !ok {
		t.Fatal("the interface value does not resolve what the cache holds")
	}
}
