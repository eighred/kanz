package monitor

import (
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
)

// The three instants this file is about, deliberately far apart so no assertion
// can pass by rounding.
//
//	lastFill         — the as_of on the position FACT the monitor replays at boot.
//	mandateEffective — when the mandate governing the portfolio took force, a
//	                   month AFTER that fill.
//	clockNow         — what the monitor's clock says while it is evaluating.
var (
	lastFill         = t0
	mandateEffective = t0.Add(30 * 24 * time.Hour)
	clockNow         = mandateEffective.Add(72 * time.Hour)
)

// effectiveAtRegistry is concentrationRegistry with the mandate's effective_at
// moved, so a test can place the position FACT on either side of it.
func effectiveAtRegistry(t *testing.T, effectiveAt time.Time) *comp.MandateRegistry {
	t.Helper()
	reg := comp.NewMandateRegistry()
	if err := reg.Put(&compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		EffectiveAt: timestamppb.New(effectiveAt),
		Rules: []*compliancepb.Rule{{
			RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				MaxWeight: decv(60, -2), // 60%
			}},
		}},
	}); err != nil {
		t.Fatalf("registry refused a well-formed mandate: %v", err)
	}
	return reg
}

// TestAReplayedPositionIsGovernedByTheMandateInForceNow pins #917.
//
// THE SHAPE IS A BOOT REPLAY, which is the only way this is reached in
// production and the reason it went unnoticed. The POSITION stream is compacted
// and the monitor arms from DeliverLastPerSubject, so the first FACT it sees for
// a portfolio carries the as_of of that portfolio's LAST FILL — months old for a
// fund that is not trading. The mandate registry, on the other side, is armed
// from its own compacted stream and holds the version IN FORCE plus the ones
// scheduled (#884, #916); no superseded version is resident to find.
//
// Resolving the mandate at the FACT's as_of therefore did not select an older
// mandate — there is no older mandate to select. It selected NOTHING, because
// the only resident version is effective AFTER the replayed fill, and a miss is
// UNGOVERNED: the monitor said once that nothing was checking the portfolio and
// returned. A fund holding 100% of one instrument under a 60% concentration cap
// was not checked at all, silently, for as long as it did not trade.
func TestAReplayedPositionIsGovernedByTheMandateInForceNow(t *testing.T) {
	fb := &fakeBus{}
	rec := &recordingRecorder{}
	m := NewMonitor(comp.NewEngine(nil), effectiveAtRegistry(t, mandateEffective), nil, NewEmitter(fb), rec, nil)
	m.now = func() time.Time { return clockNow }

	// One instrument at 100% of the book against a 60% cap: a breach if anything
	// evaluates it at all.
	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, lastFill)); err != nil {
		t.Fatal(err)
	}

	if len(fb.events) != 1 {
		t.Fatalf("the replayed book was not evaluated: %d breach FACTs, want 1 — a position FACT whose "+
			"as_of (%s) predates the mandate's effective_at (%s) resolved to no mandate, and the "+
			"portfolio was skipped as UNGOVERNED", len(fb.events), lastFill, mandateEffective)
	}
	breach, ok := fb.events[0].Payload.(*compliancepb.ComplianceBreach)
	if !ok {
		t.Fatalf("payload is not a ComplianceBreach: %T", fb.events[0].Payload)
	}
	// The mandate applied is the one in force now, named on the FACT.
	if breach.GetMandateId() != "m1" || breach.GetMandateVersion() != 1 {
		t.Fatalf("breach names mandate %q v%d, want m1 v1", breach.GetMandateId(), breach.GetMandateVersion())
	}
	if breach.GetResult().GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("result status is %v, want BREACH", breach.GetResult().GetStatus())
	}

	// THE OTHER HALF OF #917, and the half that must NOT move. Only the mandate
	// LOOKUP asks the clock. What the breach is STAMPED with is still the
	// observation — every breach must be attributable to what the system saw and
	// when it saw it, and a FACT stamped `now` claims the fund crossed its cap on
	// the day the monitor happened to restart.
	if got := breach.GetDetectedAt().AsTime(); !got.Equal(lastFill) {
		t.Fatalf("breach detected_at is %s, want the observation's as_of %s — stamping it with the "+
			"monitor's clock (%s) makes the breach unattributable to what was observed",
			got, lastFill, clockNow)
	}
	if got := fb.events[0].EventTime; !got.Equal(lastFill) {
		t.Fatalf("breach EventTime is %s, want the observation's as_of %s", got, lastFill)
	}
	if got := breach.GetResult().GetEvaluatedAt().AsTime(); !got.Equal(lastFill) {
		t.Fatalf("result evaluated_at is %s, want the observation's as_of %s — this is what the audit "+
			"producer stamps the decision record's EventTime from", got, lastFill)
	}
	if len(rec.records) != 1 {
		t.Fatalf("expected 1 post-trade decision record, got %d", len(rec.records))
	}
	if got := rec.records[0].Result.GetEvaluatedAt().AsTime(); !got.Equal(lastFill) {
		t.Fatalf("the decision record's result is stamped %s, want the observation's as_of %s", got, lastFill)
	}
	if rec.records[0].Phase != comp.PhasePostTrade || rec.records[0].TenantID != "t1" {
		t.Fatalf("decision record is phase=%v tenant=%q, want post-trade / t1",
			rec.records[0].Phase, rec.records[0].TenantID)
	}
}

// TestAMandateNotYETInForceStillGovernsNothing is the fail-closed direction, and
// it is why the repair is "resolve at now" rather than "resolve with a zero
// as_of".
//
// comp.MandateRegistry.Mandate treats a ZERO asOf as "any version will do" — it
// takes the last one on the key whatever its effective_at. Passing the zero time
// would have made a SCHEDULED mandate govern the book today, which is the
// opposite error: a mandate an operator dated for next month binding a portfolio
// a month early. Now is a real instant and asks the real question, so a mandate
// that has not taken force yet still governs nothing, and "nobody has put one in
// force" stays visible as UNGOVERNED instead of being answered with a mandate
// that is not yet in force.
func TestAMandateNotYETInForceStillGovernsNothing(t *testing.T) {
	fb := &fakeBus{}
	m := NewMonitor(comp.NewEngine(nil), effectiveAtRegistry(t, mandateEffective), nil, NewEmitter(fb), nil, nil)
	// The clock sits BEFORE the mandate takes force; the FACT is current.
	before := mandateEffective.Add(-time.Hour)
	m.now = func() time.Time { return before }

	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, before)); err != nil {
		t.Fatal(err)
	}
	if len(fb.events) != 0 {
		t.Fatalf("a mandate whose effective_at is in the FUTURE governed the book anyway: %d breach FACTs",
			len(fb.events))
	}
}
