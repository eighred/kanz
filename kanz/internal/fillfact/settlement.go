package fillfact

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/instrument"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// Legs are the actual signed inventory and cash movements of one execution.
// Price is the gross execution price used by the books' cost-basis policy.
type Legs struct {
	Quantity, Price, Cash *big.Rat
	Currency              string
}

func SettlementLegs(f *orderpb.Fill, legacyCurrency string) (Legs, error) {
	qty, qtyOK := dec.FromProtoChecked(f.GetQuantity())
	price, priceOK := dec.FromProtoChecked(f.GetPrice())
	if !qtyOK || !priceOK || !dec.InDomain(f.GetFee().GetAmount()) {
		return Legs{}, errors.New("execution decimal outside supported domain")
	}
	currency, base := legacyCurrency, ""
	if symbol := f.GetSpotVenueSymbol(); symbol != "" {
		pair, ok := instrument.SpotPair(f.GetInstrumentId(), symbol)
		if !ok || qty.Sign() <= 0 || price.Sign() <= 0 || (f.Side != orderpb.Side_SIDE_BUY && f.Side != orderpb.Side_SIDE_SELL) {
			return Legs{}, fmt.Errorf("invalid spot execution %q on %q", f.GetInstrumentId(), symbol)
		}
		currency, base = pair.Quote, pair.Base
	}
	if f.GetSide() == orderpb.Side_SIDE_SELL {
		qty.Neg(qty)
	}
	cash := new(big.Rat).Mul(qty, price)
	cash.Neg(cash)
	fee := f.GetFee()
	if fee != nil && fee.Amount == nil {
		return Legs{}, errors.New("execution fee has no amount")
	}
	if base != "" && fee.GetCurrencyCode() == base {
		qty.Sub(qty, dec.FromProto(fee.Amount))
	} else {
		charge, err := dec.MoneyIn(fee, currency)
		if err != nil {
			return Legs{}, fmt.Errorf("fee in %q cannot be netted against the cash leg in %q: %w", fee.GetCurrencyCode(), currency, err)
		}
		cash.Sub(cash, charge)
	}
	return Legs{Quantity: qty, Price: price, Cash: cash, Currency: currency}, nil
}
