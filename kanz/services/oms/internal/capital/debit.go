package capital

import (
	"math/big"
	"sort"
	"strings"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// PhysicalDebits bounds an unlevered, physically settled asset pair. The caller
// must establish that settlement model from authoritative instrument/venue
// terms; a pair name alone is not evidence that a derivative settles physically.
// fees is the complete worst-case fee vector for this quantity and execution
// policy, including per-fill minima. nil means UNKNOWN; an explicitly empty
// vector asserts no fees. Future sale proceeds and rebates never fund admission.
// The output owns its messages and is sorted for ReserveMany's lock order.
func PhysicalDebits(st *orderpb.OrderState, base, quote string, fees []*commonpb.Money) ([]*commonpb.Money, error) {
	if st == nil || !debitCurrency(base) || !debitCurrency(quote) || base == quote || fees == nil || len(fees) > 31 {
		return nil, ErrInvalid
	}
	if st.GetMarginMode() != orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED {
		return nil, ErrInvalid
	}
	if leverage := st.GetLeverage(); leverage != nil {
		v, ok := dec.FromProtoChecked(leverage)
		if !ok || v.Cmp(big.NewRat(1, 1)) != 0 {
			return nil, ErrInvalid
		}
	}
	quantity, ok := positiveDecimal(st.GetOrderedQuantity())
	if !ok {
		return nil, ErrInvalid
	}
	// A mark or a stop trigger is not an enforceable execution-price ceiling.
	// Refuse unpriced orders even on the sell side: their fee bound may depend
	// on execution notional, which this limited settlement model cannot bound.
	if st.GetOrderType() != orderpb.OrderType_ORDER_TYPE_LIMIT && st.GetOrderType() != orderpb.OrderType_ORDER_TYPE_STOP_LIMIT {
		return nil, ErrInvalid
	}
	price, ok := positiveDecimal(st.GetLimitPrice())
	if !ok {
		return nil, ErrInvalid
	}
	amounts := make(map[string]*big.Rat)
	switch st.GetSide() {
	case orderpb.Side_SIDE_BUY:
		amounts[quote] = new(big.Rat).Mul(quantity, price)
	case orderpb.Side_SIDE_SELL:
		amounts[base] = quantity
	default:
		return nil, ErrInvalid
	}
	seen := make(map[string]bool)
	for _, fee := range fees {
		if fee == nil || fee.Amount == nil || !debitCurrency(fee.CurrencyCode) || seen[fee.CurrencyCode] {
			return nil, ErrInvalid
		}
		value, valid := dec.FromProtoChecked(fee.Amount)
		if !valid || value.Sign() < 0 {
			return nil, ErrInvalid
		}
		seen[fee.CurrencyCode] = true
		if previous := amounts[fee.CurrencyCode]; previous != nil {
			amounts[fee.CurrencyCode] = new(big.Rat).Add(previous, value)
		} else {
			amounts[fee.CurrencyCode] = value
		}
	}
	currencies := make([]string, 0, len(amounts))
	for currency := range amounts {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	result := make([]*commonpb.Money, 0, len(currencies))
	for _, currency := range currencies {
		amount, exact := dec.ToProtoExact(amounts[currency])
		if !exact {
			return nil, ErrInvalid // rounding down would underfund the order
		}
		result = append(result, &commonpb.Money{CurrencyCode: currency, Amount: amount})
	}
	return result, nil
}

func debitCurrency(currency string) bool {
	return validID(currency) && strings.TrimSpace(currency) == currency
}

func positiveDecimal(value *commonpb.Decimal) (*big.Rat, bool) {
	if value == nil {
		return nil, false
	}
	v, ok := dec.FromProtoChecked(value)
	return v, ok && v.Sign() > 0
}
