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

	"github.com/kanz-eng/kanz/internal/execution"
)

// SignOKX sets the OK-ACCESS-* headers for one request on h. ts is the request
// timestamp, body the JSON payload ("" for GET). OK-ACCESS-SIGN =
// base64(HMAC-SHA256(timestamp + method + requestPath + body)) — matches
// venue-okx's signedRequest exactly.
func SignOKX(h http.Header, cred Credential, ts time.Time, method, requestPath, body string) {
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
	} `json:"data"`
}

// okxAccountID asks OKX which account cred belongs to (GET
// /api/v5/account/config). An empty uid is an ERROR, never "" — an empty id would
// compare equal to nothing and would ride upstream as a verified account.
func okxAccountID(ctx context.Context, cred Credential, opt Options) (string, error) {
	const requestPath = "/api/v5/account/config"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opt.BaseURL+requestPath, nil)
	if err != nil {
		return "", err
	}
	SignOKX(req.Header, cred, opt.Now(), http.MethodGet, requestPath, "")

	raw, err := do(req, opt.HTTPClient)
	if err != nil {
		return "", err
	}
	var out okxAccountConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("exchangeauth: okx: decode account config: %w", err)
	}
	if out.Code != "0" {
		return "", &execution.APIError{Code: atoiSafe(out.Code), Msg: out.Msg}
	}
	if len(out.Data) == 0 || out.Data[0].UID == "" {
		return "", errors.New("exchangeauth: okx: GET /api/v5/account/config carried no uid — the exchange did not " +
			"say which account this API key belongs to, so it cannot be verified")
	}
	return out.Data[0].UID, nil
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
