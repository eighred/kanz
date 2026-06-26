// Package mandate is the COMP-01f mandate-lifecycle surface: a tenant-scoped
// mandate change is published as a lifecycle.v1.ConfigChanged FACT (the platform
// already models config changes this way), keyed per (tenant, portfolio) and
// carrying the serialized mandate version. Replaying the ConfigChanged stream
// reconstructs each portfolio's full mandate history, so the engine resolves the
// version in effect at any point in time (comp.MandateRegistry) — "which mandate
// applied when" is point-in-time correct.
package mandate

import (
	"context"
	"errors"

	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/kanz-eng/kanz-schemas-go/lifecycle/v1"

	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// Bus is the publish surface — satisfied by *bus.Producer.
type Bus interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Publisher emits mandate changes as ConfigChanged FACTs.
type Publisher struct{ b Bus }

// NewPublisher wraps a Bus.
func NewPublisher(b Bus) *Publisher { return &Publisher{b: b} }

// Publish emits a ConfigChanged carrying the new mandate version. previous is
// the version being superseded (nil for the first), recorded as
// previous_value for audit/rollback. changedBy is the principal making the
// change ("{type}:{id}"); reason is an optional audit note. The FACT is
// partitioned by the config key so a portfolio's mandate changes are totally
// ordered, and event_time is the mandate's effective_at so the version's
// point-in-time anchor travels with it.
func (p *Publisher) Publish(ctx context.Context, m *compliancepb.Mandate, previous *compliancepb.Mandate, changedBy, reason string) error {
	if m.GetPortfolioId() == "" || m.GetTenantId() == "" {
		return errors.New("mandate: tenant_id and portfolio_id required")
	}
	newValue, err := comp.MarshalMandateValue(m)
	if err != nil {
		return err
	}
	prevValue := ""
	if previous != nil {
		if prevValue, err = comp.MarshalMandateValue(previous); err != nil {
			return err
		}
	}
	key := comp.MandateConfigKey(m.GetTenantId(), m.GetPortfolioId())
	cc := &lifecyclepb.ConfigChanged{
		ConfigKey:     key,
		NewValue:      newValue,
		PreviousValue: prevValue,
		ChangedBy:     changedBy,
		Reason:        reason,
	}
	return p.b.Publish(ctx, bus.Event{
		Subject:          comp.SubjectMandateChanged,
		EventType:        comp.EventTypeMandateChanged,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           comp.Domain,
		EventTime:        m.GetEffectiveAt().AsTime(),
		PartitionKey:     key,
		TenantID:         m.GetTenantId(),
		PayloadSchemaRef: "lifecycle.v1.ConfigChanged:1",
		Payload:          cc,
	})
}
