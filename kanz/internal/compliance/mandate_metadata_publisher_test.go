package compliance_test

import (
	"context"
	"testing"
	"time"

	comp "github.com/eighred/kanz/internal/compliance"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

func metadataIncomplete(m *compliancepb.Mandate, field string) *compliancepb.Mandate {
	bad := proto.Clone(m).(*compliancepb.Mandate)
	switch field {
	case "mandate_id":
		bad.MandateId = ""
	case "version":
		bad.Version = 0
	case "effective_at":
		bad.EffectiveAt = nil
	}
	return bad
}

func metadataRetainedValue(t *testing.T, shape string, current, bad, scheduled *compliancepb.Mandate) string {
	t.Helper()
	var value string
	var err error
	if shape == "legacy" {
		value, err = comp.MarshalMandateValue(bad)
	} else {
		members := []*compliancepb.Mandate{bad, current, scheduled}
		if shape == "bad_last" {
			members = []*compliancepb.Mandate{current, scheduled, bad}
		}
		value, err = comp.MarshalMandateSetValue(members)
	}
	if err != nil {
		t.Fatal(err)
	}
	// The previous value is evidence, including its original whitespace.
	return "\n " + value + "\t"
}

func TestMandateMetadataApprovedPublisherRepairsRetainedMembers(t *testing.T) {
	for _, field := range []string{"mandate_id", "version", "effective_at"} {
		t.Run(field, func(t *testing.T) {
			for _, shape := range []string{"legacy", "bad_first", "bad_last"} {
				t.Run(shape, func(t *testing.T) {
					now := time.Now().UTC()
					// Before epoch so a missing effective_at, incorrectly read as epoch,
					// would replace the current member instead of being pruned harmlessly.
					current := mandateVersion("a", "shared", 1, time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC))
					scheduled := mandateVersion("a", "shared", 20, now.Add(48*time.Hour))
					bad := metadataIncomplete(mandateVersion("a", "shared", 9, now.Add(24*time.Hour)), field)
					value := metadataRetainedValue(t, shape, current, bad, scheduled)
					b := &countingBus{}
					key, topic := comp.MandateConfigKey("a", "shared"), comp.SubjectMandateFor("a", "shared")
					if err := b.Publish(context.Background(), bus.Event{Subject: topic, Payload: &lifecyclepb.ConfigChanged{ConfigKey: key, NewValue: value}}); err != nil {
						t.Fatal(err)
					}
					repair := mandateVersion("a", "shared", 10, now.Add(36*time.Hour))
					const reason = "approved metadata repair with scheduled governance"
					if err := comp.NewPublisher(b, b).Publish(context.Background(), repair, approvalFor(t, repair, reason), reason); err != nil {
						t.Fatal(err)
					}
					if len(b.events) != 2 || b.events[1].Subject != topic || b.events[1].TenantID != "a" || b.events[1].ExpectedLastSubjectSeq == nil || *b.events[1].ExpectedLastSubjectSeq != 1 {
						t.Fatal("approved repair lost destination or exact CAS sequence")
					}
					cc := b.events[1].Payload.(*lifecyclepb.ConfigChanged)
					if cc.PreviousValue != value || cc.ChangedBy == "" || cc.ApprovedBy == "" || cc.ChangedBy == cc.ApprovedBy || cc.Reason != reason {
						t.Fatal("approved repair lost prior bytes or independent approval evidence")
					}
					want := []*compliancepb.Mandate{current, repair, scheduled}
					if shape == "legacy" {
						want = []*compliancepb.Mandate{repair}
					}
					members, err := comp.DecodeMandateValue(cc.NewValue)
					if err != nil || len(members) != len(want) {
						t.Fatalf("repair retained incomplete metadata or lost valid members: %v %v", members, err)
					}
					for i := range want {
						if !proto.Equal(members[i], want[i]) {
							t.Fatalf("repaired member %d differs: %v", i, members[i])
						}
					}
					reg := comp.NewMandateRegistry()
					data, err := proto.Marshal(cc)
					if err != nil {
						t.Fatal(err)
					}
					if err := comp.NewMandateConsumer(reg, nil).Handle(context.Background(), &envelopepb.Envelope{TenantId: "a"}, data); err != nil {
						t.Fatal(err)
					}
					for _, m := range want {
						got, gov, err := reg.Mandate(context.Background(), "a", "shared", m.EffectiveAt.AsTime())
						if err != nil || gov != comp.Governed || !proto.Equal(got, m) {
							t.Fatalf("approved repaired schedule unreadable: got=%v err=%v", got, err)
						}
					}
				})
			}
		})
	}
}
