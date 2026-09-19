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
	store               Store
	now                 func() time.Time
	discrepancyObserved func(kind, result string)
}

type ProjectorOption func(*Projector)

// WithDiscrepancyObserver counts delivery outcomes after the durable append.
// Redeliveries count as deliveries, never as distinct unresolved incidents.
func WithDiscrepancyObserver(fn func(kind, result string)) ProjectorOption {
	return func(p *Projector) { p.discrepancyObserved = fn }
}

// NewProjector builds a projector writing to store. now defaults to time.Now and
// is injectable for deterministic tests.
func NewProjector(store Store, now func() time.Time, opts ...ProjectorOption) *Projector {
	if now == nil {
		now = time.Now
	}
	p := &Projector{store: store, now: now}
	for _, opt := range opts {
		opt(p)
	}
	return p
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
	saved, err := p.store.Append(ctx, rec)
	if c.kind == KindVenueDiscrepancy && p.discrepancyObserved != nil {
		result := "failed"
		if err == nil {
			result = "legacy"
			if saved.Kind == KindVenueDiscrepancy {
				result = saved.Attributes["evidence_status"]
			}
		}
		p.discrepancyObserved(c.attrs["discrepancy_type"], result)
	}
	return err
}
