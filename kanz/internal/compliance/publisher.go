// The COMP-01f mandate-lifecycle publish surface: a tenant-scoped
// mandate change is published as a lifecycle.v1.ConfigChanged FACT (the platform
// already models config changes this way), keyed per (tenant, portfolio) and
// carrying the serialized mandate version. Replaying the ConfigChanged stream
// reconstructs each portfolio's full mandate history, so the engine resolves the
// version in effect at any point in time (MandateRegistry) — "which mandate
// applied when" is point-in-time correct.
package compliance

import (
	"context"
	"errors"
	"fmt"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/pkg/bus"
)

// Bus is the publish surface — satisfied by *bus.Producer.
type Bus interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Publisher emits mandate changes as ConfigChanged FACTs.
type Publisher struct{ b Bus }

// NewPublisher wraps a Bus.
func NewPublisher(b Bus) *Publisher { return &Publisher{b: b} }

// MandateDigest is what an approval for a mandate change covers: the canonical
// serialization of the mandate itself, plus the reason.
//
// ONE IMPLEMENTATION, called by both sides. The proposer hashes what it is
// asking for and the approver hashes what it is about to publish; if those two
// were computed by separate code the digest check would compare two conventions
// rather than two payloads, and would pass or fail for the wrong reason.
//
// The REASON is inside the digest deliberately. It is the sentence that goes in
// the audit trail, and an approval that covered the rules but not the
// justification would let the recorded story change after the second signature.
func MandateDigest(m *compliancepb.Mandate, reason string) (string, error) {
	value, err := MarshalMandateValue(m)
	if err != nil {
		return "", err
	}
	return dualcontrol.Digest(string(dualcontrol.ActMandateChange), value, reason), nil
}

// Publish emits a ConfigChanged carrying the new mandate version. previous is
// the version being superseded (nil for the first), recorded as
// previous_value for audit/rollback. reason is the audit note the approval
// covers. The FACT is partitioned by the config key so a portfolio's mandate
// changes are totally ordered, and event_time is the mandate's effective_at so
// the version's point-in-time anchor travels with it.
//
// # It takes an Approval, not two names, and that is the point (#410)
//
// This used to take `changedBy string`: one flag on one CLI invocation changed
// the limits every order in a portfolio is checked against. A mandate is the
// control — "one person can change a mandate" is #410's own wording — so a
// unilateral change is now IMPOSSIBLE TO EXPRESS here rather than discouraged.
// There is no argument to pass.
//
// The refusal happens at the signature and again in Covers, before anything is
// serialized, so a mandate that was never approved cannot reach the broker even
// on the error path. Both names go onto the FACT: changed_by is the proposer,
// approved_by is the approver, and the trail is worth having only because the
// two differ by construction.
func (p *Publisher) Publish(ctx context.Context, m *compliancepb.Mandate, previous *compliancepb.Mandate, approval dualcontrol.Approval, reason string) error {
	if m.GetPortfolioId() == "" || m.GetTenantId() == "" {
		return errors.New("mandate: tenant_id and portfolio_id required")
	}
	// APPROVAL FIRST, before any other work. An unapproved mandate must not be
	// serialized, hashed or partially processed — the only correct response is
	// to refuse having done nothing.
	digest, err := MandateDigest(m, reason)
	if err != nil {
		return err
	}
	if err := approval.Covers(dualcontrol.ActMandateChange, digest); err != nil {
		return fmt.Errorf("mandate %s/%s: %w", m.GetTenantId(), m.GetPortfolioId(), err)
	}
	changedBy := approval.Proposer()
	newValue, err := MarshalMandateValue(m)
	if err != nil {
		return err
	}
	prevValue := ""
	if previous != nil {
		if prevValue, err = MarshalMandateValue(previous); err != nil {
			return err
		}
	}
	key := MandateConfigKey(m.GetTenantId(), m.GetPortfolioId())
	cc := &lifecyclepb.ConfigChanged{
		ConfigKey:     key,
		NewValue:      newValue,
		PreviousValue: prevValue,
		ChangedBy:     changedBy,
		ApprovedBy:    approval.Approver(),
		Reason:        reason,
	}
	return p.b.Publish(ctx, bus.Event{
		// The PER-PORTFOLIO subject: this is what makes the stream a compacted
		// current-state store, so a booting consumer can arm itself in one read.
		Subject:          SubjectMandateFor(m.GetTenantId(), m.GetPortfolioId()),
		EventType:        EventTypeMandateChanged,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           Domain,
		EventTime:        m.GetEffectiveAt().AsTime(),
		PartitionKey:     key,
		TenantID:         m.GetTenantId(),
		PayloadSchemaRef: "lifecycle.v1.ConfigChanged:1",
		Payload:          cc,
	})
}
