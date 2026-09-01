package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/refdata"
)

// refDataAsOf is the reference record's OWN as_of: a month after the portfolio's
// last fill, because the security master kept being refreshed while the fund sat
// still. lastFill and clockNow come from mandate_clock_test.go — the same three
// instants, because this is the same boot replay one layer down.
var refDataAsOf = lastFill.Add(30 * 24 * time.Hour)

// masterOf is a refdata.Source holding a fixed set of golden records, all dated
// refDataAsOf. It is the seam refdata.Cache exists to make testable; everything
// downstream of it — the as-of rule, the staleness bound, the compliance
// projection — is the REAL cache and the REAL classifier the composition root
// wires (services/compliance/cmd/compliance, refCache.Compliance).
type masterOf map[string]refdata.Record

func (m masterOf) Fetch(_ context.Context, instrumentID string) (refdata.Record, bool, error) {
	rec, ok := m[instrumentID]
	if !ok {
		return refdata.Record{}, false, nil
	}
	return rec, true, nil
}

// armedCache builds the production classifier over master and PRIMES it, so the
// cache is warm at clockNow exactly as a running pod's is. Priming matters: the
// cache is demand-driven, so an instrument nothing has asked about is not
// resident and would refuse for a reason that is not this issue's.
func armedCache(t *testing.T, master masterOf) comp.Classifier {
	t.Helper()
	cache, err := refdata.NewCache(master, refdata.Options{Now: func() time.Time { return clockNow }})
	if err != nil {
		t.Fatalf("refdata.NewCache: %v", err)
	}
	cl := cache.Compliance()
	ctx := context.Background()
	for id := range master {
		_, _ = cl.Classify(ctx, id, clockNow) // records the want
	}
	if _, err := cache.Refresh(ctx); err != nil {
		t.Fatalf("refdata.Refresh: %v", err)
	}
	for id := range master {
		if _, ok := cl.Classify(ctx, id, clockNow); !ok {
			t.Fatalf("the cache did not arm %s at %s, so this test would prove nothing about the "+
				"as-of rule", id, clockNow)
		}
	}
	return cl
}

// sectorCapRegistry governs t1/p1 with one rule, in force since before the
// replayed fill — so the mandate lookup is never what this file is measuring.
func sectorCapRegistry(t *testing.T, rule *compliancepb.Rule) *comp.MandateRegistry {
	t.Helper()
	reg := comp.NewMandateRegistry()
	if err := reg.Put(&compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		EffectiveAt: timestamppb.New(lastFill),
		Rules:       []*compliancepb.Rule{rule},
	}); err != nil {
		t.Fatalf("registry refused a well-formed mandate: %v", err)
	}
	return reg
}

// sectorBucketCap is "no more than maxPct of gross in THIS sector".
func sectorBucketCap(bucket string, maxPct int64) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "sector-cap", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			Bucket:    bucket,
			MaxWeight: decv(maxPct, -2),
		}},
	}
}

// techMaster is the fund's one holding, classified into GICS:45, on a record
// dated a month AFTER the fill that produced the holding.
func techMaster() masterOf {
	return masterOf{"AAPL": {
		InstrumentID: "AAPL",
		AssetClass:   "EQUITY",
		Sector:       refdata.Sector{Taxonomy: "GICS", Code: "45", Name: "Information Technology"},
		IssuerID:     "APPLE",
		AsOf:         refDataAsOf,
	}}
}

// TestAReplayedPositionIsClassifiedAgainstCurrentReferenceData pins #930, and it
// is the issue's own "verified when".
//
// THE SHAPE IS THE SAME BOOT REPLAY #917 was about. The monitor arms from
// DeliverLastPerSubject over the compacted POSITION stream, so the first FACT it
// sees for a portfolio carries the as_of of that portfolio's LAST FILL — a month
// old for a fund that is not trading. refdata.Cache holds ONE snapshot per
// instrument and REFUSES a record whose own as_of is after the question's, so
// asking the classifier at that fill left every instrument refreshed since it
// unresolved — and comp.unresolvedDimension turns unresolved into a VIOLATION.
//
// The cap here binds a sector the fund does not hold a cent of, so there is
// nothing to breach. It breached anyway: the monitor emitted a FACT-grade
// ComplianceBreach saying the sector dimension could not be verified, on every
// restart, and AUTO-01 halts and escalates on that.
func TestAReplayedPositionIsClassifiedAgainstCurrentReferenceData(t *testing.T) {
	fb := &fakeBus{}
	m := NewMonitor(comp.NewEngine(nil),
		sectorCapRegistry(t, sectorBucketCap("GICS:50", 10)),
		armedCache(t, techMaster()), NewEmitter(fb), nil, nil)
	m.now = func() time.Time { return clockNow }

	// The whole book is GICS:45; the cap names GICS:50, which the fund does not
	// hold. Nothing is breaching.
	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, lastFill)); err != nil {
		t.Fatal(err)
	}

	if len(fb.events) != 0 {
		t.Fatalf("a portfolio that is not breaching emitted %d breach FACT(s): %s. The position FACT's "+
			"as_of (%s) predates the reference record's own as_of (%s), so the classifier was asked a "+
			"BACKDATED question, refused it, and the refusal arrived as an unresolved-dimension breach",
			len(fb.events), breachMessages(fb), lastFill, refDataAsOf)
	}
}

// TestAReplayedPositionStillBreachesTheSectorItIsActuallyIn is the positive
// control, and it is the assertion that "no breach FACT" alone cannot make: a
// monitor that never evaluated anything also emits nothing.
//
// Same replay, same backdated FACT, but the cap now names the sector the fund IS
// wholly in. The breach must be the REAL one — "concentration exceeds limit",
// naming GICS:45 — and not the refusal. Under the defect this test also emitted
// a breach, which is exactly why it asserts on CONTENT: the message and the
// bucket are what tell "the sector cap fired" apart from "the sector could not
// be read", and only the first is a control doing its job.
func TestAReplayedPositionStillBreachesTheSectorItIsActuallyIn(t *testing.T) {
	fb := &fakeBus{}
	rec := &recordingRecorder{}
	m := NewMonitor(comp.NewEngine(nil),
		sectorCapRegistry(t, sectorBucketCap("GICS:45", 10)),
		armedCache(t, techMaster()), NewEmitter(fb), rec, nil)
	m.now = func() time.Time { return clockNow }

	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, lastFill)); err != nil {
		t.Fatal(err)
	}

	if len(fb.events) != 1 {
		t.Fatalf("a book 100%% in a sector capped at 10%% emitted %d breach FACTs, want 1", len(fb.events))
	}
	breach, ok := fb.events[0].Payload.(*compliancepb.ComplianceBreach)
	if !ok {
		t.Fatalf("payload is not a ComplianceBreach: %T", fb.events[0].Payload)
	}
	var fired *compliancepb.Violation
	for _, v := range breach.GetResult().GetViolations() {
		if v.GetMessage() == "concentration exceeds limit" {
			fired = v
		}
		if strings.Contains(v.GetMessage(), "cannot be verified") {
			t.Fatalf("the sector dimension was REFUSED rather than read: %q, evidence %v. The "+
				"classifier was asked at the replayed FACT's as_of (%s) instead of at the monitor's "+
				"clock (%s), and the reference record is dated %s",
				v.GetMessage(), v.GetEvidence(), lastFill, clockNow, refDataAsOf)
		}
	}
	if fired == nil {
		t.Fatalf("no concentration violation among %s", breachMessages(fb))
	}
	if got := fired.GetEvidence()["bucket"]; got != "GICS:45" {
		t.Fatalf("the violation names bucket %q, want GICS:45 — the bucket is what proves the "+
			"classifier RESOLVED the holding rather than dropping it into the empty key", got)
	}
	// THE ATTRIBUTION HALF, unmoved (#917). Only the LOOKUPS ask the monitor's
	// clock; what the breach is stamped with is still the observation.
	if got := breach.GetDetectedAt().AsTime(); !got.Equal(lastFill) {
		t.Fatalf("breach detected_at is %s, want the observation's as_of %s", got, lastFill)
	}
	if got := breach.GetResult().GetEvaluatedAt().AsTime(); !got.Equal(lastFill) {
		t.Fatalf("result evaluated_at is %s, want the observation's as_of %s — giving the "+
			"reference-data lookup its own clock must not move the stamp", got, lastFill)
	}
	if len(rec.records) != 1 {
		t.Fatalf("expected 1 post-trade decision record, got %d", len(rec.records))
	}
}

// TestASectorTheMasterDoesNotHoldStillBreaches is the control this change must
// NOT turn off.
//
// "The question was backdated" and "the dimension is dark" reach the rule engine
// as the same ok=false, and #930 is about telling them apart — not about
// answering both with a pass. An instrument the security master does not hold is
// unresolved at NOW as well, and a sector cap that cannot see a sector has not
// been checked (#640). It must still refuse, and the evidence must still name the
// holding so somebody can go and load it.
func TestASectorTheMasterDoesNotHoldStillBreaches(t *testing.T) {
	fb := &fakeBus{}
	// The master holds a DIFFERENT instrument, so the cache is armed and healthy
	// and the fund's holding is simply unknown to it — a reference-data gap, not
	// an unwired classifier.
	master := masterOf{"MSFT": {
		InstrumentID: "MSFT",
		AssetClass:   "EQUITY",
		Sector:       refdata.Sector{Taxonomy: "GICS", Code: "45"},
		IssuerID:     "MICROSOFT",
		AsOf:         refDataAsOf,
	}}
	m := NewMonitor(comp.NewEngine(nil),
		sectorCapRegistry(t, sectorBucketCap("GICS:50", 10)),
		armedCache(t, master), NewEmitter(fb), nil, nil)
	m.now = func() time.Time { return clockNow }

	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, lastFill)); err != nil {
		t.Fatal(err)
	}

	if len(fb.events) != 1 {
		t.Fatalf("a SECTOR cap over a holding the security master cannot classify emitted %d breach "+
			"FACTs, want 1: the dimension is dark, and a control whose input the platform cannot "+
			"establish refuses", len(fb.events))
	}
	breach := fb.events[0].Payload.(*compliancepb.ComplianceBreach)
	var refusal *compliancepb.Violation
	for _, v := range breach.GetResult().GetViolations() {
		if strings.Contains(v.GetMessage(), "cannot be verified") {
			refusal = v
		}
	}
	if refusal == nil {
		t.Fatalf("no unresolved-dimension refusal among %s", breachMessages(fb))
	}
	if got := refusal.GetEvidence()["unresolved_instruments"]; got != "AAPL" {
		t.Fatalf("the refusal names unresolved_instruments=%q, want AAPL", got)
	}
	if got := refusal.GetEvidence()["classifier"]; got != "present" {
		t.Fatalf("the refusal says classifier=%q, want present — a reference-data gap and an unwired "+
			"classifier send an operator to different places", got)
	}
}

// breachMessages renders the violation messages on the published breach FACTs.
func breachMessages(fb *fakeBus) string {
	var msgs []string
	for _, e := range fb.events {
		b, ok := e.Payload.(*compliancepb.ComplianceBreach)
		if !ok {
			continue
		}
		for _, v := range b.GetResult().GetViolations() {
			msgs = append(msgs, v.GetMessage())
		}
	}
	if len(msgs) == 0 {
		return "(no violations)"
	}
	return strings.Join(msgs, "; ")
}
