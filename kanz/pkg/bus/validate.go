package bus

import (
	"errors"
	"fmt"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// Validate enforces the publish-side envelope invariants from
// kanz-schemas/docs/envelope-policy.md §7 plus the per-class rules from
// docs/event-class-rules.md. Producer.Publish runs this after stamping;
// subscribers should run it on receive too (a defensive check is cheap and
// the envelope is the constitution).
func Validate(env *envelopepb.Envelope) error {
	if env == nil {
		return errors.New("envelope is nil")
	}
	if err := requireNonEmpty(env.EventId, "event_id"); err != nil {
		return err
	}
	if err := requireNonEmpty(env.EventType, "event_type"); err != nil {
		return err
	}
	if env.SchemaVersion == 0 {
		return errors.New("schema_version required (must be >= 1)")
	}
	if env.EnvelopeVersion == 0 {
		return errors.New("envelope_version required")
	}
	if env.EventClass == envelopepb.EventClass_EVENT_CLASS_UNSPECIFIED {
		return errors.New("event_class must not be UNSPECIFIED")
	}
	if err := requireNonEmpty(env.Domain, "domain"); err != nil {
		return err
	}
	if env.EventTime == nil || !env.EventTime.IsValid() {
		return errors.New("event_time required and must be a valid Timestamp")
	}
	if env.IngestionTime == nil || !env.IngestionTime.IsValid() {
		return errors.New("ingestion_time required and must be a valid Timestamp")
	}
	if env.PublishTime == nil || !env.PublishTime.IsValid() {
		return errors.New("publish_time required and must be a valid Timestamp")
	}
	if err := requireNonEmpty(env.CorrelationId, "correlation_id"); err != nil {
		return err
	}
	if err := requireNonEmpty(env.Source, "source"); err != nil {
		return err
	}
	if err := requireNonEmpty(env.ProducerVersion, "producer_version"); err != nil {
		return err
	}
	if err := requireNonEmpty(env.IdempotencyKey, "idempotency_key"); err != nil {
		return err
	}
	if err := requireNonEmpty(env.PayloadSchemaRef, "payload_schema_ref"); err != nil {
		return err
	}
	// FACT events: idempotency_key MUST equal event_id (event-class-rules §1).
	if env.EventClass == envelopepb.EventClass_EVENT_CLASS_FACT && env.IdempotencyKey != env.EventId {
		return errors.New("FACT events require idempotency_key == event_id")
	}
	// QUALITY_FLAG_REPLAYED is set only by replay tooling (EVT-20). A live
	// publisher must reject it before it reaches the bus.
	for _, qf := range env.QualityFlags {
		if qf == envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED {
			return errors.New("live publish must not set QUALITY_FLAG_REPLAYED")
		}
	}
	// producer_sequence is per-(source, partition_key); 0 means N/A. Empty
	// partition_key ⇒ sequence must be 0 (envelope.proto field comment).
	if env.PartitionKey == "" && env.ProducerSequence != 0 {
		return errors.New("producer_sequence must be 0 when partition_key is empty")
	}
	return nil
}

func requireNonEmpty(s, name string) error {
	if s == "" {
		return fmt.Errorf("%s required", name)
	}
	return nil
}
