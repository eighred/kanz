package okx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
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
	buckets    *okxBuckets
	now        func() time.Time
	onThrottle func()
	// mode decides which OKX book every signed request from this client reaches
	// (#147). Held on the client rather than passed per call: a per-call argument
	// would be one more thing to get right at each of the order, cancel, amend and
	// reconcile sites, and getting it wrong at exactly one of them is the bug.
	mode exchangeauth.OKXTradingMode
}

type okxRestConfig struct {
	BaseURL    string
	APIKey     string
	APISecret  string
	Passphrase string
	HTTPClient *http.Client
	Buckets    *okxBuckets
	Now        func() time.Time
	OnThrottle func()
	Mode       exchangeauth.OKXTradingMode
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
		passphrase: cfg.Passphrase, httpc: cfg.HTTPClient, buckets: cfg.Buckets,
		now: cfg.Now, onThrottle: cfg.OnThrottle, mode: cfg.Mode,
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
	// AlgoClOrdID is OUR id when this order was CREATED BY A TRIGGER firing
	// (#485). OKX gives such an order a clOrdId of its own, so this is the only
	// field on it that names the order this platform placed.
	AlgoClOrdID string `json:"algoClOrdId"`
}

type okxQueryResp struct {
	Code string     `json:"code"`
	Msg  string     `json:"msg"`
	Data []okxOrder `json:"data"`
}

// placeOrder places a signed order (POST /api/v5/trade/order, weight 1).
func (c *okxREST) placeOrder(ctx context.Context, body map[string]string) (*okxPlaceData, error) {
	if !c.buckets.allow(familyUnverified, 1) {
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
	if !c.buckets.allow(familyUnverified, 1) {
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

// okxFill is ONE EXECUTION as GET /api/v5/trade/fills-history reports it, and
// TradeID is the identity that matters: it is the SAME field the private
// user-data stream carries on a live fill, so a fill rebuilt from here and a
// fill built from the websocket are one fill with one name (#923).
//
// ordId is a DIFFERENT identifier space. Naming a fill after it — which this
// connector did, for one synthesized cumulative fill per order — gave the same
// execution two names, and the order aggregate and the position book both make
// folding exactly-once by deduping on that name.
type okxFill struct {
	InstID  string `json:"instId"`
	TradeID string `json:"tradeId"`
	OrdID   string `json:"ordId"`
	ClOrdID string `json:"clOrdId"`
	Side    string `json:"side"`
	FillPx  string `json:"fillPx"`
	FillSz  string `json:"fillSz"`
	Fee     string `json:"fee"`
	FeeCcy  string `json:"feeCcy"`
	TS      string `json:"ts"`
}

// THE 6-UNIT WEIGHT THAT USED TO LIVE HERE IS NOW A BUCKET (#933).
//
// okxFillsHistoryWeight expressed this endpoint's real 10-per-2s limit in units
// of a bucket sized 60-per-2s for /trade/order — six times tighter, so six units.
// That translation was correct arithmetic and the wrong model: OKX meters PER
// ENDPOINT, so the two are independent remote budgets and charging one against
// the other made the adapter self-throttle at 7 traded orders per window
// (measured, not derived — see okx_buckets_test.go) where the venue permits its
// own 10 fills-history calls alongside 60 placements.
//
// The limit now lives in okx_buckets.go as familyFillsHistory's own bucket, and a
// call against it costs 1. Nothing about the remote limit changed; only which
// budget it is drawn from.

// fillsHistory lists the individual executions behind one order (GET
// /api/v5/trade/fills-history).
//
// ADDRESSED BY OKX'S OWN ordId. There is no clOrdId form of this endpoint, which
// makes it the single place this connector cannot use its own deterministic id —
// the ordId comes off the order query that precedes it, so the pair is still
// anchored on ours.
//
// AN EMPTY data ARRAY IS NOT AN ERROR HERE, and that is the difference from
// queryOrder beside it. queryOrder treats empty as "no such order" because a
// lookup by clOrdId either resolves or does not; a trade list legitimately comes
// back empty for an order whose executions OKX has not published yet, and
// turning that into an APIError would make the caller read a propagation delay
// as a venue refusal. The caller decides what an empty answer means, and its two
// callers answer it differently — see okx_venue.go's tradedFills.
func (c *okxREST) fillsHistory(ctx context.Context, instID, ordID string) ([]okxFill, error) {
	if !c.buckets.allow(familyFillsHistory, 1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	path := "/api/v5/trade/fills-history?instType=SPOT&instId=" + instID + "&ordId=" + ordID
	raw, err := c.signedRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Code string    `json:"code"`
		Msg  string    `json:"msg"`
		Data []okxFill `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("okx: decode fills-history response: %w", err)
	}
	if out.Code != "0" {
		return nil, &APIError{Code: atoiSafe(out.Code), Msg: out.Msg}
	}
	return out.Data, nil
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
	if !c.buckets.allow(familyUnverified, 1) {
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
	if !c.buckets.allow(familyUnverified, 1) {
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

// ExchangeAccountID asks OKX which account this API key belongs to (SOV-02a).
//
// It implements accountproof.Exchange, and it is called ONCE at startup: the adapter
// refuses to trade if OKX names an account other than the one the deployment claims.
// The signature, request, and uid-decode/validation live once in exchangeauth — this
// method only spends the weight budget and delegates.
func (c *okxREST) ExchangeAccountID(ctx context.Context) (string, error) {
	info, err := c.exchangeAccount(ctx)
	return info.ID, err
}

// exchangeAccount reads the identity and account mode from the same signed OKX
// account/config response. The caller never receives credential material.
func (c *okxREST) exchangeAccount(ctx context.Context) (exchangeauth.AccountInfo, error) {
	if !c.buckets.allow(familyUnverified, 1) {
		c.onThrottle()
		return exchangeauth.AccountInfo{}, ErrRateLimited
	}
	return exchangeauth.Account(ctx, "okx", exchangeauth.Credential{
		APIKey:     c.apiKey,
		APISecret:  string(c.apiSecret),
		Passphrase: c.passphrase,
	}, exchangeauth.Options{
		BaseURL:    c.baseURL,
		HTTPClient: c.httpc,
		Now:        c.now,
		// THE SAME MODE THE ORDERS USE, and it has to be (#147). This call is the
		// account proof: it asks OKX which account the key belongs to and the
		// adapter refuses to trade if the answer is not the configured one. Demo
		// and live are SEPARATE accounts with separate uids, so proving against
		// the wrong book would either fail outright or — worse — verify a uid the
		// orders will never touch, turning the proof into a formality.
		OKXTrading: c.mode,
	})
}

// tickerPrice returns the last price for an instrument (public GET
// /api/v5/market/ticker, unsigned) — feeds the MarkSource seam.
func (c *okxREST) tickerPrice(ctx context.Context, instID string) (string, error) {
	if !c.buckets.allow(familyUnverified, 1) {
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
// the JSON payload (empty for GET). The OK-ACCESS-* headers are built by
// exchangeauth.SignOKX — the one place that knows the OKX signature scheme.
func (c *okxREST) signedRequest(ctx context.Context, method, requestPath string, body map[string]string) ([]byte, error) {
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = json.Marshal(body)
	}
	return c.signedRequestRaw(ctx, method, requestPath, bodyBytes)
}

// signedRequestRaw is signedRequest for a body that is not a flat object.
// /trade/cancel-algos takes a JSON ARRAY, and the signature covers the body
// verbatim — so the marshalling and the signing must not be separated, or the
// bytes signed stop being the bytes sent.
//
// ONE SIGNING PATH, deliberately: signedRequest marshals and delegates here
// rather than repeating the header construction, because a second copy of an
// HMAC preamble is a second place for the demo-trading header to be forgotten,
// and an unmarked request at OKX is a LIVE one.
func (c *okxREST) signedRequestRaw(ctx context.Context, method, requestPath string, bodyBytes []byte) ([]byte, error) {
	ts := c.now()

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+requestPath, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	cred := exchangeauth.Credential{APIKey: c.apiKey, APISecret: string(c.apiSecret), Passphrase: c.passphrase}
	if err := exchangeauth.SignOKX(req.Header, cred, ts, method, requestPath, string(bodyBytes), c.mode); err != nil {
		// Refuse to send rather than send unmarked. An unmarked request is a LIVE
		// request at OKX, so "we could not tell which book this was for" must never
		// resolve to the one that spends real money.
		return nil, err
	}

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

// ===== CONDITIONAL (STOP) ORDERS (#485) =====
//
// A stop is not a variant of a regular order at OKX — it is a separate product
// on a separate endpoint, and everything below was measured against the demo API
// on 2026-08-15 rather than read from documentation.
//
// WHAT MAKES IT WORKABLE: our own id addresses it at every endpoint that
// matters. algoClOrdId is accepted on placement, resolves the order on query
// WITHOUT an instId, and cancels it with no algoId round trip. So this connector
// stays stateless — it never has to remember an exchange-assigned identifier.
//
// WHAT NEARLY MADE IT UNWORKABLE: the algo order is not the order that fills. It
// CREATES one when it triggers, and OKX gives that order a clOrdId of its own
// (prefixed "O"), not ours. Ours survives on algoClOrdId, which is why
// okx_userdata.go reads that field first — without it a triggered stop's fill
// would arrive keyed by an id this platform has never seen.

// okxAlgoOrder is one element of GET /api/v5/trade/order-algo.
//
// state is the algo order's own lifecycle, NOT the triggered order's:
// "live" (waiting), "effective" (fired), "canceled", "order_failed", "pause".
// actualSz is what the triggered order actually took.
type okxAlgoOrder struct {
	AlgoID      string `json:"algoId"`
	AlgoClOrdID string `json:"algoClOrdId"`
	InstID      string `json:"instId"`
	State       string `json:"state"`
	Side        string `json:"side"`
	Sz          string `json:"sz"`
	ActualSz    string `json:"actualSz"`
	ActualPx    string `json:"actualPx"`
	TriggerPx   string `json:"triggerPx"`
	OrdPx       string `json:"ordPx"`
	FailCode    string `json:"failCode"`
}

type okxAlgoResp struct {
	Code string         `json:"code"`
	Msg  string         `json:"msg"`
	Data []okxAlgoOrder `json:"data"`
}

// placeAlgoOrder places a conditional order (POST /api/v5/trade/order-algo).
//
// The response shape is the same okxPlaceResp the regular endpoint returns —
// sCode "0" means the order itself was accepted — so the caller's success and
// idempotency handling is unchanged.
func (c *okxREST) placeAlgoOrder(ctx context.Context, body map[string]string) (*okxPlaceData, error) {
	if !c.buckets.allow(familyUnverified, 1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	raw, err := c.signedRequest(ctx, http.MethodPost, "/api/v5/trade/order-algo", body)
	if err != nil {
		return nil, err
	}
	var out okxPlaceResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("okx: decode algo place response: %w", err)
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

// queryAlgoOrder fetches a conditional order's true state by OUR id.
//
// NO instId, DELIBERATELY — the algo endpoint does not require one, unlike
// /trade/order which refuses without it (50014). That is a real simplification:
// the recovery path here needs no symbol mapping, so it cannot fail for the one
// reason the regular path can.
func (c *okxREST) queryAlgoOrder(ctx context.Context, algoClOrdID string) (*okxAlgoOrder, error) {
	if !c.buckets.allow(familyUnverified, 1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	raw, err := c.signedRequest(ctx, http.MethodGet,
		"/api/v5/trade/order-algo?algoClOrdId="+algoClOrdID, nil)
	if err != nil {
		return nil, err
	}
	var out okxAlgoResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("okx: decode algo order: %w", err)
	}
	if out.Code != "0" || len(out.Data) == 0 {
		return nil, &APIError{Code: atoiSafe(out.Code), Msg: out.Msg}
	}
	return &out.Data[0], nil
}

// cancelAlgoOrder withdraws a resting conditional order by OUR id (POST
// /api/v5/trade/cancel-algos).
//
// THE BODY IS A JSON ARRAY, which is why this signs raw bytes rather than going
// through the map[string]string path — the signature covers the body verbatim,
// so the marshalling and the signing cannot be separated.
//
// Addressed by algoClOrdId rather than algoId, so a retried cancel resolves to
// the original order and this connector never has to have remembered an
// exchange-assigned identifier.
func (c *okxREST) cancelAlgoOrder(ctx context.Context, instID, algoClOrdID string) (*okxPlaceData, error) {
	if !c.buckets.allow(familyUnverified, 1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	body, err := json.Marshal([]map[string]string{{"instId": instID, "algoClOrdId": algoClOrdID}})
	if err != nil {
		return nil, fmt.Errorf("okx: encode algo cancel: %w", err)
	}
	raw, err := c.signedRequestRaw(ctx, http.MethodPost, "/api/v5/trade/cancel-algos", body)
	if err != nil {
		return nil, err
	}
	var out okxPlaceResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("okx: decode algo cancel response: %w", err)
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

// queryTriggeredOrder finds the REGULAR order a conditional order created when
// it fired, by scanning recent history for our id (#485).
//
// # Why a scan, and why there is no alternative
//
// A triggered order cannot be addressed by our id on the regular endpoint. OKX
// gives it a clOrdId of its own (observed: "O3833848819892766720"), and
// /trade/order accepts only ordId or clOrdId — neither of which we know. Our id
// is present on the order, as algoClOrdId, but is not a lookup key there.
//
// So this asks for recent orders on the instrument and matches. It is the
// BACKSTOP, not the primary path: fills reach this platform through the
// user-data stream, which carries algoClOrdId and needs no lookup at all. This
// exists so that a missed stream event does not leave a triggered stop
// unreconciled forever, which is the one failure reconciliation is for.
//
// Bounded at one page. A stop whose trigger fired long enough ago to fall off it
// is past the point where this backstop helps, and paging further would turn one
// recon tick into an unbounded walk of the order history.
func (c *okxREST) queryTriggeredOrder(ctx context.Context, instID, algoClOrdID string) (*okxOrder, error) {
	if !c.buckets.allow(familyUnverified, 1) {
		c.onThrottle()
		return nil, ErrRateLimited
	}
	raw, err := c.signedRequest(ctx, http.MethodGet,
		"/api/v5/trade/orders-history?instType=SPOT&instId="+instID+"&limit=100", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Code string     `json:"code"`
		Msg  string     `json:"msg"`
		Data []okxOrder `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("okx: decode order history: %w", err)
	}
	if out.Code != "0" {
		return nil, &APIError{Code: atoiSafe(out.Code), Msg: out.Msg}
	}
	for i := range out.Data {
		if out.Data[i].AlgoClOrdID == algoClOrdID {
			return &out.Data[i], nil
		}
	}
	return nil, &APIError{Code: okxOrderDoesNotExist, Msg: "okx: no triggered order found for " + algoClOrdID}
}

// okxOrderDoesNotExist is OKX's 51603. Reported when a lookup finds nothing —
// including the regular endpoint asked about a CONDITIONAL order, which is why
// reconciliation must choose its endpoint by order type rather than treating
// this code as "the venue has forgotten the order" (#485).
const okxOrderDoesNotExist = 51603
