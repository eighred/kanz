//go:build okx

package execution

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// okxREST is a minimal, dependency-free OKX v5 REST client. OKX signs with
// base64(HMAC-SHA256(timestamp + method + requestPath + body, secret)) plus an
// API passphrase header — distinct from Binance's query-string HMAC — so this
// is a separate signer, but it reuses the shared weight bucket + DNS-bypass
// transport. Only the endpoints the connector needs are implemented; no vendor
// SDK is imported.
type okxREST struct {
	baseURL    string
	apiKey     string
	apiSecret  []byte
	passphrase string
	httpc      *http.Client
	bucket     *weightBucket
	now        func() time.Time
	onThrottle func()
}

type okxRestConfig struct {
	BaseURL    string
	APIKey     string
	APISecret  string
	Passphrase string
	HTTPClient *http.Client
	Bucket     *weightBucket
	Now        func() time.Time
	OnThrottle func()
}

func newOKXREST(cfg okxRestConfig) *okxREST {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.OnThrottle == nil {
		cfg.OnThrottle = func() {}
	}
	return &okxREST{
		baseURL: cfg.BaseURL, apiKey: cfg.APIKey, apiSecret: []byte(cfg.APISecret),
		passphrase: cfg.Passphrase, httpc: cfg.HTTPClient, bucket: cfg.Bucket,
		now: cfg.Now, onThrottle: cfg.OnThrottle,
	}
}

// okxPlaceResp is POST /api/v5/trade/order. code "0" at the envelope means the
// request was accepted; data[0].sCode "0" means the order itself was accepted.
type okxPlaceResp struct {
	Code string         `json:"code"`
	Msg  string         `json:"msg"`
	Data []okxPlaceData `json:"data"`
}

type okxPlaceData struct {
	OrdID   string `json:"ordId"`
	ClOrdID string `json:"clOrdId"`
	SCode   string `json:"sCode"`
	SMsg    string `json:"sMsg"`
}

// okxOrder is one element of GET /api/v5/trade/order — the order's true state.
type okxOrder struct {
	OrdID     string `json:"ordId"`
	ClOrdID   string `json:"clOrdId"`
	State     string `json:"state"` // live | partially_filled | filled | canceled
	Sz        string `json:"sz"`
	AccFillSz string `json:"accFillSz"` // cumulative filled base quantity
	AvgPx     string `json:"avgPx"`
	Fee       string `json:"fee"`
	FeeCcy    string `json:"feeCcy"`
	TradeID   string `json:"tradeId"`
}

type okxQueryResp struct {
	Code string     `json:"code"`
	Msg  string     `json:"msg"`
	Data []okxOrder `json:"data"`
}

// placeOrder places a signed order (POST /api/v5/trade/order, weight 1).
func (c *okxREST) placeOrder(ctx context.Context, body map[string]string) (*okxPlaceData, error) {
	if !c.bucket.allow(1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	raw, err := c.signedRequest(ctx, http.MethodPost, "/api/v5/trade/order", body)
	if err != nil {
		return nil, err
	}
	var out okxPlaceResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("okx: decode place response: %w", err)
	}
	if len(out.Data) == 0 {
		return nil, &APIError{Code: atoiSafe(out.Code), Msg: out.Msg}
	}
	d := out.Data[0]
	if out.Code != "0" || d.SCode != "0" {
		return &d, &APIError{Code: atoiSafe(d.SCode), Msg: d.SMsg}
	}
	return &d, nil
}

// queryOrder fetches an order's true state by client order id (GET
// /api/v5/trade/order, weight 1) — the idempotency-recovery path.
func (c *okxREST) queryOrder(ctx context.Context, instID, clOrdID string) (*okxOrder, error) {
	if !c.bucket.allow(1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	path := "/api/v5/trade/order?instId=" + instID + "&clOrdId=" + clOrdID
	raw, err := c.signedRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out okxQueryResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("okx: decode query response: %w", err)
	}
	if out.Code != "0" || len(out.Data) == 0 {
		return nil, &APIError{Code: atoiSafe(out.Code), Msg: out.Msg}
	}
	return &out.Data[0], nil
}

// sweepMarket places an aggressive market order to flatten a residual exposure
// when an in-flight close is stuck (the healing seam). side is "buy"/"sell", sz
// is the base quantity. clOrdId is deterministic ("heal-"+orderID) so a retried
// sweep is idempotent exchange-side and never double-flattens.
func (c *okxREST) sweepMarket(ctx context.Context, instID, side, sz, clOrdID string) (*okxPlaceData, error) {
	return c.placeOrder(ctx, map[string]string{
		"instId":  instID,
		"tdMode":  "cash",
		"side":    side,
		"ordType": "market",
		"tgtCcy":  "base_ccy",
		"clOrdId": clOrdID,
		"sz":      sz,
	})
}

// cancelOrder withdraws a working order by clOrdId (POST
// /api/v5/trade/cancel-order, weight 1). It addresses the order by the SAME
// deterministic clOrdId the submit stamped, so a retried cancel resolves to the
// original order rather than racing a second one.
func (c *okxREST) cancelOrder(ctx context.Context, instID, clOrdID string) (*okxPlaceData, error) {
	if !c.bucket.allow(1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	raw, err := c.signedRequest(ctx, http.MethodPost, "/api/v5/trade/cancel-order", map[string]string{
		"instId": instID, "clOrdId": clOrdID,
	})
	if err != nil {
		return nil, err
	}
	var out okxPlaceResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("okx: decode cancel response: %w", err)
	}
	if len(out.Data) == 0 {
		return nil, &APIError{Code: atoiSafe(out.Code), Msg: out.Msg}
	}
	d := out.Data[0]
	if out.Code != "0" || d.SCode != "0" {
		return &d, &APIError{Code: atoiSafe(d.SCode), Msg: d.SMsg}
	}
	return &d, nil
}

// okxBalances is GET /api/v5/account/balance — cash balances for reconciliation.
type okxBalances struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		Details []struct {
			Ccy     string `json:"ccy"`
			CashBal string `json:"cashBal"`
		} `json:"details"`
	} `json:"data"`
}

// balances fetches the account cash balances per currency (signed, weight 1).
func (c *okxREST) balances(ctx context.Context) (map[string]string, error) {
	if !c.bucket.allow(1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	raw, err := c.signedRequest(ctx, http.MethodGet, "/api/v5/account/balance", nil)
	if err != nil {
		return nil, err
	}
	var out okxBalances
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("okx: decode balance: %w", err)
	}
	if out.Code != "0" {
		return nil, &APIError{Code: atoiSafe(out.Code), Msg: out.Msg}
	}
	m := map[string]string{}
	for _, d := range out.Data {
		for _, b := range d.Details {
			m[b.Ccy] = b.CashBal
		}
	}
	return m, nil
}

// tickerPrice returns the last price for an instrument (public GET
// /api/v5/market/ticker, unsigned) — feeds the MarkSource seam.
func (c *okxREST) tickerPrice(ctx context.Context, instID string) (string, error) {
	if !c.bucket.allow(1) {
		c.onThrottle()
		return "", ErrRateLimited
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v5/market/ticker?instId="+instID, nil)
	if err != nil {
		return "", err
	}
	raw, err := c.do(req)
	if err != nil {
		return "", err
	}
	var out struct {
		Code string `json:"code"`
		Data []struct {
			Last string `json:"last"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if out.Code != "0" || len(out.Data) == 0 {
		return "", &APIError{Code: atoiSafe(out.Code), Msg: "ticker unavailable"}
	}
	return out.Data[0].Last, nil
}

// do executes a request and returns the body for any <500 status.
func (c *okxREST) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %s status %d", ErrEgressDenied, req.URL.Path, resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("okx: %s: status %d", req.URL.Path, resp.StatusCode)
	}
	return raw, nil
}

// signedRequest signs and sends. requestPath includes any query string; body is
// the JSON payload (empty for GET). OK-ACCESS-SIGN =
// base64(HMAC-SHA256(timestamp + method + requestPath + body)).
func (c *okxREST) signedRequest(ctx context.Context, method, requestPath string, body map[string]string) ([]byte, error) {
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = json.Marshal(body)
	}
	ts := c.now().UTC().Format("2006-01-02T15:04:05.000Z")
	prehash := ts + method + requestPath + string(bodyBytes)
	mac := hmac.New(sha256.New, c.apiSecret)
	mac.Write([]byte(prehash))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+requestPath, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("OK-ACCESS-KEY", c.apiKey)
	req.Header.Set("OK-ACCESS-SIGN", sign)
	req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
	req.Header.Set("OK-ACCESS-PASSPHRASE", c.passphrase)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %s status %d", ErrEgressDenied, requestPath, resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("okx: %s: status %d", requestPath, resp.StatusCode)
	}
	return raw, nil
}

// atoiSafe parses an OKX string code to int (0 on empty/parse failure).
func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
