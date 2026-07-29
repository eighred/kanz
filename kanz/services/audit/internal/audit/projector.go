package audit

import (
	"context"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
)

// Projector is the bus consumer that materializes events into the audit store
// (AUDIT-01a). Its Handle method matches bus.EventHandler, so it subscribes to
// the audited subjects directly. It records EVERY delivered event — recognized
// payloads enriched, the rest generic — because a dropped event is an audit gap.
type Projector struct {
	store Store
	now   func() time.Time
}

// NewProjector builds a projector writing to store. now defaults to time.Now and
// is injectable for deterministic tests.
func NewProjector(store Store, now func() time.Time) *Projector {
	if now == nil {
		now = time.Now
	}
	return &Projector{store: store, now: now}
}

// Handle projects one event into the audit log. It is idempotent (the store
// dedups on event_id), so at-least-once redelivery is safe. A store error is
// returned so the bus can retry / DLQ — the audit projection is durable-grade,
// not lossy-tolerant, despite riding observation-class inputs.
func (p *Projector) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	c := classify(env, payload)
	rec := &Record{
		EventID:       env.GetEventId(),
		CorrelationID: env.GetCorrelationId(),
		CausationID:   env.GetCausationId(),
		Domain:        env.GetDomain(),
		EventType:     env.GetEventType(),
		EventClass:    className(env.GetEventClass()),
		TenantID:      env.GetTenantId(),
		Source:        env.GetSource(),
		OccurredAt:    env.GetEventTime().AsTime(),
		RecordedAt:    p.now().UTC(),
		Kind:          c.kind,
		Summary:       c.summary,
		Attributes:    c.attrs,
		SchemaRef:     env.GetPayloadSchemaRef(),
	}
	_, err := p.store.Append(ctx, rec)
	return err
}
