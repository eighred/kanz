package audit_test

// AUDIT-01e: the three board-named behaviors as one end-to-end flow over the
// real projector + store + lineage + report + chain (nothing mocked):
//  1. reconstruct a decision to its inputs in <1min,
//  2. a tamper attempt is detected by chain verification,
//  3. a sample regulatory report is generated end-to-end.

import (
	"context"
	"testing"
	"time"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	observationpb "github.com/kanz-eng/kanz-schemas-go/observation/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/services/audit/internal/audit"
	"github.com/kanz-eng/kanz/services/audit/internal/lineage"
	"github.com/kanz-eng/kanz/services/audit/internal/report"
)

func ev(id, etype, domain, caus, ref string, class envelopepb.EventClass) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventId: id, EventType: etype, Domain: domain, EventClass: class,
		CorrelationId: "trade-cascade", CausationId: caus, PayloadSchemaRef: ref,
		Source: "test/1", EventTime: timestamppb.New(time.Unix(1700000000, 0)),
	}
}

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// project a realistic cascade: market tick -> model decision -> order command
// -> command outcome, all sharing one correlation_id, linked by causation_id.
func project(t *testing.T, p *audit.Projector) {
	t.Helper()
	ctx := context.Background()
	steps := []struct {
		env     *envelopepb.Envelope
		payload []byte
	}{
		{ev("tick", "market.equity.trade", "market", "", "market.v1.Trade:1", envelopepb.EventClass_EVENT_CLASS_FACT), nil},
		{ev("signal", "risk.signal.raised", "risk", "tick", "observation.v1.DecisionLog:1", envelopepb.EventClass_EVENT_CLASS_OBSERVATION),
			marshal(t, &observationpb.DecisionLog{Decider: "model:momentum@2", Summary: "score 0.87 over 0.80 -> BUY"})},
		{ev("order", "trade.order.submit", "trade", "signal", "command.v1.Order:1", envelopepb.EventClass_EVENT_CLASS_COMMAND), nil},
		{ev("outcome", "trade.order.outcome", "trade", "order", "command.v1.CommandOutcome:1", envelopepb.EventClass_EVENT_CLASS_FACT),
			marshal(t, &commandpb.CommandOutcome{Status: commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, Reason: "filled 100 @ 10.25"})},
	}
	for _, s := range steps {
		if err := p.Handle(ctx, s.env, s.payload); err != nil {
			t.Fatalf("project %s: %v", s.env.EventId, err)
		}
	}
}

func TestAuditEndToEnd(t *testing.T) {
	ctx := context.Background()
	store := audit.NewMemory()
	project(t, audit.NewProjector(store, time.Now))

	// (1) Reconstruct the command outcome back to the market tick that triggered
	// it — and prove it is fast (the <1min budget; in practice microseconds).
	start := time.Now()
	lin, err := lineage.Reconstruct(ctx, store, "outcome")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Minute {
		t.Fatalf("reconstruction took %v, over the 1min budget", elapsed)
	}
	wantChain := []string{"tick", "signal", "order", "outcome"}
	if len(lin.Ancestry) != len(wantChain) {
		t.Fatalf("ancestry=%d want %d", len(lin.Ancestry), len(wantChain))
	}
	for i, id := range wantChain {
		if lin.Ancestry[i].EventID != id {
			t.Fatalf("ancestry[%d]=%s want %s", i, lin.Ancestry[i].EventID, id)
		}
	}
	// The decision is reconstructed with its reasoning intact.
	if lin.Ancestry[1].Kind != audit.KindDecision || lin.Ancestry[1].Attributes["decider"] != "model:momentum@2" {
		t.Fatalf("decision not reconstructed: %+v", lin.Ancestry[1])
	}

	// (3) Generate a sample regulatory report end-to-end — verified attestation.
	rep, err := report.Generate(ctx, store, report.BuiltIns()["command-outcomes"], time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Records) != 1 || rep.Records[0].Summary != "filled 100 @ 10.25" {
		t.Fatalf("report body wrong: %+v", rep.Records)
	}
	if !rep.Integrity.Verified {
		t.Fatalf("report attestation not verified: %+v", rep.Integrity)
	}
	if _, err := rep.RenderCSV(); err != nil {
		t.Fatalf("render report: %v", err)
	}

	// (2) Tamper: silently rewrite a stored record. Chain verification catches it.
	all, _ := store.All(ctx)
	all[1].Summary = "score 0.10 -> SELL" // forge the decision after the fact
	att, err := report.Verify(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if att.Verified {
		t.Fatal("tampered audit log passed verification")
	}
}
