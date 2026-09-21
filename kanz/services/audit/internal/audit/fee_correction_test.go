package audit

import (
	"strings"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestFeeApprovalAuditNeverEquatesSignatureWithBookCompletion(t *testing.T) {
	a := &orderpb.ExecutionFeeCorrectionApproved{CaseId: "case", OrderId: "order", PortfolioId: "fund", Digest: strings.Repeat("a", 64), Proposer: "user:maker", Approver: "user:checker", ApprovedAt: timestamppb.Now()}
	env := &envelopepb.Envelope{EventType: "order.order.fee_correction_approved", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
	for _, valid := range []bool{true, false} {
		if !valid {
			a.Approver = " USER:MAKER "
		}
		data, err := proto.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		c := classify(env, data)
		if valid {
			if c.attrs["disposition"] != "approved_pending_book_proof" || c.attrs["evidence_status"] != "valid" || c.attrs["portfolio_id"] != "fund" {
				t.Fatalf("approval evidence: %+v", c)
			}
		} else if c.attrs["evidence_status"] != "invalid" {
			t.Fatal("self approval presented as authorized")
		}
	}
}
