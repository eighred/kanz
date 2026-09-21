package fillfact

import (
	"math/big"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

// MaxMemoryFeeRevisions bounds immutable correction evidence in ephemeral test
// books. Exhaustion refuses another revision; it never forgets a replay claim.
const MaxMemoryFeeRevisions = 128

func MemoryFeeHead(original *orderpb.Fill, revisions []*orderpb.Fill) *orderpb.Fill {
	if len(revisions) != 0 {
		return revisions[len(revisions)-1]
	}
	return original
}

func CheckMemoryFeeRevision(original *orderpb.Fill, revisions []*orderpb.Fill, revised *orderpb.Fill) (*big.Rat, bool, error) {
	if !SameExecutionExceptFee(original, revised) {
		return nil, false, ErrFeeRevision
	}
	if _, err := FeeRevisionTerms(revised); err != nil {
		return nil, false, err
	}
	for _, prior := range revisions {
		if prior.GetRecovery().GetCaseId() == revised.GetRecovery().GetCaseId() {
			if !proto.Equal(prior, revised) {
				return nil, false, ErrFeeRevision
			}
			return new(big.Rat), false, nil
		}
	}
	if len(revisions) >= MaxMemoryFeeRevisions {
		return nil, false, ErrFeeRevision
	}
	delta, err := ApprovedFeeDelta(MemoryFeeHead(original, revisions), revised)
	return delta, err == nil, err
}
