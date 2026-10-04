package binance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"

	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
)

type commissionComponent struct {
	Maker  string `json:"maker"`
	Taker  string `json:"taker"`
	Buyer  string `json:"buyer"`
	Seller string `json:"seller"`
}

// currentCommission is an observation of the account's present symbol rate.
// Binance may change these rates or charge in a discount asset on a later fill;
// this response must never authorize an OMS maximum-fee reservation.
func (c *binanceREST) currentCommission(ctx context.Context, symbol string) (*venuepb.CurrentVenueFee, error) {
	if !c.bucket.Allow(20) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	raw, err := c.signedGet(ctx, "/api/v3/account/commission", url.Values{"symbol": {symbol}})
	if err != nil {
		return nil, err
	}
	var out struct {
		Symbol   string              `json:"symbol"`
		Standard commissionComponent `json:"standardCommission"`
		Special  commissionComponent `json:"specialCommission"`
		Tax      commissionComponent `json:"taxCommission"`
		Discount struct {
			Asset string `json:"discountAsset"`
		} `json:"discount"`
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("binance: decode current commission: %w", err)
	}
	if out.Code != 0 {
		return nil, &APIError{Code: out.Code, Msg: out.Msg}
	}
	if out.Symbol != symbol {
		return nil, fmt.Errorf("binance: commission response symbol %q does not match %q", out.Symbol, symbol)
	}
	fee := &venuepb.CurrentVenueFee{FeeCurrencyMode: "UNKNOWN_FROM_COMMISSION_ENDPOINT", DiscountAsset: out.Discount.Asset}
	for _, named := range []struct {
		name string
		c    commissionComponent
	}{
		{"standard", out.Standard}, {"special", out.Special}, {"tax", out.Tax},
	} {
		for _, rate := range []string{named.c.Maker, named.c.Taker, named.c.Buyer, named.c.Seller} {
			r, ok := new(big.Rat).SetString(rate)
			if !ok || r.Sign() < 0 {
				return nil, fmt.Errorf("binance: invalid %s current commission component", named.name)
			}
		}
		fee.Components = append(fee.Components, &venuepb.FeeRateComponent{
			Name: named.name, MakerRate: named.c.Maker, TakerRate: named.c.Taker,
			BuyerRate: named.c.Buyer, SellerRate: named.c.Seller,
		})
	}
	digest := sha256.Sum256(raw)
	fee.SourceVersion = &venuepb.SourceVersion{Source: "binance:/api/v3/account/commission", ContentSha256: hex.EncodeToString(digest[:]), ObservedAtUnixMillis: c.now().UnixMilli()}
	return fee, nil
}
