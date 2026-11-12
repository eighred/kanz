package position

import (
	"fmt"
	"math/big"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// Finite observations must retain every digit, including a tiny nonzero mark.
// Weighted-cost P&L and cost-marked snapshots can instead be nonterminating
// rationals; only those retain the existing half-away, eight-place conversion.
// An unrepresentable finite amount must never fall back to that rounding path.
func projectionMoney(r *big.Rat, currency string) (*commonpb.Money, error) {
	var amount *commonpb.Decimal
	var ok bool
	if _, finite := r.FloatPrec(); finite {
		amount, ok = dec.ToProtoExact(r)
	} else {
		amount, ok = dec.ToProtoScaled(r)
	}
	if !ok || !dec.InDomain(amount) {
		return nil, fmt.Errorf("amount is not representable as a Decimal")
	}
	return &commonpb.Money{Amount: amount, CurrencyCode: currency}, nil
}

// A rounded cost mark is used for admission, unlike analytic P&L. If its
// nonzero value rounds to zero, refuse the view rather than hiding the holding.
// Snapshot refusal does not undo the separately committed execution and lot.
func projectionMark(r *big.Rat, currency string) (*commonpb.Money, error) {
	m, err := projectionMoney(r, currency)
	if err != nil {
		return nil, err
	}
	if r.Sign() != 0 && m.Amount.Coefficient == 0 {
		return nil, fmt.Errorf("nonzero position value rounds to zero")
	}
	return m, nil
}
