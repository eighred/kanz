package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/pkg/bus"
)

var t0 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// testCtx stands in for what bus.Consumer stashes onto a handler's ctx from the
// inbound envelope (pkg/bus/context.go's WithTenantID) before the handler ever
// runs. Monitor.Handle is only ever wired through SubscribeBroadcast in
// production, so a direct Handle(ctx, ...) call in a test bypasses the one thing
// that supplies the tenant — and must supply it itself.
func testCtx() context.Context {
	return bus.WithTenantID(context.Background(), "test-tenant")
}

// fakeBus records published events for assertion. Publish enforces the
// caller-facing preconditions the real bus.Producer enforces (pkg/bus/producer.go
// publish/stamp; pkg/bus/validate.go Validate) — the fields a handler, not the
// producer, is responsible for getting right. It deliberately does NOT run the
// full bus.Validate: event_id, schema_version, publish_time and the rest are
// stamped by Producer.stamp deterministically and cannot be wrong from a
// handler's side.
//
// The tenant rule is the one that matters here. Emitter.EmitBreach sets no
// Event.TenantID — it relies entirely on the ctx tenant — and compliance's
// producer sets no ProducerConfig.Tenant fallback, so an empty tenant is a hard
// publish failure in production. These tests used to pass a bare
// context.Background() and go green anyway, purely because this double accepted
// what the broker would reject. That is exactly how a Critical bug reached a
// live cluster from the OMS with its whole suite green: a double that accepts
// what the real broker rejects certifies nothing.
type fakeBus struct{ events []bus.Event }

func (f *fakeBus) Publish(ctx context.Context, e bus.Event) error {
	if e.Payload == nil {
		return errors.New("bus: Event.Payload required")
	}
	if e.EventTime.IsZero() {
		return errors.New("Event.EventTime required")
	}
	if e.EventClass == envelopepb.EventClass_EVENT_CLASS_COMMAND && e.IdempotencyKey == "" {
		return errors.New("idempotency_key required for COMMAND events")
	}
	// Tenant precedence mirrors stamp (Event.TenantID > ctx), then Validate's
	// live-path rejection of an empty tenant. compliance configures no
	// ProducerConfig.Tenant, so there is no third fallback to model.
	tenant := e.TenantID
	if tenant == "" {
		tenant = bus.TenantIDFromContext(ctx)
	}
	if tenant == "" {
		return errors.New("envelope validation: tenant_id required")
	}
	f.events = append(f.events, e)
	return nil
}

type recordingRecorder struct{ records []comp.DecisionRecord }

func (r *recordingRecorder) Record(_ context.Context, rec comp.DecisionRecord) error {
	r.records = append(r.records, rec)
	return nil
}

func dec(c int64, e int32) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: c, Exponent: e} }

func positionEvent(t *testing.T, inst string, qty, mv int64, asOf time.Time) []byte {
	t.Helper()
	b, err := proto.Marshal(&domainpb.PositionState{
		PortfolioId:  "p1",
		InstrumentId: inst,
		Quantity:     dec(qty, 0),
		MarketValue:  &commonpb.Money{Amount: dec(mv, 0), CurrencyCode: "USD"},
		AsOf:         timestamppb.New(asOf),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func concentrationRegistry() *comp.MandateRegistry {
	reg := comp.NewMandateRegistry()
	reg.Put(&compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		EffectiveAt: timestamppb.New(t0),
		Rules: []*compliancepb.Rule{{
			RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				MaxWeight: dec(60, -2), // 60%
			}},
		}},
	})
	return reg
}

func TestMonitor_PassiveBreachEmitsAndEscalates(t *testing.T) {
	fb := &fakeBus{}
	rec := &recordingRecorder{}
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(), nil, NewEmitter(fb), rec, nil)
	ctx := testCtx()
	env := &envelopepb.Envelope{}

	// 1) AAPL alone ⇒ 100% > 60% (seed breach, POSITION_CHANGE).
	if err := m.Handle(ctx, env, positionEvent(t, "AAPL", 100, 100000, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	// 2) MSFT 100k ⇒ AAPL 50% ⇒ book clears to PASS.
	if err := m.Handle(ctx, env, positionEvent(t, "MSFT", 100, 100000, t0.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	// 3) AAPL re-marked up to 200k with SAME quantity ⇒ 66.7% > 60%: a passive,
	//    market-move-induced breach.
	if err := m.Handle(ctx, env, positionEvent(t, "AAPL", 100, 200000, t0.Add(3*time.Minute))); err != nil {
		t.Fatal(err)
	}
	// 4) AAPL up further, still breaching ⇒ no new emit (no transition).
	if err := m.Handle(ctx, env, positionEvent(t, "AAPL", 100, 250000, t0.Add(4*time.Minute))); err != nil {
		t.Fatal(err)
	}

	if len(fb.events) != 2 {
		t.Fatalf("expected 2 breach emits (seed + market-move), got %d", len(fb.events))
	}
	last := fb.events[1]
	if last.Subject != comp.SubjectBreach || last.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Fatalf("breach must be a FACT on %s, got subject=%s class=%v", comp.SubjectBreach, last.Subject, last.EventClass)
	}
	breach, ok := last.Payload.(*compliancepb.ComplianceBreach)
	if !ok {
		t.Fatalf("payload is not a ComplianceBreach: %T", last.Payload)
	}
	if breach.GetTrigger() != compliancepb.BreachTrigger_BREACH_TRIGGER_MARKET_MOVE {
		t.Fatalf("re-mark with unchanged quantity should be MARKET_MOVE, got %v", breach.GetTrigger())
	}
	if breach.GetResult().GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("breach result must carry BREACH status")
	}
	// Escalation feed: every breach is also recorded as a post-trade decision.
	var postTrade int
	for _, r := range rec.records {
		if r.Phase == comp.PhasePostTrade {
			postTrade++
		}
	}
	if postTrade != 2 {
		t.Fatalf("expected 2 post-trade decision records, got %d", postTrade)
	}
}

func TestMonitor_NoMandateNoEmit(t *testing.T) {
	fb := &fakeBus{}
	m := NewMonitor(comp.NewEngine(nil), comp.NewMandateRegistry(), nil, NewEmitter(fb), nil, nil)
	if err := m.Handle(testCtx(), &envelopepb.Envelope{}, positionEvent(t, "AAPL", 100, 100000, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if len(fb.events) != 0 {
		t.Fatalf("portfolio with no mandate must not emit, got %d", len(fb.events))
	}
}
