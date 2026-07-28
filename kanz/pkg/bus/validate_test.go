package bus_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/pkg/bus"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

func validEnvelope() *envelopepb.Envelope {
	now := timestamppb.New(time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	return &envelopepb.Envelope{
		EventId:          "evt-1",
		EventType:        "market.equity.trade",
		SchemaVersion:    1,
		EnvelopeVersion:  2,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		Domain:           "market",
		EventTime:        now,
		IngestionTime:    now,
		PublishTime:      now,
		CorrelationId:    "evt-1",
		Source:           "svc/inst",
		ProducerVersion:  "1.0.0",
		IdempotencyKey:   "evt-1", // FACT: equals event_id
		PayloadSchemaRef: "market.v1.MarketDataEvent:1",
		PartitionKey:     "AAPL",
		ProducerSequence: 1,
		TenantId:         "acme",
	}
}

func TestValidateAcceptsCanonicalEnvelope(t *testing.T) {
	if err := bus.Validate(validEnvelope()); err != nil {
		t.Fatalf("canonical envelope rejected: %v", err)
	}
}

func TestValidateRejectsNil(t *testing.T) {
	if err := bus.Validate(nil); err == nil {
		t.Error("Validate accepted nil envelope")
	}
}

func TestValidateRejectsMissingFields(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*envelopepb.Envelope)
	}{
		{"event_id", func(e *envelopepb.Envelope) { e.EventId = "" }},
		{"event_type", func(e *envelopepb.Envelope) { e.EventType = "" }},
		{"schema_version", func(e *envelopepb.Envelope) { e.SchemaVersion = 0 }},
		{"envelope_version", func(e *envelopepb.Envelope) { e.EnvelopeVersion = 0 }},
		{"event_class", func(e *envelopepb.Envelope) { e.EventClass = envelopepb.EventClass_EVENT_CLASS_UNSPECIFIED }},
		{"domain", func(e *envelopepb.Envelope) { e.Domain = "" }},
		{"event_time", func(e *envelopepb.Envelope) { e.EventTime = nil }},
		{"ingestion_time", func(e *envelopepb.Envelope) { e.IngestionTime = nil }},
		{"publish_time", func(e *envelopepb.Envelope) { e.PublishTime = nil }},
		{"correlation_id", func(e *envelopepb.Envelope) { e.CorrelationId = "" }},
		{"source", func(e *envelopepb.Envelope) { e.Source = "" }},
		{"producer_version", func(e *envelopepb.Envelope) { e.ProducerVersion = "" }},
		{"idempotency_key", func(e *envelopepb.Envelope) { e.IdempotencyKey = "" }},
		{"payload_schema_ref", func(e *envelopepb.Envelope) { e.PayloadSchemaRef = "" }},
		{"tenant_id", func(e *envelopepb.Envelope) { e.TenantId = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnvelope()
			tc.mut(env)
			if err := bus.Validate(env); err == nil {
				t.Errorf("Validate accepted envelope missing %s", tc.name)
			}
		})
	}
}

func TestValidateRejectsFactWithMismatchedIdempotencyKey(t *testing.T) {
	env := validEnvelope()
	env.IdempotencyKey = "different-key"
	if err := bus.Validate(env); err == nil {
		t.Error("Validate accepted FACT with idempotency_key != event_id")
	}
}

func TestValidateRejectsReplayedFlag(t *testing.T) {
	env := validEnvelope()
	env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
	if err := bus.Validate(env); err == nil {
		t.Error("Validate accepted live publish with REPLAYED flag")
	}
}

func TestValidateRejectsSequenceWithoutPartitionKey(t *testing.T) {
	env := validEnvelope()
	env.PartitionKey = ""
	env.ProducerSequence = 5
	if err := bus.Validate(env); err == nil {
		t.Error("Validate accepted producer_sequence != 0 with empty partition_key")
	}
}

func TestValidateReplayRequiresFlag(t *testing.T) {
	// Without the flag, ValidateReplay must reject — an un-flagged event on
	// a replay subject means the publisher skipped EVT-20c stamping.
	if err := bus.ValidateReplay(validEnvelope()); err == nil {
		t.Error("ValidateReplay accepted envelope without REPLAYED flag")
	}
}

func TestValidateReplayAcceptsFlaggedEnvelope(t *testing.T) {
	env := validEnvelope()
	env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
	if err := bus.ValidateReplay(env); err != nil {
		t.Errorf("ValidateReplay rejected flagged envelope: %v", err)
	}
}

func TestValidateReplayStillEnforcesFieldRules(t *testing.T) {
	// Replay validation is REPLAYED-aware, but the rest of the envelope
	// contract still applies.
	env := validEnvelope()
	env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
	env.EventId = ""
	if err := bus.ValidateReplay(env); err == nil {
		t.Error("ValidateReplay accepted flagged envelope with missing event_id")
	}
}

func TestValidateRequiresTenantOnLivePath(t *testing.T) {
	env := validEnvelope()
	env.TenantId = ""
	if err := bus.Validate(env); err == nil {
		t.Error("Validate accepted a live envelope with no tenant_id")
	}
}

func TestValidateReplayToleratesMissingTenant(t *testing.T) {
	// MT-01a replay nuance: pre-tenancy events read from old logs carry no
	// tenant_id; ValidateReplay must NOT reject them (the replay path maps them
	// to SystemTenant), so replay determinism is preserved.
	env := validEnvelope()
	env.TenantId = ""
	env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
	if err := bus.ValidateReplay(env); err != nil {
		t.Errorf("ValidateReplay rejected a pre-tenancy event: %v", err)
	}
}

func TestValidateAllowsCommandWithCallerIdempotencyKey(t *testing.T) {
	env := validEnvelope()
	env.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND
	env.IdempotencyKey = "caller-key" // != event_id is fine for COMMAND
	if err := bus.Validate(env); err != nil {
		t.Errorf("Validate rejected COMMAND with distinct idempotency_key: %v", err)
	}
}
