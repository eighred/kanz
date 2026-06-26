package monitor

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/pkg/bus"
)

var t0 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

type fakeBus struct{ events []bus.Event }

func (f *fakeBus) Publish(_ context.Context, e bus.Event) error {
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
	ctx := context.Background()
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
	if err := m.Handle(context.Background(), &envelopepb.Envelope{}, positionEvent(t, "AAPL", 100, 100000, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if len(fb.events) != 0 {
		t.Fatalf("portfolio with no mandate must not emit, got %d", len(fb.events))
	}
}
