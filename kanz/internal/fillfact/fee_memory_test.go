package fillfact

import (
	"fmt"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

func TestMemoryFeeCapacityNeverEvictsReplayEvidence(t *testing.T) {
	original, template := feeFixture(t)
	var revisions []*orderpb.Fill
	head := original
	for i := 0; i <= MaxMemoryFeeRevisions; i++ {
		v := proto.Clone(template).(*orderpb.Fill)
		v.Fee.Amount = &commonpb.Decimal{Coefficient: int64(i) + 10}
		v.Recovery.CaseId = fmt.Sprint(i)
		v.Recovery.FeeApproval.ProposalId = v.Recovery.CaseId
		v.Recovery.FeeApproval.PreviousFee = proto.Clone(head.Fee).(*commonpb.Money)
		digest, err := EconomicDigest(head)
		if err != nil {
			t.Fatal(err)
		}
		v.Recovery.FeeApproval.PreviousExecutionDigest = digest
		_, fresh, err := CheckMemoryFeeRevision(original, revisions, v)
		if i == MaxMemoryFeeRevisions {
			if err == nil || fresh {
				t.Fatal("capacity exceeded")
			}
			break
		}
		if err != nil || !fresh {
			t.Fatalf("revision=%d err=%v", i, err)
		}
		revisions = append(revisions, v)
		head = v
	}
	delta, fresh, err := CheckMemoryFeeRevision(original, revisions, revisions[0])
	if err != nil || fresh || delta.Sign() != 0 {
		t.Fatalf("old replay lost at capacity: %v %v", fresh, err)
	}
}
