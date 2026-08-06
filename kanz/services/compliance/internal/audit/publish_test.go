// THE COMPLIANCE DECISION RECORD, DRIVEN THROUGH A REAL PRODUCER (#245).
//
// This package had no test of any kind. What it publishes is the audit trail for
// every compliance verdict — pre-trade pass/reject and post-trade breach — which
// AUDIT-01 projects into the tamper-evident store.
//
// THE ABSENCE MATTERED MORE HERE THAN ANYWHERE ELSE ON #245's LIST, because
// Record is BEST-EFFORT BY DESIGN: both enforcement points call it and log the
// error rather than failing the decision, on the AUTH-01d stance that an
// audit-sink outage must not become a trading outage. That is the right call —
// and it means a publish that fails EVERY TIME is indistinguishable, from
// outside, from an audit sink that is simply quiet. Trading continues; nothing
// is recorded; every probe stays green.
//
// Tier-B: a real bus.Producer over a fake bus.Client, so bus.Validate runs and
// the assertions are on the wire bytes.
package audit

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/pkg/bus"
)

// captureClient records the framed wire bytes — the Tier-B helper from
// internal/risk/publish and services/accounting/internal/cashmove.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

var evaluatedAt = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

func newRecorder(t *testing.T) (*BusRecorder, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "compliance",
		ProducerVersion: "test",
		Tenant:          "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return NewBusRecorder(prod, nil), cc
}

func preTradeDecision() comp.DecisionRecord {
	return comp.DecisionRecord{
		Phase:   comp.PhasePreTrade,
		Allowed: false,
		OrderID: "o-1",
		Issuer:  "user:alice",
		Result: &compliancepb.ComplianceResult{
			PortfolioId:    "pf-1",
			MandateId:      "m-1",
			MandateVersion: 3,
			Status:         compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
			EvaluatedAt:    timestamppb.New(evaluatedAt),
		},
	}
}

// THE ONE THAT MATTERS. If this fails, no compliance decision is recorded at
// all — and because Record is best-effort, nothing upstream notices.
func TestRecordPublishesAValidObservation(t *testing.T) {
	r, cc := newRecorder(t)

	if err := r.Record(context.Background(), preTradeDecision()); err != nil {
		t.Fatalf("Record: %v\n\nEvery compliance decision fails to publish. Record is best-effort by "+
			"design, so both enforcement points log this and carry on: trading continues, the audit "+
			"trail records NOTHING, and every probe stays green.", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}

	env, payload, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("the emitted envelope fails Validate: %v", err)
	}
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_OBSERVATION {
		t.Errorf("event_class = %v, want OBSERVATION — a compliance verdict is an observation, not a "+
			"FACT the book folds", got)
	}
	if got := env.GetEventType(); got != DecisionEventType {
		t.Errorf("event_type = %q, want %q — AUDIT-01 selects compliance decisions off the "+
			"observation stream BY THIS FIELD", got, DecisionEventType)
	}
	if got := env.GetDomain(); got != DecisionDomain {
		t.Errorf("domain = %q, want %q", got, DecisionDomain)
	}
	if got := env.GetTenantId(); got != "acme" {
		t.Errorf("tenant_id = %q, want acme", got)
	}
	if got := env.GetPayloadSchemaRef(); got != "observation.v1.DecisionLog:1" {
		t.Errorf("payload_schema_ref = %q", got)
	}
	if got := string(cc.sent[0].Key); got != "pf-1" {
		t.Errorf("partition key = %q, want the portfolio id — a portfolio's decisions must stay "+
			"ordered against each other", got)
	}

	var log observationpb.DecisionLog
	if err := proto.Unmarshal(payload, &log); err != nil {
		t.Fatalf("unmarshal the marshalled payload: %v", err)
	}
	if log.GetDecider() != Decider {
		t.Errorf("decider = %q, want %q", log.GetDecider(), Decider)
	}
}

// THE EVENT TIME IS THE DECISION'S OWN, never ingest time. The audit store is
// queried by when a decision was MADE; stamping the publish time would make a
// replayed or delayed decision look like it happened now.
func TestRecordCarriesTheEvaluationTime(t *testing.T) {
	r, cc := newRecorder(t)
	if err := r.Record(context.Background(), preTradeDecision()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if got := env.GetEventTime().AsTime(); !got.Equal(evaluatedAt) {
		t.Errorf("event_time = %s, want the result's evaluated_at %s — the audit store is queried by "+
			"when the decision was MADE", got, evaluatedAt)
	}
}

// A post-trade breach records too, and carries its trigger. Pre-trade and
// post-trade take different branches through BuildDecisionLog, so proving one
// says nothing about the other.
func TestRecordPublishesAPostTradeBreach(t *testing.T) {
	r, cc := newRecorder(t)
	rec := preTradeDecision()
	rec.Phase = comp.PhasePostTrade
	rec.OrderID = ""
	rec.Trigger = "position_changed"

	if err := r.Record(context.Background(), rec); err != nil {
		t.Fatalf("Record (post-trade): %v", err)
	}
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("the post-trade envelope fails Validate: %v", err)
	}
}
