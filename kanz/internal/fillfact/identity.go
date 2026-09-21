package fillfact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/eighred/kanz/internal/dec"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

var ErrExecutionIdentityConflict = errors.New("execution identity has conflicting scope or economics")

// ExecutionKey is the economic identity within a tenant. Exchange trade numbers
// are not globally unique: accounts and instruments may legitimately reuse them.
// Unattributed legacy facts retain their old key; a consumer must check for a
// legacy claim before adopting a scoped key, never rename an already posted fill.
func ExecutionKey(f *orderpb.Fill) string {
	if f.GetVenue() == "" || f.GetVenueAccountId() == "" || f.GetInstrumentId() == "" || f.GetVenueExecutionId() == "" {
		return f.GetFillId()
	}
	// A string array has an unambiguous encoding even when labels contain a
	// separator. Marshal cannot fail for this type.
	data, _ := json.Marshal([4]string{f.GetVenue(), f.GetVenueAccountId(), f.GetInstrumentId(), f.GetVenueExecutionId()})
	sum := sha256.Sum256(data)
	return "execution:v1:" + hex.EncodeToString(sum[:])
}

// SameExecution compares economic facts, not transport provenance or a legacy
// producer's local fill alias. Decimal encodings of the same exact value agree.
func SameExecution(a, b *orderpb.Fill) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if _, ok := dec.InDomainDeep(a); !ok {
		return false
	}
	if _, ok := dec.InDomainDeep(b); !ok {
		return false
	}
	if a.GetOrderId() != b.GetOrderId() || a.GetVenue() != b.GetVenue() || a.GetVenueAccountId() != b.GetVenueAccountId() ||
		a.GetInstrumentId() != b.GetInstrumentId() || a.GetVenueExecutionId() != b.GetVenueExecutionId() || a.GetSide() != b.GetSide() ||
		a.GetFee().GetCurrencyCode() != b.GetFee().GetCurrencyCode() {
		return false
	}
	if (a.Quantity == nil) != (b.Quantity == nil) || (a.Price == nil) != (b.Price == nil) ||
		(a.GetFee().GetAmount() == nil) != (b.GetFee().GetAmount() == nil) ||
		(a.ExecutedAt == nil) != (b.ExecutedAt == nil) {
		return false
	}
	if a.ExecutedAt != nil && (a.ExecutedAt.CheckValid() != nil || b.ExecutedAt.CheckValid() != nil || !a.ExecutedAt.AsTime().Equal(b.ExecutedAt.AsTime())) {
		return false
	}
	return dec.Cmp(a.Quantity, b.Quantity) == 0 && dec.Cmp(a.Price, b.Price) == 0 && dec.Cmp(a.GetFee().GetAmount(), b.GetFee().GetAmount()) == 0
}
