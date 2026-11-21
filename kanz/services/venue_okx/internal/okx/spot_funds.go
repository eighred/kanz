package okx

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"strings"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
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

// OKX publishes the account schedule by instrument group. Its trade-fee API
// excludes zero-fee promotions, so the browser exposes this as a schedule.
func (v *OKXVenue) DemoSpotTakerSchedule(ctx context.Context, in execution.InstrumentSymbol, uid string) *venuepb.DemoSpotTakerSchedule {
	if v.rest.mode != exchangeauth.OKXDemo || strings.TrimRight(v.rest.baseURL, "/") != "https://www.okx.com" ||
		uid == "" || in.VenueSymbol != in.Pair.Base+"-"+in.Pair.Quote {
		return nil
	}
	info, err := v.rest.exchangeAccount(ctx)
	if err != nil || info.ID != uid || info.AccountLevel != "1" || (info.SpotFeeType != "0" && info.SpotFeeType != "1") {
		return nil
	}
	query := url.Values{"instType": {"SPOT"}, "instId": {in.VenueSymbol}}.Encode()
	get := func(path string, family okxRateFamily, result any) bool {
		if !v.rest.buckets.allow(family, 1) {
			v.rest.onThrottle()
			return false
		}
		raw, err := v.rest.signedRequest(ctx, http.MethodGet, path+"?"+query, nil)
		return err == nil && json.Unmarshal(raw, result) == nil
	}
	var instruments struct {
		Code string
		Data []struct{ InstType, InstId, GroupId, BaseCcy, QuoteCcy, State string }
	}
	if !get("/api/v5/account/instruments", familySpotInstruments, &instruments) || instruments.Code != "0" || len(instruments.Data) != 1 {
		return nil
	}
	inst := instruments.Data[0]
	if inst.InstId != in.VenueSymbol || inst.InstType != "SPOT" || inst.GroupId == "" ||
		inst.BaseCcy != in.Pair.Base || inst.QuoteCcy != in.Pair.Quote || inst.State != "live" {
		return nil
	}
	var fees struct {
		Code string
		Data []struct {
			InstType string
			FeeGroup []struct{ GroupId, Taker string }
		}
	}
	if !get("/api/v5/account/trade-fee", familyTradeFees, &fees) || fees.Code != "0" || len(fees.Data) != 1 || fees.Data[0].InstType != "SPOT" {
		return nil
	}
	var bps *commonpb.Decimal
	for _, group := range fees.Data[0].FeeGroup {
		if group.GroupId != inst.GroupId {
			continue
		}
		rate, ok := dec.ParseProtoExact(group.Taker)
		if !ok || bps != nil {
			return nil
		}
		// OKX negative means a charge. Kanz positive means a charge.
		bps, ok = dec.ToProtoExact(new(big.Rat).Mul(dec.FromProto(rate), big.NewRat(-10000, 1)))
		if !ok {
			return nil
		}
	}
	if bps == nil {
		return nil
	}
	buyCurrency := in.Pair.Base
	if info.SpotFeeType == "1" {
		buyCurrency = in.Pair.Quote
	}
	return &venuepb.DemoSpotTakerSchedule{
		Buy:  &commonpb.SpotTakerFee{Bps: bps, Currency: buyCurrency},
		Sell: &commonpb.SpotTakerFee{Bps: bps, Currency: in.Pair.Quote},
	}
}
