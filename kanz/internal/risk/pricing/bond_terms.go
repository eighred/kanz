package pricing

import (
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	"google.golang.org/protobuf/proto"
)

// MaxBondTermsBytes bounds optional canonical metadata copied for diagnostics.
const MaxBondTermsBytes = 1 << 20

// CloneBoundedBondTerms returns nil for absent or oversized diagnostic metadata.
// It never truncates fields, changes the authoritative numeric BondSpec, or
// implies a successful shadow calculation. Size is checked before any copy.
func CloneBoundedBondTerms(terms *referencepb.BondTerms) *referencepb.BondTerms {
	if terms == nil || proto.Size(terms) > MaxBondTermsBytes {
		return nil
	}
	return proto.Clone(terms).(*referencepb.BondTerms)
}
