package compliance_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/pkg/bus"
)

// A MANDATE CHANGE TAKES TWO PEOPLE, AND THE PUBLISHER IS WHERE THAT BECOMES
// UNARGUABLE (#410).
//
// These do not use a broker. The property under test is that nothing is
// published, so a recording Bus that fails the test if it is ever called is a
// sharper instrument than a real one — a refusal that reached the wire and was
// rejected there would still be a refusal in the wrong place.

// countingBus records publishes. Its whole job is to be able to say "you
// published, and you should not have".
//
// IT IS ALSO THE COMPACTED SUBJECT the publisher reads back (#916), because the
// two are the same thing: a MaxMsgsPerSubject=1 stream retains exactly the newest
// message per subject, which is what LastOnSubject answers with here. Making the
// double read from what it recorded is what keeps these tests honest — a
// publisher merging into a source that disagrees with the one it writes would
// pass against a fiction.
type countingBus struct {
	events []bus.Event
	// readErr, when set, is what LastOnSubject returns instead of an answer — a
	// broker that is up enough to publish and not up enough to read.
	readErr error
}

func (c *countingBus) Publish(_ context.Context, e bus.Event) error {
	c.events = append(c.events, e)
	return nil
}

func (c *countingBus) LastOnSubject(_ context.Context, subject string) (*envelopepb.Envelope, []byte, uint64, error) {
	if c.readErr != nil {
		return nil, nil, 0, c.readErr
	}
	for i := len(c.events) - 1; i >= 0; i-- {
		if c.events[i].Subject != subject {
			continue
		}
		payload, err := proto.Marshal(c.events[i].Payload)
		if err != nil {
			return nil, nil, 0, err
		}
		return nil, payload, uint64(i + 1), nil
	}
	return nil, nil, 0, fmt.Errorf("%w %q", bus.ErrNoRetainedMessage, subject)
}

func mandateFixture() *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId:   "M-1",
		TenantId:    "acme",
		PortfolioId: "PF1",
		Version:     3,
		EffectiveAt: timestamppb.New(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)),
	}
}

// approvalFor builds a genuine Approval over m+reason, from two different people.
func approvalFor(t *testing.T, m *compliancepb.Mandate, reason string) dualcontrol.Approval {
	t.Helper()
	digest, err := comp.MandateDigest(m, reason)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	now := time.Now().UTC()
	prop, err := dualcontrol.Propose("p1", dualcontrol.ActMandateChange, "mandate",
		"operator:alice", digest, now, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	a, err := prop.Approve("operator:bob", digest, now)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return a
}

// THE ZERO APPROVAL PUBLISHES NOTHING.
//
// This is the case that makes taking an Approval worth anything. Go lets any
// caller write dualcontrol.Approval{}, so a publisher that merely accepted the
// TYPE would treat "nobody approved this" as approved — which is precisely the
// state the old `changedBy string` signature was in, one keystroke away.
func TestPublish_RefusesAZeroApproval(t *testing.T) {
	b := &countingBus{}
	err := comp.NewPublisher(b, b).Publish(context.Background(), mandateFixture(),
		dualcontrol.Approval{}, "no approval at all")

	if err == nil {
		t.Fatal("a mandate published with a zero Approval — an unapproved change to what " +
			"governs every order in the portfolio")
	}
	if !errors.Is(err, dualcontrol.ErrMalformed) {
		t.Errorf("error = %v, want ErrMalformed — the operator needs to know nothing was "+
			"checked, not that someone self-approved", err)
	}
	if len(b.events) != 0 {
		t.Errorf("%d event(s) published on the refusal path — the refusal must happen before "+
			"anything reaches the broker", len(b.events))
	}
}

// AN APPROVAL FOR A DIFFERENT MANDATE DOES NOT PUBLISH THIS ONE.
//
// The attack the digest exists to stop: propose a defensible mandate, collect the
// second signature, publish a different one. Without this the approval covers the
// REQUEST rather than the VALUE.
func TestPublish_RefusesAnApprovalForADifferentMandate(t *testing.T) {
	approved := mandateFixture()
	approval := approvalFor(t, approved, "Q3 mandate")

	swapped := mandateFixture()
	swapped.Version = 4 // a different mandate, same portfolio

	b := &countingBus{}
	err := comp.NewPublisher(b, b).Publish(context.Background(), swapped, approval, "Q3 mandate")

	if err == nil {
		t.Fatal("a mandate was published under an approval collected for a different one")
	}
	if !errors.Is(err, dualcontrol.ErrPayloadChanged) {
		t.Errorf("error = %v, want ErrPayloadChanged", err)
	}
	if len(b.events) != 0 {
		t.Errorf("%d event(s) published", len(b.events))
	}
}

// AND NEITHER DOES CHANGING THE REASON.
//
// The reason is the sentence that lands in the audit trail. An approval covering
// the rules but not the justification lets the recorded story change after the
// second signature, which makes the trail describe a decision nobody approved.
func TestPublish_RefusesWhenTheReasonChangedAfterApproval(t *testing.T) {
	m := mandateFixture()
	approval := approvalFor(t, m, "Q3 mandate, approved by the IC")

	b := &countingBus{}
	err := comp.NewPublisher(b, b).Publish(context.Background(), m, approval,
		"routine update")

	if err == nil || !errors.Is(err, dualcontrol.ErrPayloadChanged) {
		t.Fatalf("error = %v, want ErrPayloadChanged — the reason is inside the digest", err)
	}
	if len(b.events) != 0 {
		t.Errorf("%d event(s) published", len(b.events))
	}
}

// AN APPROVAL FOR ANOTHER ACT MUST NOT PUBLISH A MANDATE.
//
// The three acts share this package so the rule is written once. That sharing is
// what makes cross-act replay possible, so it is refused explicitly: a pricing
// override approved by two people must not become a mandate change.
func TestPublish_RefusesAnApprovalForAnotherAct(t *testing.T) {
	m := mandateFixture()
	digest, err := comp.MandateDigest(m, "Q3 mandate")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	prop, err := dualcontrol.Propose("p1", dualcontrol.ActPricingOverride, "exception-7",
		"operator:alice", digest, now, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := prop.Approve("operator:bob", digest, now)
	if err != nil {
		t.Fatal(err)
	}

	b := &countingBus{}
	err = comp.NewPublisher(b, b).Publish(context.Background(), m, approval, "Q3 mandate")
	if err == nil {
		t.Fatal("an approval for a PRICING_OVERRIDE published a mandate change")
	}
	if len(b.events) != 0 {
		t.Errorf("%d event(s) published", len(b.events))
	}
}

// THE HAPPY PATH PUBLISHES, AND CARRIES BOTH NAMES.
//
// Without this the refusals above are satisfied by a publisher that refuses
// everything — and the audit value of dual control is entirely in the FACT naming
// two people, so the names are asserted rather than assumed.
func TestPublish_CarriesBothNamesOntoTheFact(t *testing.T) {
	m := mandateFixture()
	const reason = "Q3 mandate, approved by the IC"
	approval := approvalFor(t, m, reason)

	b := &countingBus{}
	if err := comp.NewPublisher(b, b).Publish(context.Background(), m, approval, reason); err != nil {
		t.Fatalf("a valid two-person approval was refused: %v", err)
	}
	if len(b.events) != 1 {
		t.Fatalf("published %d events, want 1", len(b.events))
	}
	cc, ok := b.events[0].Payload.(*lifecyclepb.ConfigChanged)
	if !ok {
		t.Fatalf("payload is %T, want *lifecyclepb.ConfigChanged", b.events[0].Payload)
	}
	if cc.GetChangedBy() != "operator:alice" {
		t.Errorf("changed_by = %q, want the PROPOSER", cc.GetChangedBy())
	}
	if cc.GetApprovedBy() != "operator:bob" {
		t.Errorf("approved_by = %q, want the APPROVER — a FACT that records only one name "+
			"cannot evidence four eyes, and the trail is the whole point", cc.GetApprovedBy())
	}
	if cc.GetChangedBy() == cc.GetApprovedBy() {
		t.Error("changed_by equals approved_by — that is a self-approval wearing a four-eyes trail")
	}
}

// THE PUBLISHER TAKES NO ARGUMENT THAT COULD NAME A LONE ACTOR.
//
// A compile-time statement of the property, kept as a test so it is read as an
// invariant rather than as a coincidence of the current signature. If someone
// adds `changedBy string` back, this stops compiling.
func TestPublish_SignatureAdmitsNoLoneActor(t *testing.T) {
	b := &countingBus{}
	var _ func(context.Context, *compliancepb.Mandate,
		dualcontrol.Approval, string) error = comp.NewPublisher(b, b).Publish
}
