package okx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

// DemoSpotFunds asks for cash-mode buying and selling capacity for one pair.
func (v *OKXVenue) DemoSpotFunds(ctx context.Context, in execution.InstrumentSymbol, uid string) (string, string, error) {
	if v.rest.mode != exchangeauth.OKXDemo || strings.TrimRight(v.rest.baseURL, "/") != "https://www.okx.com" ||
		uid == "" || in.VenueSymbol != in.Pair.Base+"-"+in.Pair.Quote {
		return "", "", errors.New("unavailable OKX demo spot pair")
	}
	info, err := v.rest.exchangeAccount(ctx)
	if err != nil {
		return "", "", err
	}
	if info.ID != uid || info.AccountLevel != "1" {
		return "", "", errors.New("verified spot-mode account required")
	}
	if !v.rest.buckets.allow(familyUnverified, 1) {
		v.rest.onThrottle()
		return "", "", ErrRateLimited
	}
	query := url.Values{"instId": {in.VenueSymbol}, "tdMode": {"cash"}}
	raw, err := v.rest.signedRequest(ctx, http.MethodGet, "/api/v5/account/max-avail-size?"+query.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	var reply struct {
		Code string `json:"code"`
		Data []struct {
			InstID string `json:"instId"`
			Buy    string `json:"availBuy"`
			Sell   string `json:"availSell"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Code != "0" || len(reply.Data) != 1 || reply.Data[0].InstID != in.VenueSymbol {
		return "", "", errors.New("unconfirmed spot availability")
	}
	return reply.Data[0].Sell, reply.Data[0].Buy, nil
}
