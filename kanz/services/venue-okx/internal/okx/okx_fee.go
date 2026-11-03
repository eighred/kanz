package okx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"

	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
)

// currentTradeFee reads the account's present spot fee and fee-currency mode.
// Neither is a bound on future fills; admission needs separately approved policy.
func (c *okxREST) currentTradeFee(ctx context.Context, instID, groupID string) (*venuepb.CurrentVenueFee, error) {
	if groupID == "" {
		return nil, fmt.Errorf("okx: instrument fee group unknown for %s", instID)
	}
	if !c.buckets.allow(familyUnverified, 1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	path := "/api/v5/account/trade-fee?instType=SPOT&instId=" + url.QueryEscape(instID)
	raw, err := c.signedRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			FeeGroup []struct {
				GroupID  string `json:"groupId"`
				Maker    string `json:"maker"`
				Taker    string `json:"taker"`
				RPIMaker string `json:"rpiMaker"`
				ELPMaker string `json:"elpMaker"`
			} `json:"feeGroup"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("okx: decode trade-fee: %w", err)
	}
	if out.Code != "0" || len(out.Data) != 1 {
		return nil, fmt.Errorf("okx: trade-fee unavailable for %s: code=%s %s", instID, out.Code, out.Msg)
	}
	var selected *venuepb.FeeRateComponent
	var alternate []*venuepb.FeeRateComponent
	for _, group := range out.Data[0].FeeGroup {
		if group.GroupID != groupID {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("okx: duplicate fee group %s", groupID)
		}
		selected = &venuepb.FeeRateComponent{Name: "spot-group:" + groupID, MakerRate: group.Maker, TakerRate: group.Taker}
		// RPI/ELP are distinct execution regimes, not alternate defaults.
		for _, item := range []struct{ name, rate string }{{"rpi-maker", group.RPIMaker}, {"elp-maker", group.ELPMaker}} {
			if item.rate == "" {
				continue
			}
			if _, ok := new(big.Rat).SetString(item.rate); !ok {
				return nil, fmt.Errorf("okx: invalid alternate maker rate")
			}
			alternate = append(alternate, &venuepb.FeeRateComponent{Name: item.name, MakerRate: item.rate})
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("okx: no current rate for instrument fee group %s", groupID)
	}
	for _, rate := range []string{selected.MakerRate, selected.TakerRate} {
		if _, ok := new(big.Rat).SetString(rate); !ok {
			return nil, fmt.Errorf("okx: missing or invalid current trade-fee rate for %s", instID)
		}
	}
	// The fee currency is an account choice, not a property of the instrument.
	// Reading only /trade-fee would let OMS mistake a base-asset fee for quote.
	if !c.buckets.allow(familyUnverified, 1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	configRaw, err := c.signedRequest(ctx, http.MethodGet, "/api/v5/account/config", nil)
	if err != nil {
		return nil, err
	}
	var config struct {
		Code string `json:"code"`
		Data []struct {
			FeeType string `json:"feeType"`
		} `json:"data"`
	}
	if err := json.Unmarshal(configRaw, &config); err != nil {
		return nil, fmt.Errorf("okx: decode fee currency mode: %w", err)
	}
	if config.Code != "0" || len(config.Data) != 1 || (config.Data[0].FeeType != "0" && config.Data[0].FeeType != "1") {
		return nil, fmt.Errorf("okx: account did not provide a recognized fee currency mode")
	}
	fee := &venuepb.CurrentVenueFee{
		Components:      append([]*venuepb.FeeRateComponent{selected}, alternate...),
		FeeCurrencyMode: config.Data[0].FeeType,
	}
	// Hash both observations: the rate alone does not identify the fee currency.
	combined := append(append([]byte(nil), raw...), configRaw...)
	digest := sha256.Sum256(combined)
	fee.SourceVersion = &venuepb.SourceVersion{Source: "okx:/api/v5/account/trade-fee+/api/v5/account/config", ContentSha256: hex.EncodeToString(digest[:]), ObservedAtUnixMillis: c.now().UnixMilli()}
	return fee, nil
}
