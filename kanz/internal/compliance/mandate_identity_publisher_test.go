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

func TestMandateIdentityPublisherRepairsFutureForeignMembers(t *testing.T) {
	for _, field := range []string{"foreign-tenant", "foreign-portfolio", "missing-tenant", "missing-portfolio"} {
		t.Run(field, func(t *testing.T) {
			now := time.Now().UTC()
			current := mandateVersion("a", "shared", 1, now.Add(-time.Hour))
			scheduled := mandateVersion("a", "shared", 20, now.Add(48*time.Hour))
			foreign := mandateVersion("a", "shared", 9, now.Add(24*time.Hour))
			switch field {
			case "foreign-tenant":
				foreign.TenantId = "b"
			case "foreign-portfolio":
				foreign.PortfolioId = "another"
			case "missing-tenant":
				foreign.TenantId = ""
			case "missing-portfolio":
				foreign.PortfolioId = ""
			}
			value, err := comp.MarshalMandateSetValue([]*compliancepb.Mandate{current, foreign, scheduled})
			if err != nil {
				t.Fatal(err)
			}
			key := comp.MandateConfigKey("a", "shared")
			b := &countingBus{}
			// Model the already-retained malformed value; only the repair below
			// travels through the ordinary approved Publisher.
			if err := b.Publish(context.Background(), bus.Event{Subject: comp.SubjectMandateFor("a", "shared"), Payload: &lifecyclepb.ConfigChanged{ConfigKey: key, NewValue: value}}); err != nil {
				t.Fatal(err)
			}
			repair := mandateVersion("a", "shared", 10, now.Add(-time.Second))
			const reason = "scoped current repair preserving scheduled governance"
			if err := comp.NewPublisher(b, b).Publish(context.Background(), repair, approvalFor(t, repair, reason), reason); err != nil {
				t.Fatal(err)
			}
			if len(b.events) != 2 || b.events[1].Subject != comp.SubjectMandateFor("a", "shared") || b.events[1].TenantID != "a" || b.events[1].ExpectedLastSubjectSeq == nil || *b.events[1].ExpectedLastSubjectSeq != 1 {
				t.Fatal("repair changed its authorized destination or conditional-write identity")
			}
			cc := b.events[1].Payload.(*lifecyclepb.ConfigChanged)
			if cc.PreviousValue != value || cc.ChangedBy == "" || cc.ApprovedBy == "" || cc.ChangedBy == cc.ApprovedBy {
				t.Fatal("repair lost the exact superseded value or independent approval attribution")
			}
			members, err := comp.DecodeMandateValue(cc.NewValue)
			if err != nil || len(members) != 2 || !proto.Equal(members[0], repair) || !proto.Equal(members[1], scheduled) {
				t.Fatalf("repair did not preserve exactly its approved current and existing own future: %v %v", members, err)
			}
			reg := comp.NewMandateRegistry()
			victim := mandateVersion("b", "shared", 1, now.Add(-time.Hour))
			if err := reg.Put(victim); err != nil {
				t.Fatal(err)
			}
			data, err := proto.Marshal(cc)
			if err != nil {
				t.Fatal(err)
			}
			if err := comp.NewMandateConsumer(reg, nil).Handle(context.Background(), &envelopepb.Envelope{TenantId: "a"}, data); err != nil {
				t.Fatal(err)
			}
			for _, check := range []struct {
				tenant string
				at     time.Time
				want   *compliancepb.Mandate
			}{{"a", now, repair}, {"a", scheduled.EffectiveAt.AsTime(), scheduled}, {"b", scheduled.EffectiveAt.AsTime(), victim}} {
				got, gov, err := reg.Mandate(context.Background(), check.tenant, "shared", check.at)
				if err != nil || gov != comp.Governed || !proto.Equal(got, check.want) {
					t.Fatalf("repaired retained value failed tenant=%s at=%v: got=%v err=%v", check.tenant, check.at, got, err)
				}
			}
		})
	}
}
