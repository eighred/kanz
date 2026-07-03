package audit

import (
	"context"
	"testing"
	"time"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	observationpb "github.com/kanz-eng/kanz-schemas-go/observation/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/internal/audit/chain"
)

func env(id, etype, domain string, class envelopepb.EventClass) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventId: id, EventType: etype, Domain: domain, EventClass: class,
		CorrelationId: "corr", Source: "test/1", EventTime: timestamppb.New(time.Unix(1700000000, 0)),
	}
}

func withRef(e *envelopepb.Envelope, ref string) *envelopepb.Envelope {
	e.PayloadSchemaRef = ref
	return e
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClassify(t *testing.T) {
	authz := mustMarshal(t, &observationpb.DecisionLog{Decider: "authz:policy", Summary: "DENY read", Attributes: map[string]string{"decision": "deny"}})
	decision := mustMarshal(t, &observationpb.DecisionLog{Decider: "model:vol@1", Summary: "score 0.9 -> BUY"})
	metric := mustMarshal(t, &observationpb.MetricObservation{Name: "latency", Value: 1})
	dq := mustMarshal(t, &observationpb.DataQualityEvent{Subject: "market.equity.trade", Severity: observationpb.Severity_SEVERITY_CRITICAL, Summary: "gap"})
	outcome := mustMarshal(t, &commandpb.CommandOutcome{Status: commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, Reason: "filled"})

	cases := []struct {
		name    string
		env     *envelopepb.Envelope
		payload []byte
		want    Kind
		summary string
	}{
		{"authz", env("1", auth.AuthzDecisionEventType, "platform", envelopepb.EventClass_EVENT_CLASS_OBSERVATION), authz, KindAuthzDecision, "DENY read"},
		{"decision", withRef(env("2", "risk.signal", "risk", envelopepb.EventClass_EVENT_CLASS_OBSERVATION), "observation.v1.DecisionLog:1"), decision, KindDecision, "score 0.9 -> BUY"},
		{"metric-is-generic", env("3", "metric.latency", "risk", envelopepb.EventClass_EVENT_CLASS_OBSERVATION), metric, KindEvent, ""},
		{"data-quality", env("4", "data.market.gap_detected", "data", envelopepb.EventClass_EVENT_CLASS_OBSERVATION), dq, KindDataQuality, "gap"},
		{"command-outcome", env("5", "trade.order.outcome", "trade", envelopepb.EventClass_EVENT_CLASS_FACT), outcome, KindCommandOutcome, "filled"},
		{"command", env("6", "trade.order.submit", "trade", envelopepb.EventClass_EVENT_CLASS_COMMAND), nil, KindCommand, ""},
		{"unknown-fact", env("7", "risk.exposure.recomputed", "risk", envelopepb.EventClass_EVENT_CLASS_FACT), nil, KindEvent, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(c.env, c.payload)
			if got.kind != c.want {
				t.Fatalf("kind=%q want %q", got.kind, c.want)
			}
			if c.summary != "" && got.summary != c.summary {
				t.Fatalf("summary=%q want %q", got.summary, c.summary)
			}
		})
	}
}

func TestProjectorAppendsAndChains(t *testing.T) {
	store := NewMemory()
	p := NewProjector(store, func() time.Time { return time.Unix(1700000001, 0) })
	ctx := context.Background()

	for i, e := range []*envelopepb.Envelope{
		env("e1", "risk.a", "risk", envelopepb.EventClass_EVENT_CLASS_FACT),
		env("e2", "risk.b", "risk", envelopepb.EventClass_EVENT_CLASS_FACT),
	} {
		if err := p.Handle(ctx, e, nil); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}

	all, _ := store.All(ctx)
	if len(all) != 2 {
		t.Fatalf("len=%d want 2", len(all))
	}
	if all[0].Seq != 1 || all[1].Seq != 2 {
		t.Fatalf("seq=%d,%d", all[0].Seq, all[1].Seq)
	}
	links := []chain.Link{all[0], all[1]}
	if idx, err := chain.Verify(links); err != nil {
		t.Fatalf("store chain invalid at %d: %v", idx, err)
	}
}

func TestProjectorIdempotent(t *testing.T) {
	store := NewMemory()
	p := NewProjector(store, time.Now)
	ctx := context.Background()
	e := env("dup", "risk.a", "risk", envelopepb.EventClass_EVENT_CLASS_FACT)

	for i := 0; i < 3; i++ {
		if err := p.Handle(ctx, e, nil); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := store.All(ctx)
	if len(all) != 1 {
		t.Fatalf("redelivery duplicated: len=%d want 1", len(all))
	}
}

func TestMemoryQueryFilters(t *testing.T) {
	store := NewMemory()
	p := NewProjector(store, time.Now)
	ctx := context.Background()
	_ = p.Handle(ctx, env("a", "data.x.gap_detected", "data", envelopepb.EventClass_EVENT_CLASS_OBSERVATION),
		mustMarshal(t, &observationpb.DataQualityEvent{Subject: "x", Severity: observationpb.Severity_SEVERITY_WARNING, Summary: "g"}))
	_ = p.Handle(ctx, env("b", "risk.y", "risk", envelopepb.EventClass_EVENT_CLASS_FACT), nil)

	got, _ := store.Query(ctx, Filter{Kind: KindDataQuality})
	if len(got) != 1 || got[0].EventID != "a" {
		t.Fatalf("kind filter got %d records", len(got))
	}
}
