package binance

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"strings"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
)

// DemoSpotFunds reads only the explicitly reported free balances at Spot Testnet.
func (v *BinanceVenue) DemoSpotFunds(ctx context.Context, in execution.InstrumentSymbol, uid string) (string, string, error) {
	if strings.TrimRight(v.rest.baseURL, "/") != "https://testnet.binance.vision" || uid == "" ||
		in.VenueSymbol != in.Pair.Base+in.Pair.Quote {
		return "", "", errors.New("unavailable Binance Spot Testnet pair")
	}
	info, err := v.rest.account(ctx)
	if err != nil {
		return "", "", err
	}
	if info.AccountType != "SPOT" || !info.CanTrade || string(info.UID) != uid {
		return "", "", errors.New("spot account identity or trading status unavailable")
	}
	funds := make(map[string]string, 2)
	for _, row := range info.Balances {
		if row.Asset == in.Pair.Base || row.Asset == in.Pair.Quote {
			if _, found := funds[row.Asset]; found {
				return "", "", errors.New("ambiguous spot account balance")
			}
			funds[row.Asset] = row.Free
		}
	}
	return funds[in.Pair.Base], funds[in.Pair.Quote], nil
}

// The same REST client has just verified the account UID in DemoSpotFunds.
// A commission paid in a discount asset needs a separate balance and FX mark.
func (v *BinanceVenue) DemoSpotTakerSchedule(ctx context.Context, in execution.InstrumentSymbol, uid string) *venuepb.DemoSpotTakerSchedule {
	if uid == "" || strings.TrimRight(v.rest.baseURL, "/") != "https://testnet.binance.vision" || in.VenueSymbol != in.Pair.Base+in.Pair.Quote {
		return nil
	}
	if !v.rest.bucket.Allow(20) {
		v.rest.onThrottle()
		return nil
	}
	raw, err := v.rest.signedGet(ctx, "/api/v3/account/commission", url.Values{"symbol": {in.VenueSymbol}})
	type components struct{ Taker, Buyer, Seller string }
	var data struct {
		Code                                                 int
		Symbol                                               string
		StandardCommission, SpecialCommission, TaxCommission components
		Discount                                             struct{ EnabledForAccount, EnabledForSymbol *bool }
	}
	if err != nil || json.Unmarshal(raw, &data) != nil || data.Code != 0 || data.Symbol != in.VenueSymbol ||
		data.Discount.EnabledForAccount == nil || data.Discount.EnabledForSymbol == nil ||
		(*data.Discount.EnabledForAccount && *data.Discount.EnabledForSymbol) {
		return nil
	}
	buy, sell := new(big.Rat), new(big.Rat)
	for _, part := range []components{data.StandardCommission, data.SpecialCommission, data.TaxCommission} {
		taker, t := dec.ParseProtoExact(part.Taker)
		buyer, b := dec.ParseProtoExact(part.Buyer)
		seller, s := dec.ParseProtoExact(part.Seller)
		if !t || !b || !s {
			return nil
		}
		buy.Add(buy, new(big.Rat).Add(dec.FromProto(taker), dec.FromProto(buyer)))
		sell.Add(sell, new(big.Rat).Add(dec.FromProto(taker), dec.FromProto(seller)))
	}
	buyBps, b := dec.ToProtoExact(buy.Mul(buy, big.NewRat(10000, 1)))
	sellBps, s := dec.ToProtoExact(sell.Mul(sell, big.NewRat(10000, 1)))
	if !b || !s {
		return nil
	}
	return &venuepb.DemoSpotTakerSchedule{
		Buy:  &commonpb.SpotTakerFee{Bps: buyBps, Currency: in.Pair.Base},
		Sell: &commonpb.SpotTakerFee{Bps: sellBps, Currency: in.Pair.Quote},
	}
}
