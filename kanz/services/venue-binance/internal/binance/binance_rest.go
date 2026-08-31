package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

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

// accountInfo is GET /api/v3/account — balances for reconciliation. The uid that
// says WHOSE account this key is (SOV-02a) is decoded once, in exchangeauth,
// which ExchangeAccountID below delegates to.
type accountInfo struct {
	Balances []balanceEntry `json:"balances"`
	Code     int            `json:"code"`
	Msg      string         `json:"msg"`
}

type balanceEntry struct {
	Asset  string `json:"asset"`
	Free   string `json:"free"`
	Locked string `json:"locked"`
}

// account fetches balances (GET /api/v3/account, weight 10).
func (c *binanceREST) account(ctx context.Context) (*accountInfo, error) {
	if !c.bucket.Allow(10) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	body, err := c.signedGet(ctx, "/api/v3/account", url.Values{})
	if err != nil {
		return nil, err
	}
	var out accountInfo
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("binance: decode account: %w", err)
	}
	if out.Code != 0 {
		return nil, &APIError{Code: out.Code, Msg: out.Msg}
	}
	return &out, nil
}

// ExchangeAccountID asks Binance which account this API key belongs to (SOV-02a).
//
// It implements accountproof.Exchange, and it is called ONCE at startup: the adapter
// refuses to trade if Binance names an account other than the one the deployment says
// it is. The request, uid-decode, and "no uid is an error, never empty string"
// validation live once in exchangeauth — this method only spends the weight budget
// and delegates.
//
// NOTE on error mapping: exchangeauth's GET /api/v3/account maps a 401/403 response
// to execution.ErrEgressDenied, same as OKX. binanceREST.do (used by the TRADING
// path: orders, cancels, queries) does not map 401/403 specially and never has — it
// only special-cases >=500. That asymmetry is intentional and pre-existing; it is
// not widened by this delegation because the trading path's do() is untouched here.
// The only consumer of ExchangeAccountID is accountproof.Resolve, which treats ANY
// error as unverifiable (fatal unless AllowUnverified), so this changes no
// consumer-visible behaviour.
func (c *binanceREST) ExchangeAccountID(ctx context.Context) (string, error) {
	if !c.bucket.Allow(10) {
		c.onThrottle()
		return "", ErrRateLimited
	}
	return exchangeauth.AccountID(ctx, "binance", exchangeauth.Credential{
		APIKey:    c.apiKey,
		APISecret: string(c.apiSecret),
	}, exchangeauth.Options{
		BaseURL:    c.baseURL,
		HTTPClient: c.httpc,
		Now:        c.now,
	})
}

// signedGet performs a signed GET and returns the raw body (weight already
// consumed by the caller).
func (c *binanceREST) signedGet(ctx context.Context, path string, params url.Values) ([]byte, error) {
	params.Set("timestamp", strconv.FormatInt(c.now().UnixMilli(), 10))
	params.Set("recvWindow", strconv.Itoa(c.recvWindow))
	query := params.Encode()
	full := c.baseURL + path + "?" + query + "&signature=" + c.sign(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKey)
	return c.do(req)
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

// tradeEntry is one execution as GET /api/v3/myTrades reports it. `ID` is the
// tradeId — the SAME identity the placement response and the user-data stream
// carry, which is what lets a re-query rebuild a fill with the fill_id the
// platform already holds instead of a second name for one trade.
type tradeEntry struct {
	Symbol          string `json:"symbol"`
	ID              int64  `json:"id"`
	OrderID         int64  `json:"orderId"`
	Price           string `json:"price"`
	Qty             string `json:"qty"`
	Commission      string `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`
	Time            int64  `json:"time"`
}

// myTradesWeight is the request weight of GET /api/v3/myTrades. It is far more
// expensive than the order query beside it (2), which is why the query path asks
// for trades ONLY once Binance has said the order actually traded — a working or
// unknown order costs 2, not 22.
const myTradesWeight = 20

// myTrades lists the executions behind one order (GET /api/v3/myTrades).
//
// ADDRESSED BY THE EXCHANGE'S NUMERIC orderId, which is the one place the query
// path cannot use our own client order id: Binance offers no origClientOrderId
// form of this endpoint. The numeric id comes from the order query that precedes
// it, so the two calls are still anchored on our deterministic id.
//
// An error body is a JSON OBJECT and a success body a JSON ARRAY, so the object
// decode is attempted first: a {code,msg} that unmarshals is the exchange
// refusing, and anything else falls through to the list.
func (c *binanceREST) myTrades(ctx context.Context, symbol string, orderID int64) ([]tradeEntry, error) {
	if !c.bucket.Allow(myTradesWeight) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	body, err := c.signedGet(ctx, "/api/v3/myTrades", url.Values{
		"symbol":  {symbol},
		"orderId": {strconv.FormatInt(orderID, 10)},
	})
	if err != nil {
		return nil, err
	}
	var apiErr struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Code != 0 {
		return nil, &APIError{Code: apiErr.Code, Msg: apiErr.Msg}
	}
	var out []tradeEntry
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("binance: decode trade list: %w", err)
	}
	return out, nil
}

// cancelOrder withdraws a working order by its client order id (DELETE
// /api/v3/order, weight 1). It addresses the order by the SAME deterministic
// origClientOrderId the submit stamped, so a retried cancel resolves to the
// original order instead of racing a second one. A nil error means Binance
// confirmed the withdrawal; a -2011 ("Unknown order sent") means it is already
// gone — the caller treats that as confirmed, not as a failure.
func (c *binanceREST) cancelOrder(ctx context.Context, symbol, origClientOrderID string) (*orderResponse, error) {
	params := url.Values{"symbol": {symbol}, "origClientOrderId": {origClientOrderID}}
	return c.signedOrderCall(ctx, http.MethodDelete, "/api/v3/order", params, 1)
}

// sweepMarket places an aggressive MARKET order to flatten a residual exposure
// left by a close that did not land (the healing sweep). clOrdID is the
// deterministic "heal-"+orderID, so a retried sweep resolves to the original
// sweep at the venue and NEVER double-flattens the position.
func (c *binanceREST) sweepMarket(ctx context.Context, symbol, side, qty, clOrdID string) (*orderResponse, error) {
	return c.newOrder(ctx, url.Values{
		"symbol":           {symbol},
		"side":             {side},
		"type":             {"MARKET"},
		"quantity":         {qty},
		"newClientOrderId": {clOrdID},
	})
}

// tickerPrice returns the last price for a symbol (GET /api/v3/ticker/price,
// weight 1) — feeds the MarkSource seam for live unrealized P&L.
func (c *binanceREST) tickerPrice(ctx context.Context, symbol string) (string, error) {
	if !c.bucket.Allow(1) {
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
	if !c.bucket.Allow(weight) {
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
	return exchangeauth.SignBinance(string(c.apiSecret), query)
}
