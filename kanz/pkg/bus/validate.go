package bus

import (
	"errors"
	"fmt"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// Validate enforces the publish-side envelope invariants from
// kanz-schemas/docs/envelope-policy.md §7 plus the per-class rules from
// docs/event-class-rules.md. Producer.Publish runs this after stamping; the
// live-consumer path (the default for bus.Consumer) runs it on receive so a
// replayed event that somehow leaks into a live subject is hard-rejected at
// the bus boundary — that is the live-sink enforcement half of EVT-20c.
//
// Replay-scoped consumers pass bus.WithValidator(bus.ValidateReplay) so they
// can accept REPLAYED events; that path REQUIRES the flag (a non-flagged
// event on a replay subject is a misconfigured publisher and is rejected).
// SystemTenant is the reserved tenant assigned to pre-tenancy events read from
// old durable logs on the replay path (MT-01a). Those events predate the
// envelope tenant_id field and carry the empty string; the replay/consumer path
// maps them to SystemTenant rather than rejecting, so EVT-20 replay determinism
// is preserved. Live publishers must NOT use it — Validate rejects an empty
// tenant on the live path, and a real tenant is always available there.
const SystemTenant = "__system__"

func Validate(env *envelopepb.Envelope) error {
	if err := validateEnvelopeFields(env); err != nil {
		return err
	}
	// tenant_id is required on the LIVE path only (MT-01a). It is enforced here,
	// not in validateEnvelopeFields, so ValidateReplay stays tolerant of
	// pre-tenancy events (which the replay path maps to SystemTenant).
	if err := requireNonEmpty(env.TenantId, "tenant_id"); err != nil {
		return err
	}
	return rejectReplayed(env)
}

// ValidateReplay validates an envelope against the same field/class rules as
// Validate, but requires QUALITY_FLAG_REPLAYED to be present rather than
// rejecting it. Pass this to bus.WithValidator on replay-scoped consumers.
// An un-flagged event on a replay subject means a publisher skipped the
// stamping step — that publisher is broken and the event must not be
// dispatched as if it were live.
func ValidateReplay(env *envelopepb.Envelope) error {
	if err := validateEnvelopeFields(env); err != nil {
		return err
	}
	return requireReplayed(env)
}

func validateEnvelopeFields(env *envelopepb.Envelope) error {
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
	// producer_sequence is per-(source, partition_key); 0 means N/A. Empty
	// partition_key ⇒ sequence must be 0 (envelope.proto field comment).
	if env.PartitionKey == "" && env.ProducerSequence != 0 {
		return errors.New("producer_sequence must be 0 when partition_key is empty")
	}
	return nil
}

// rejectReplayed is the live-sink guard: QUALITY_FLAG_REPLAYED is set only
// by replay tooling (EVT-20). A live publisher must reject it before it
// reaches the bus; a live consumer hard-rejects on receive so a replay event
// that leaked past namespace isolation (EVT-20b) is DLQ'd, not dispatched.
func rejectReplayed(env *envelopepb.Envelope) error {
	for _, qf := range env.QualityFlags {
		if qf == envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED {
			return errors.New("QUALITY_FLAG_REPLAYED forbidden on live path (only replay-scoped consumers accept it)")
		}
	}
	return nil
}

// requireReplayed is the replay-scoped consumer guard: every event on a
// replay subject must carry the flag. An un-flagged event here means the
// publisher skipped stamping — defect, not data.
func requireReplayed(env *envelopepb.Envelope) error {
	for _, qf := range env.QualityFlags {
		if qf == envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED {
			return nil
		}
	}
	return errors.New("replay-scoped consumer received event without QUALITY_FLAG_REPLAYED")
}

func requireNonEmpty(s, name string) error {
	if s == "" {
		return fmt.Errorf("%s required", name)
	}
	return nil
}
