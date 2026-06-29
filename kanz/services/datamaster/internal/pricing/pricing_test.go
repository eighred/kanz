package pricing

import (
	"testing"
	"time"
)

func ts(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func TestArbitrate_ConsensusMedian(t *testing.T) {
	now := ts(2026, 6, 1)
	cands := []Candidate{
		{Source: "BLOOMBERG", Price: 100.0, AsOf: now},
		{Source: "REFINITIV", Price: 100.5, AsOf: now},
		{Source: "ICE", Price: 101.0, AsOf: now},
	}
	a := Arbitrate("INST1", cands, 0.05, 24*time.Hour, now)
	if !a.HasPrice || a.Chosen != 100.5 {
		t.Fatalf("consensus = %v (has=%v), want median 100.5", a.Chosen, a.HasPrice)
	}
	if len(a.Exceptions) != 0 {
		t.Errorf("tight cluster should raise no exceptions, got %v", a.Exceptions)
	}
}

func TestArbitrate_ToleranceBreach(t *testing.T) {
	now := ts(2026, 6, 1)
	// ICE is a fat-finger outlier ~20% above the cluster.
	cands := []Candidate{
		{Source: "BLOOMBERG", Price: 100.0, AsOf: now},
		{Source: "REFINITIV", Price: 100.0, AsOf: now},
		{Source: "ICE", Price: 120.0, AsOf: now},
	}
	a := Arbitrate("INST1", cands, 0.05, 24*time.Hour, now)
	// Median (100) is robust to the single outlier.
	if a.Chosen != 100.0 {
		t.Errorf("consensus = %v, want 100 (median robust to outlier)", a.Chosen)
	}
	if len(a.Exceptions) != 1 || a.Exceptions[0].Kind != KindPriceTolerance {
		t.Fatalf("want 1 PRICE_TOLERANCE exception, got %v", a.Exceptions)
	}
	if a.Exceptions[0].ID != "INST1:PRICE_TOLERANCE:ICE" {
		t.Errorf("exception id = %q", a.Exceptions[0].ID)
	}
}

func TestArbitrate_StaleExcluded(t *testing.T) {
	now := ts(2026, 6, 10)
	cands := []Candidate{
		{Source: "BLOOMBERG", Price: 100.0, AsOf: now},
		{Source: "ICE", Price: 200.0, AsOf: ts(2026, 6, 1)}, // 9 days old
	}
	a := Arbitrate("INST1", cands, 0.05, 24*time.Hour, now)
	// The stale ICE price is excluded, so the consensus is just BLOOMBERG's 100.
	if a.Chosen != 100.0 {
		t.Errorf("consensus = %v, want 100 (stale excluded)", a.Chosen)
	}
	if len(a.Exceptions) != 1 || a.Exceptions[0].Kind != KindStalePrice {
		t.Fatalf("want 1 STALE_PRICE exception, got %v", a.Exceptions)
	}
}

func TestArbitrate_MissingAndAllStale(t *testing.T) {
	now := ts(2026, 6, 10)
	missing := Arbitrate("INST1", nil, 0, 0, now)
	if missing.HasPrice || len(missing.Exceptions) != 1 || missing.Exceptions[0].Kind != KindMissingPrice {
		t.Fatalf("empty candidates ⇒ MISSING_PRICE, got %+v", missing)
	}
	allStale := Arbitrate("INST1", []Candidate{{Source: "ICE", Price: 1, AsOf: ts(2026, 1, 1)}}, 0, 0, now)
	if allStale.HasPrice {
		t.Errorf("all-stale should yield no consensus")
	}
	if len(allStale.Exceptions) != 1 || allStale.Exceptions[0].Kind != KindStalePrice {
		t.Errorf("all-stale ⇒ STALE_PRICE, got %v", allStale.Exceptions)
	}
}

func TestQueue_OverrideAuditTrail(t *testing.T) {
	now := ts(2026, 6, 1)
	q := NewQueue()
	// Three sources so the median (100) outvotes the single ICE outlier (130).
	a := Arbitrate("INST1", []Candidate{
		{Source: "BLOOMBERG", Price: 100, AsOf: now},
		{Source: "REFINITIV", Price: 100, AsOf: now},
		{Source: "ICE", Price: 130, AsOf: now},
	}, 0.05, 24*time.Hour, now)
	q.AddAll(a.Exceptions)

	open := q.Open()
	if len(open) != 1 {
		t.Fatalf("want 1 open exception, got %d", len(open))
	}
	id := open[0].ID

	// Re-detecting the same break must NOT wipe the entry (idempotent on id).
	q.AddAll(a.Exceptions)
	if len(q.Open()) != 1 {
		t.Fatalf("re-add should be idempotent")
	}

	// An analyst overrides with a chosen price.
	if err := q.Override(id, "alice@kanz", "ICE confirmed correct after corp action", 130, now); err != nil {
		t.Fatalf("override: %v", err)
	}
	ex, _ := q.Get(id)
	if ex.Status != StatusOverridden {
		t.Errorf("status = %v, want OVERRIDDEN", ex.Status)
	}
	if len(ex.Overrides) != 1 || ex.Overrides[0].Actor != "alice@kanz" || ex.Overrides[0].ChosenPrice != 130 {
		t.Fatalf("override audit = %+v", ex.Overrides)
	}
	// Overridden exceptions leave the OPEN queue.
	if len(q.Open()) != 0 {
		t.Errorf("overridden exception should leave the open queue")
	}
	// A second override appends — the trail is append-only.
	if err := q.Override(id, "bob@kanz", "re-reviewed", 130, now); err != nil {
		t.Fatalf("second override: %v", err)
	}
	ex, _ = q.Get(id)
	if len(ex.Overrides) != 2 {
		t.Errorf("override trail = %d entries, want 2 (append-only)", len(ex.Overrides))
	}

	// Override of an unknown id, or missing actor/reason, errors.
	if err := q.Override("nope", "a", "b", 0, now); err == nil {
		t.Errorf("unknown id should error")
	}
	if err := q.Override(id, "", "", 0, now); err == nil {
		t.Errorf("missing actor/reason should error")
	}
}
