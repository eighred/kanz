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
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

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

// MandateState reads the message a portfolio's mandate subject CURRENTLY
// RETAINS, and the stream sequence it sits at. Satisfied by *bus.NATSClient.
//
// A MANDATE PUBLISH IS A READ-MODIFY-WRITE, and this is the read. The stream
// keeps one message per (tenant, portfolio) subject, and that message carries the
// mandate in force plus every mandate scheduled after it — so a publisher that
// wrote only the mandate it was handed would delete the rest. There is no other
// source for what is on that subject: the operator does not know it, and a
// registry that happens to be resident is a replica's view rather than the
// stream's (#916).
//
// bus.ErrNoRetainedMessage means the subject is empty — a portfolio nobody has
// put under mandate yet. Any other error means the read FAILED, and the two must
// never be collapsed: treating a failed read as an empty subject publishes a set
// that silently drops every version already in force.
type MandateState interface {
	LastOnSubject(ctx context.Context, subject string) (*envelopepb.Envelope, []byte, uint64, error)
}

// Publisher emits mandate changes as ConfigChanged FACTs.
type Publisher struct {
	b     Bus
	state MandateState
	// now is the instant the published set is pruned against — the same bound
	// retainSelectable applies in the registry, applied to what goes on the wire so
	// the stream stops carrying versions no lookup can choose. A field so a test
	// can place a mandate on either side of it; there is no production override,
	// because the only honest answer there is the wall clock.
	now func() time.Time
}

// NewPublisher wraps a Bus and the reader of the state it is about to merge into.
//
// state IS NOT OPTIONAL and a nil one makes every Publish refuse. That is the
// posture #916 asks for: a publisher that cannot see what a portfolio's subject
// already holds cannot write that subject without risking the deletion of the
// mandate in force, and "nothing configured" must not be able to look like
// "checked, and fine". The refusal names the seam, so a deployment that forgets
// to wire it fails on the first mandate change rather than on the next restart of
// something else.
func NewPublisher(b Bus, state MandateState) *Publisher {
	return &Publisher{b: b, state: state, now: func() time.Time { return time.Now().UTC() }}
}

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

// Publish emits a ConfigChanged carrying the mandates a portfolio's subject must
// hold: m, plus every version already on that subject that a lookup can still
// choose. reason is the audit note the approval covers. The FACT is partitioned
// by the config key so a portfolio's mandate changes are totally ordered, and
// event_time is the mandate's effective_at so the version's point-in-time anchor
// travels with it.
//
// # It publishes a SET, and that is what #916 is
//
// The MANDATE stream keeps ONE message per (tenant, portfolio) subject and never
// ages it out. Publishing a single mandate therefore DELETED whatever was there —
// which is correct only while every publish is already in force. Schedule one, as
// the dual-control flow explicitly allows, and the message that got deleted was
// the mandate GOVERNING THE PORTFOLIO RIGHT NOW: the next replica to boot read
// only the future-dated version, found nothing in effect, and resolved the
// portfolio as UNGOVERNED — admitted with no constraints at all under
// OMS_REQUIRE_MANDATE=false. A routine operator action disarmed the pre-trade
// control, and nothing reported a loss, because the message applied cleanly.
//
// So the value carries the whole selectable set and compaction keeping the newest
// message becomes correct BY CONSTRUCTION rather than by luck about dates. The
// set is pruned with retainSelectable, the same bound the registry applies, so
// the stream holds the version in force plus the ones scheduled and nothing else
// — a republish collapses instead of accumulating.
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
// read or serialized, so a mandate that was never approved cannot reach the
// broker even on the error path. Both names go onto the FACT: changed_by is the
// proposer, approved_by is the approver, and the trail is worth having only
// because the two differ by construction.
//
// THE APPROVAL STILL COVERS ONE MANDATE, not the set. The two signatures are for
// the CHANGE the operators actually made; the other members of the set are
// versions already published under their own approvals, carried forward
// mechanically. Hashing the derived set instead would make the digest depend on
// what the stream happened to hold between propose and approve, and every
// approval would fail for a reason that has nothing to do with the decision.
//
// # There is no previous parameter any more
//
// Both callers passed nil and each explained, correctly, that it could not
// honestly name what it superseded — neither read the stream. This one does, so
// previous_value is filled from the value actually being replaced rather than
// invented or omitted.
func (p *Publisher) Publish(ctx context.Context, m *compliancepb.Mandate, approval dualcontrol.Approval, reason string) error {
	if m.GetPortfolioId() == "" || m.GetTenantId() == "" {
		return errors.New("mandate: tenant_id and portfolio_id required")
	}
	// APPROVAL FIRST, before any other work. An unapproved mandate must not be
	// serialized, hashed, read against or partially processed — the only correct
	// response is to refuse having done nothing.
	digest, err := MandateDigest(m, reason)
	if err != nil {
		return err
	}
	if err := approval.Covers(dualcontrol.ActMandateChange, digest); err != nil {
		return fmt.Errorf("mandate %s/%s: %w", m.GetTenantId(), m.GetPortfolioId(), err)
	}
	if p.state == nil {
		return fmt.Errorf("mandate %s/%s: no reader for the mandate subject is wired, so this "+
			"publisher cannot see which mandates the portfolio's subject already carries. "+
			"Publishing blind would overwrite them — including the one in force — on a compacted "+
			"stream that never ages out (#916)", m.GetTenantId(), m.GetPortfolioId())
	}

	subject := SubjectMandateFor(m.GetTenantId(), m.GetPortfolioId())
	held, prevValue, expect, err := p.retained(ctx, subject)
	if err != nil {
		return fmt.Errorf("mandate %s/%s: %w", m.GetTenantId(), m.GetPortfolioId(), err)
	}

	set, err := mergeIntoSet(held, m, p.now())
	if err != nil {
		return fmt.Errorf("mandate %s/%s v%d: %w", m.GetTenantId(), m.GetPortfolioId(), m.GetVersion(), err)
	}
	newValue, err := MarshalMandateSetValue(set)
	if err != nil {
		return err
	}

	key := MandateConfigKey(m.GetTenantId(), m.GetPortfolioId())
	cc := &lifecyclepb.ConfigChanged{
		ConfigKey:     key,
		NewValue:      newValue,
		PreviousValue: prevValue,
		ChangedBy:     approval.Proposer(),
		ApprovedBy:    approval.Approver(),
		Reason:        reason,
	}
	return p.b.Publish(ctx, bus.Event{
		// The PER-PORTFOLIO subject: this is what makes the stream a compacted
		// current-state store, so a booting consumer can arm itself in one read.
		Subject:          subject,
		EventType:        EventTypeMandateChanged,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           Domain,
		EventTime:        m.GetEffectiveAt().AsTime(),
		PartitionKey:     key,
		TenantID:         m.GetTenantId(),
		PayloadSchemaRef: "lifecycle.v1.ConfigChanged:1",
		Payload:          cc,
		// CONDITIONAL ON WHAT WE READ. Everything above is a merge into the value
		// at `expect`; if another approver wrote that subject in between, this
		// publish must be REFUSED rather than land and silently drop their mandate.
		// Zero is a real expectation here — "this portfolio's subject is still
		// empty" — which is why bus.Event takes a pointer.
		ExpectedLastSubjectSeq: &expect,
	})
}

// retained reads the mandates a subject currently holds, the raw value they came
// in (for previous_value), and the sequence to make the write conditional on.
//
// AN EMPTY SUBJECT AND A FAILED READ ARE DIFFERENT ANSWERS. Empty is normal — the
// first mandate for a portfolio — and yields an empty set with expect=0. A failed
// read is refused: continuing would publish a set built from nothing and delete
// whatever is actually there.
//
// AN UNDECODABLE RETAINED VALUE IS NOT A FAILURE, and that asymmetry is
// deliberate. A portfolio whose retained message cannot be parsed is already
// UNGOVERNABLE — every consumer that boots re-reads it and rejects it, and its
// orders are refused (#619) — and republishing is the ONLY repair. Refusing here
// would make the repair unreachable and pin the portfolio out of trading forever,
// which is the fix being worse than the defect. So the new set starts clean. The
// value still travels on previous_value when the ConfigChanged around it decoded
// and only the mandate inside did not — when the FRAME itself is unreadable there
// is no config value to carry, and the sequence is all that survives.
func (p *Publisher) retained(ctx context.Context, subject string) (held []*compliancepb.Mandate, prevValue string, expect uint64, err error) {
	_, payload, seq, err := p.state.LastOnSubject(ctx, subject)
	if errors.Is(err, bus.ErrNoRetainedMessage) {
		return nil, "", 0, nil
	}
	if err != nil {
		return nil, "", 0, fmt.Errorf("reading the mandates already on %q: %w — refusing to publish, "+
			"because a set built from an unread subject deletes the mandate in force", subject, err)
	}
	var cc lifecyclepb.ConfigChanged
	if uErr := proto.Unmarshal(payload, &cc); uErr != nil {
		return nil, "", seq, nil
	}
	prevValue = cc.GetNewValue()
	held, dErr := DecodeMandateValue(prevValue)
	if dErr != nil {
		return nil, prevValue, seq, nil
	}
	return held, prevValue, seq, nil
}

// mergeIntoSet folds m into the mandates a subject already holds and returns what
// the subject must hold next: sorted ascending by (effective_at, version) and
// pruned to the versions a lookup at or after asOf can still choose.
//
// A REPUBLISH OF THE SAME VERSION REPLACES IT rather than appending a twin — the
// operator correcting a mandate keeps its version and moves its terms or its
// date, exactly as MandateRegistry.Put handles the same case.
//
// A MANDATE THE SET CANNOT SELECT IS REFUSED, LOUDLY. That is a mandate dated
// BEHIND the one already in force: retainSelectable would drop it on the way out,
// the published set would not contain it, and the operator would be told their
// dual-signed change was PUBLISHED while nothing about the portfolio changed. A
// publish that cannot take effect is not a publish, and the silent version of
// this is the same class of defect as the one #916 is about.
func mergeIntoSet(held []*compliancepb.Mandate, m *compliancepb.Mandate, asOf time.Time) ([]*compliancepb.Mandate, error) {
	merged := make([]*compliancepb.Mandate, 0, len(held)+1)
	replaced := false
	for _, v := range held {
		if v.GetVersion() == m.GetVersion() {
			merged = append(merged, m)
			replaced = true
			continue
		}
		merged = append(merged, v)
	}
	if !replaced {
		merged = append(merged, m)
	}
	sort.Slice(merged, func(i, j int) bool {
		ti, tj := merged[i].GetEffectiveAt().AsTime(), merged[j].GetEffectiveAt().AsTime()
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return merged[i].GetVersion() < merged[j].GetVersion()
	})
	kept := retainSelectable(merged, asOf)
	for _, v := range kept {
		if v.GetVersion() == m.GetVersion() {
			return kept, nil
		}
	}
	return nil, fmt.Errorf("effective_at %s is BEHIND the mandate already in force for this "+
		"portfolio, so no lookup could ever select it and publishing would change nothing. "+
		"Give the change a date at or after the version it supersedes",
		m.GetEffectiveAt().AsTime().UTC().Format(time.RFC3339))
}
