package pricing

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

func ts(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

// tol is the 5% default, spelled out.
func tol() *big.Rat { return dec.Rat("0.05") }

func TestArbitrate_ConsensusMedian(t *testing.T) {
	now := ts(2026, 6, 1)
	cands := []Candidate{
		{Source: "BLOOMBERG", Price: dec.Rat("100"), AsOf: now},
		{Source: "REFINITIV", Price: dec.Rat("100.5"), AsOf: now},
		{Source: "ICE", Price: dec.Rat("101"), AsOf: now},
	}
	a := Arbitrate("INST1", cands, tol(), 24*time.Hour, now)
	if !a.HasPrice || a.Chosen.Cmp(dec.Rat("100.5")) != 0 {
		t.Fatalf("consensus = %v (has=%v), want median 100.5", a.Chosen, a.HasPrice)
	}
	if len(a.Exceptions) != 0 {
		t.Errorf("tight cluster should raise no exceptions, got %v", a.Exceptions)
	}
}

// TestArbitrate_MedianIsExact pins what DATA-M8b bought.
//
// The consensus of 0.10 and 0.20 is 0.15. As float64 it is not: 0.1 and 0.2 are
// both inexact in binary, and (0.1+0.2)/2 evaluates to 0.15000000000000002 — a
// mark that is not the number any vendor quoted and not the number anyone would
// write down. This is the value feed.NormalizePrice publishes to the market plane
// as the instrument's price, and the value a human is shown when adjudicating a
// break. A median of exact decimals is exact, and now it stays that way.
func TestArbitrate_MedianIsExact(t *testing.T) {
	now := ts(2026, 6, 1)
	a := Arbitrate("INST1", []Candidate{
		{Source: "A", Price: dec.Rat("0.10"), AsOf: now},
		{Source: "B", Price: dec.Rat("0.20"), AsOf: now},
	}, tol(), 24*time.Hour, now)

	if got := dec.Str(a.Chosen); got != "0.15" {
		t.Fatalf("consensus = %s, want exactly 0.15 (float64 gives 0.15000000000000002)", got)
	}
	if a.Chosen.Cmp(big.NewRat(3, 20)) != 0 {
		t.Fatalf("consensus %v is not exactly 3/20", a.Chosen)
	}
}

// The tolerance test is a multiplication, not a division, so a candidate sitting
// exactly ON the 5% line decides deterministically rather than on whichever way a
// binary rounding fell. Exactly 5% is within tolerance; a hair beyond is not.
func TestArbitrate_ToleranceBoundaryIsExact(t *testing.T) {
	now := ts(2026, 6, 1)
	at := func(price string) Arbitration {
		return Arbitrate("INST1", []Candidate{
			{Source: "A", Price: dec.Rat("100"), AsOf: now},
			{Source: "B", Price: dec.Rat("100"), AsOf: now},
			{Source: "C", Price: dec.Rat(price), AsOf: now},
		}, tol(), 24*time.Hour, now)
	}
	if ex := at("105").Exceptions; len(ex) != 0 { // exactly 5% — within tolerance
		t.Fatalf("a candidate exactly on the tolerance line was flagged: %v", ex)
	}
	if ex := at("105.000000001").Exceptions; len(ex) != 1 || ex[0].Kind != KindPriceTolerance {
		t.Fatalf("a candidate a hair beyond the line was not flagged: %v", ex)
	}
}

func TestArbitrate_ToleranceBreach(t *testing.T) {
	now := ts(2026, 6, 1)
	// ICE is a fat-finger outlier ~20% above the cluster.
	cands := []Candidate{
		{Source: "BLOOMBERG", Price: dec.Rat("100"), AsOf: now},
		{Source: "REFINITIV", Price: dec.Rat("100"), AsOf: now},
		{Source: "ICE", Price: dec.Rat("120"), AsOf: now},
	}
	a := Arbitrate("INST1", cands, tol(), 24*time.Hour, now)
	// Median (100) is robust to the single outlier.
	if a.Chosen.Cmp(dec.Rat("100")) != 0 {
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
		{Source: "BLOOMBERG", Price: dec.Rat("100"), AsOf: now},
		{Source: "ICE", Price: dec.Rat("200"), AsOf: ts(2026, 6, 1)}, // 9 days old
	}
	a := Arbitrate("INST1", cands, tol(), 24*time.Hour, now)
	// The stale ICE price is excluded, so the consensus is just BLOOMBERG's 100.
	if a.Chosen.Cmp(dec.Rat("100")) != 0 {
		t.Errorf("consensus = %v, want 100 (stale excluded)", a.Chosen)
	}
	if len(a.Exceptions) != 1 || a.Exceptions[0].Kind != KindStalePrice {
		t.Fatalf("want 1 STALE_PRICE exception, got %v", a.Exceptions)
	}
}

func TestArbitrate_MissingAndAllStale(t *testing.T) {
	now := ts(2026, 6, 10)
	missing := Arbitrate("INST1", nil, nil, 0, now)
	if missing.HasPrice || len(missing.Exceptions) != 1 || missing.Exceptions[0].Kind != KindMissingPrice {
		t.Fatalf("empty candidates ⇒ MISSING_PRICE, got %+v", missing)
	}
	allStale := Arbitrate("INST1", []Candidate{{Source: "ICE", Price: dec.Rat("1"), AsOf: ts(2026, 1, 1)}}, nil, 0, now)
	if allStale.HasPrice {
		t.Errorf("all-stale should yield no consensus")
	}
	if len(allStale.Exceptions) != 1 || allStale.Exceptions[0].Kind != KindStalePrice {
		t.Errorf("all-stale ⇒ STALE_PRICE, got %v", allStale.Exceptions)
	}
}

// A source that supplies no price at all has not quoted zero. Voting a nil into
// the median as a 0 would drag the consensus toward zero — a mark nobody quoted,
// on an instrument somebody holds.
func TestArbitrate_NoPriceIsNotAZeroPrice(t *testing.T) {
	now := ts(2026, 6, 1)
	a := Arbitrate("INST1", []Candidate{
		{Source: "GOOD", Price: dec.Rat("100"), AsOf: now},
		{Source: "SILENT", Price: nil, AsOf: now},
	}, nil, 0, now)

	if !a.HasPrice || a.Chosen.Cmp(dec.Rat("100")) != 0 {
		t.Fatalf("consensus = %v, want 100 (the silent source must not vote a 0)", a.Chosen)
	}
	if len(a.Exceptions) != 1 || a.Exceptions[0].Kind != KindMissingPrice {
		t.Fatalf("want a MISSING_PRICE break against the silent source, got %v", a.Exceptions)
	}
	if a.Exceptions[0].ID != "INST1:MISSING_PRICE:SILENT" {
		t.Errorf("exception id = %q", a.Exceptions[0].ID)
	}
}

// The override audit record crosses the wire as an exact decimal STRING. A JSON
// number is an IEEE-754 double by definition, so emitting one would round the
// price a named human chose on the way out of the compliance record.
func TestOverrideMarshalsAsAnExactDecimalString(t *testing.T) {
	o := Override{Actor: "alice@kanz", Reason: "corp action", ChosenPrice: dec.Rat("123456789.123456789"), At: ts(2026, 6, 1)}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	price, ok := out["chosen_price"].(string)
	if !ok {
		t.Fatalf("chosen_price came out as %T (%v) — a JSON number is a float", out["chosen_price"], out["chosen_price"])
	}
	if price != "123456789.12345679" { // dec.Str renders at the platform's 8dp scale
		t.Fatalf("chosen_price = %q", price)
	}
}

func TestQueue_OverrideAuditTrail(t *testing.T) {
	now := ts(2026, 6, 1)
	q := NewQueue()
	// Three sources so the median (100) outvotes the single ICE outlier (130).
	a := Arbitrate("INST1", []Candidate{
		{Source: "BLOOMBERG", Price: dec.Rat("100"), AsOf: now},
		{Source: "REFINITIV", Price: dec.Rat("100"), AsOf: now},
		{Source: "ICE", Price: dec.Rat("130"), AsOf: now},
	}, tol(), 24*time.Hour, now)
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
	if err := q.Override(id, "alice@kanz", "ICE confirmed correct after corp action", dec.Rat("130"), now); err != nil {
		t.Fatalf("override: %v", err)
	}
	ex, _ := q.Get(id)
	if ex.Status != StatusOverridden {
		t.Errorf("status = %v, want OVERRIDDEN", ex.Status)
	}
	if len(ex.Overrides) != 1 || ex.Overrides[0].Actor != "alice@kanz" || ex.Overrides[0].ChosenPrice.Cmp(dec.Rat("130")) != 0 {
		t.Fatalf("override audit = %+v", ex.Overrides)
	}
	// Overridden exceptions leave the OPEN queue.
	if len(q.Open()) != 0 {
		t.Errorf("overridden exception should leave the open queue")
	}
	// A second override appends — the trail is append-only.
	if err := q.Override(id, "bob@kanz", "re-reviewed", dec.Rat("130"), now); err != nil {
		t.Fatalf("second override: %v", err)
	}
	ex, _ = q.Get(id)
	if len(ex.Overrides) != 2 {
		t.Errorf("override trail = %d entries, want 2 (append-only)", len(ex.Overrides))
	}

	// Override of an unknown id, a missing actor/reason, or no chosen price errors.
	if err := q.Override("nope", "a", "b", dec.Rat("1"), now); err == nil {
		t.Errorf("unknown id should error")
	}
	if err := q.Override(id, "", "", dec.Rat("1"), now); err == nil {
		t.Errorf("missing actor/reason should error")
	}
	if err := q.Override(id, "a", "b", nil, now); err == nil {
		t.Errorf("a nil chosen price should error — an audit record must say what was chosen")
	}
}
