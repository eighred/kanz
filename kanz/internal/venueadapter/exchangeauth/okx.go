package exchangeauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/eighred/kanz/internal/execution"
)

// OKXTradingMode selects which OKX order book a signed request reaches (#147).
//
// IT IS A SEPARATE AXIS FROM THE HOSTNAME, and that is the entire reason this
// type exists. Binance publishes testnet.binance.vision: a distinct host, so the
// URL answers "is this real money?" and a key for one is rejected by the other.
// OKX publishes NO demo hostname. Demo and production are the SAME host,
// www.okx.com, and are distinguished per-request by `x-simulated-trading: 1`.
//
// So for OKX the endpoint cannot answer the question and only the caller can.
// Before this type, nothing asked: every signed request went to production
// because the header was absent, and no amount of manifest review would show it
// — the adapter was not misconfigured, it was structurally incapable of demo.
//
// THE ZERO VALUE IS NOT A MODE. An unset OKXTradingMode is rejected rather than
// treated as either option. "Nobody stated the mode" and "somebody chose live"
// must never be the same state on the one code path measured in real money —
// the same rule that already removed the OKX_BASE_URL default.
type OKXTradingMode string

const (
	// OKXLive reaches the real order book. Orders settle in real money.
	OKXLive OKXTradingMode = "live"
	// OKXDemo reaches OKX's demo book via `x-simulated-trading: 1`. Requires a
	// demo API key — a live key sent with this header is rejected by OKX, which
	// is the correct direction to fail.
	OKXDemo OKXTradingMode = "demo"
)

// simulatedTradingHeader is OKX's demo-trading switch. Its VALUE being "1" and
// its presence are the same signal — OKX treats any request carrying it as demo.
const simulatedTradingHeader = "x-simulated-trading"

// ErrUnknownOKXTradingMode is returned when a request would be signed without a
// stated mode. It is an error and not a fallback: falling back would reintroduce
// exactly the defect #147 records.
var ErrUnknownOKXTradingMode = errors.New("exchangeauth: okx: trading mode is unset or unrecognised — " +
	"pass OKXLive or OKXDemo explicitly; there is no default, because a default here is a default about " +
	"whether orders are real")

// SignOKX sets the OK-ACCESS-* headers for one request on h. ts is the request
// timestamp, body the JSON payload ("" for GET). OK-ACCESS-SIGN =
// base64(HMAC-SHA256(timestamp + method + requestPath + body)) — matches
// venue-okx's signedRequest exactly.
//
// mode is REQUIRED and has no default, so no OKX request can be signed without
// someone stating which book it reaches. It is a parameter rather than a field
// on Credential because it is not a property of the key: the same code path
// signs both, and the compiler is what makes it impossible to forget.
//
// Returns an error only for an unstated mode; signing itself cannot fail.
func SignOKX(h http.Header, cred Credential, ts time.Time, method, requestPath, body string, mode OKXTradingMode) error {
	switch mode {
	case OKXLive, OKXDemo:
	default:
		return fmt.Errorf("%w (got %q)", ErrUnknownOKXTradingMode, string(mode))
	}

	stamp := ts.UTC().Format("2006-01-02T15:04:05.000Z")
	prehash := stamp + method + requestPath + body
	mac := hmac.New(sha256.New, []byte(cred.APISecret))
	mac.Write([]byte(prehash))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	h.Set("OK-ACCESS-KEY", cred.APIKey)
	h.Set("OK-ACCESS-SIGN", sign)
	h.Set("OK-ACCESS-TIMESTAMP", stamp)
	h.Set("OK-ACCESS-PASSPHRASE", cred.Passphrase)
	h.Set("Content-Type", "application/json")

	// SET ON DEMO, and explicitly DELETED on live rather than merely not set.
	// h may be a reused header map, and a stale demo header surviving into a live
	// request would be the one direction of this bug nobody would report: orders
	// silently landing on the demo book while the operator believes they are real.
	if mode == OKXDemo {
		h.Set(simulatedTradingHeader, "1")
	} else {
		h.Del(simulatedTradingHeader)
	}
	return nil
}

// okxAccountConfig is GET /api/v5/account/config — the account behind the
// credential.
type okxAccountConfig struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		// UID is OKX's own id for the account this API key belongs to. OKX returns
		// it as a STRING (unlike Binance's numeric uid), so it is kept as one — the
		// value is an identity to compare, never a number to do arithmetic on.
		UID string `json:"uid"`
		// AcctLv is OKX's account mode: 1 spot, 2 futures/single-currency,
		// 3 multi-currency margin, 4 portfolio margin.
		AcctLv string `json:"acctLv"`
	} `json:"data"`
}

// okxAccount asks OKX which account cred belongs to (GET
// /api/v5/account/config). An empty uid is an ERROR, never "" — an empty id would
// compare equal to nothing and would ride upstream as a verified account.
func okxAccount(ctx context.Context, cred Credential, opt Options) (AccountInfo, error) {
	const requestPath = "/api/v5/account/config"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opt.BaseURL+requestPath, nil)
	if err != nil {
		return AccountInfo{}, err
	}
	// THE MODE MATTERS ON A READ, TOO. Demo and live are separate OKX accounts
	// with separate keys, so asking live "which account is this?" with a demo key
	// does not merely fail — before the header existed it was the only thing this
	// call could do, and a verified-looking uid from the wrong environment is
	// worse than a refusal.
	if err := SignOKX(req.Header, cred, opt.Now(), http.MethodGet, requestPath, "", opt.OKXTrading); err != nil {
		return AccountInfo{}, err
	}

	raw, err := do(req, opt.HTTPClient)
	if err != nil {
		return AccountInfo{}, err
	}
	var out okxAccountConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		return AccountInfo{}, fmt.Errorf("exchangeauth: okx: decode account config: %w", err)
	}
	if out.Code != "0" {
		return AccountInfo{}, &execution.APIError{Code: atoiSafe(out.Code), Msg: out.Msg}
	}
	if len(out.Data) == 0 || out.Data[0].UID == "" {
		return AccountInfo{}, errors.New("exchangeauth: okx: GET /api/v5/account/config carried no uid — the exchange did not " +
			"say which account this API key belongs to, so it cannot be verified")
	}
	return AccountInfo{ID: out.Data[0].UID, AccountLevel: out.Data[0].AcctLv}, nil
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
