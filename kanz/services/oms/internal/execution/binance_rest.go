//go:build binance

package execution

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrRateLimited is returned when the local weight budget is exhausted before a
// REST call — the caller backs off and raises a structural alert rather than
// firing the request and risking an exchange ban. It is never a fabricated fill.
var ErrRateLimited = errors.New("binance: local rate-limit budget exhausted")

// APIError is a typed Binance error body ({code, msg}).
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string { return fmt.Sprintf("binance error %d: %s", e.Code, e.Msg) }

// binanceREST is a minimal, dependency-free Binance Spot REST client. It signs
// requests with HMAC-SHA256 over the query string (RFC-standard Binance auth),
// meters every call against a weight bucket, and decodes typed responses. Only
// the endpoints the connector needs are implemented; no vendor SDK is imported,
// so this compiles cleanly behind the build tag with the stdlib alone.
type binanceREST struct {
	baseURL    string
	apiKey     string
	apiSecret  []byte
	recvWindow int
	httpc      *http.Client
	bucket     *weightBucket
	now        func() time.Time
	onThrottle func() // structural alert hook when the budget is exhausted
}

type restConfig struct {
	BaseURL    string
	APIKey     string
	APISecret  string
	RecvWindow int
	HTTPClient *http.Client
	Bucket     *weightBucket
	Now        func() time.Time
	OnThrottle func()
}

func newBinanceREST(cfg restConfig) *binanceREST {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RecvWindow <= 0 {
		cfg.RecvWindow = 5000
	}
	if cfg.OnThrottle == nil {
		cfg.OnThrottle = func() {}
	}
	return &binanceREST{
		baseURL: cfg.BaseURL, apiKey: cfg.APIKey, apiSecret: []byte(cfg.APISecret),
		recvWindow: cfg.RecvWindow, httpc: cfg.HTTPClient, bucket: cfg.Bucket,
		now: cfg.Now, onThrottle: cfg.OnThrottle,
	}
}

// orderResponse is the FULL new-order / query-order response (the fields the
// connector reads).
type orderResponse struct {
	Symbol              string      `json:"symbol"`
	OrderID             int64       `json:"orderId"`
	ClientOrderID       string      `json:"clientOrderId"`
	OrigClientOrderID   string      `json:"origClientOrderId"`
	Status              string      `json:"status"`
	ExecutedQty         string      `json:"executedQty"`
	CummulativeQuoteQty string      `json:"cummulativeQuoteQty"`
	Fills               []orderFill `json:"fills"`
	TransactTime        int64       `json:"transactTime"`
	Code                int         `json:"code"`
	Msg                 string      `json:"msg"`
}

type orderFill struct {
	Price           string `json:"price"`
	Qty             string `json:"qty"`
	Commission      string `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`
	TradeID         int64  `json:"tradeId"`
}

// newOrder places a signed order (POST /api/v3/order, weight 1) with
// newOrderRespType=FULL so the immediate fills come back inline.
func (c *binanceREST) newOrder(ctx context.Context, params url.Values) (*orderResponse, error) {
	params.Set("newOrderRespType", "FULL")
	return c.signedOrderCall(ctx, http.MethodPost, "/api/v3/order", params, 1)
}

// queryOrder fetches an order's current state by its client order id (GET
// /api/v3/order, weight 2) — the idempotency-recovery path after an ambiguous
// timeout: the same newClientOrderId resolves to the original order.
func (c *binanceREST) queryOrder(ctx context.Context, symbol, origClientOrderID string) (*orderResponse, error) {
	params := url.Values{"symbol": {symbol}, "origClientOrderId": {origClientOrderID}}
	return c.signedOrderCall(ctx, http.MethodGet, "/api/v3/order", params, 2)
}

// tickerPrice returns the last price for a symbol (GET /api/v3/ticker/price,
// weight 1) — feeds the MarkSource seam for live unrealized P&L.
func (c *binanceREST) tickerPrice(ctx context.Context, symbol string) (string, error) {
	if !c.bucket.allow(1) {
		c.onThrottle()
		return "", ErrRateLimited
	}
	u := c.baseURL + "/api/v3/ticker/price?symbol=" + url.QueryEscape(symbol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	body, err := c.do(req)
	if err != nil {
		return "", err
	}
	var out struct {
		Price string `json:"price"`
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", &APIError{Code: out.Code, Msg: out.Msg}
	}
	return out.Price, nil
}

func (c *binanceREST) signedOrderCall(ctx context.Context, method, path string, params url.Values, weight int) (*orderResponse, error) {
	if !c.bucket.allow(weight) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	params.Set("timestamp", strconv.FormatInt(c.now().UnixMilli(), 10))
	params.Set("recvWindow", strconv.Itoa(c.recvWindow))
	query := params.Encode()
	sig := c.sign(query)
	full := c.baseURL + path + "?" + query + "&signature=" + sig

	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKey)

	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var out orderResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("binance: decode order response: %w", err)
	}
	if out.Code != 0 {
		return &out, &APIError{Code: out.Code, Msg: out.Msg}
	}
	return &out, nil
}

// do executes the request and returns the body for any status; the caller
// decodes the typed {code,msg} error. A 5xx is a real transport fault
// (surfaced), distinct from a 4xx business error (decoded).
func (c *binanceREST) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("binance: %s: status %d", req.URL.Path, resp.StatusCode)
	}
	return body, nil
}

func (c *binanceREST) sign(query string) string {
	m := hmac.New(sha256.New, c.apiSecret)
	m.Write([]byte(query))
	return hex.EncodeToString(m.Sum(nil))
}
