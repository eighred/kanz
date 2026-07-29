package signal_test

import (
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/services/autopilot/internal/signal"
)

func dqeEnvelope(t *testing.T, dqe *observationpb.DataQualityEvent) (*envelopepb.Envelope, []byte) {
	t.Helper()
	payload, err := proto.Marshal(dqe)
	if err != nil {
		t.Fatal(err)
	}
	env := &envelopepb.Envelope{
		EventId:          "ev-1",
		Domain:           "data",
		EventType:        "data.quality.detected",
		PayloadSchemaRef: "observation.v1.DataQualityEvent:1",
		Source:           "integrity",
		EventTime:        timestamppb.New(time.Unix(0, 0).UTC()),
	}
	return env, payload
}

func TestClassifyDataQualityKinds(t *testing.T) {
	crit := observationpb.Severity_SEVERITY_CRITICAL
	cases := []struct {
		name string
		dqe  *observationpb.DataQualityEvent
		want signal.Kind
	}{
		{"gap", &observationpb.DataQualityEvent{Subject: "AAPL", Severity: crit, Detail: &observationpb.DataQualityEvent_Gap{Gap: &observationpb.GapDetail{}}}, signal.KindDataGap},
		{"staleness", &observationpb.DataQualityEvent{Subject: "AAPL", Severity: crit, Detail: &observationpb.DataQualityEvent_Staleness{Staleness: &observationpb.StalenessDetail{}}}, signal.KindStaleness},
		{"drift", &observationpb.DataQualityEvent{Subject: "AAPL", Severity: crit, Detail: &observationpb.DataQualityEvent_Drift{Drift: &observationpb.DriftDetail{}}}, signal.KindDrift},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, payload := dqeEnvelope(t, tc.dqe)
			s, ok := signal.Classify(env, payload)
			if !ok {
				t.Fatal("not classified as a signal")
			}
			if s.Kind != tc.want {
				t.Errorf("kind = %s, want %s", s.Kind, tc.want)
			}
			if s.Severity != signal.SeverityCritical || s.Subject != "AAPL" {
				t.Errorf("severity/subject = %s/%s", s.Severity, s.Subject)
			}
		})
	}
}

func TestClassifyEventTypeConditions(t *testing.T) {
	cases := []struct {
		eventType string
		want      signal.Kind
	}{
		{"data.reconcile.divergence", signal.KindReconcileDivergence},
		{"observation.slo.fastburn", signal.KindSLOBurn},
		{"platform.inference.circuit.open", signal.KindCircuitOpen},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			env := &envelopepb.Envelope{
				EventType:    tc.eventType,
				PartitionKey: "subj",
				EventTime:    timestamppb.New(time.Unix(0, 0).UTC()),
			}
			s, ok := signal.Classify(env, nil)
			if !ok || s.Kind != tc.want {
				t.Fatalf("classify(%s) = %s,%v want %s", tc.eventType, s.Kind, ok, tc.want)
			}
			if s.Subject != "subj" {
				t.Errorf("subject = %s, want subj", s.Subject)
			}
		})
	}
}

func TestClassifyIgnoresNonSignals(t *testing.T) {
	env := &envelopepb.Envelope{
		EventType:        "risk.portfolio.snapshot",
		PayloadSchemaRef: "domain.v1.PortfolioSnapshot:1",
		EventTime:        timestamppb.New(time.Unix(0, 0).UTC()),
	}
	if _, ok := signal.Classify(env, []byte("not-a-dqe")); ok {
		t.Error("ordinary business event should not be a signal")
	}
}
