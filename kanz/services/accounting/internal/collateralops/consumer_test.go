package collateralops

import (
	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"testing"
)

func TestCustodianCannotSelectAuthorityThroughEnvelope(t *testing.T) {
	c, err := NewConsumer(New(nil), "tenant-A", "input", `[{"Source":"A","Custodian":"custody-A","Account":"A"},{"Source":"B","Custodian":"custody-B","Account":"B"}]`)
	if err != nil {
		t.Fatal(err)
	}
	handler := c.ConfirmationHandlers()[SubjectConfirmation+".A"]
	env := &envelopepb.Envelope{TenantId: "tenant-A", Source: "B", EventType: SubjectConfirmation, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, PayloadSchemaRef: "collateral.v1.PostingConfirmation:1"}
	blob, err := marshal(&pb.PostingConfirmation{CustodianId: "custody-B", AccountId: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if handler(t.Context(), env, blob) == nil {
		t.Fatal("source A selected B through its envelope")
	}
	env.Source = "A"
	if handler(t.Context(), env, blob) == nil {
		t.Fatal("source A selected B account")
	}
	env.TenantId = "tenant-B"
	if handler(t.Context(), env, blob) == nil {
		t.Fatal("cross-tenant confirmation")
	}
}
