package exchangeauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/eighred/kanz/internal/execution"
)

// SignBinance returns the hex HMAC-SHA256 of query under secret — the value
// Binance expects as the &signature= parameter. Matches venue-binance's sign
// exactly.
func SignBinance(secret, query string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(query))
	return hex.EncodeToString(mac.Sum(nil))
}

// binanceAccountInfo is GET /api/v3/account — the uid says WHOSE account this
// API key belongs to.
type binanceAccountInfo struct {
	// UID is Binance's own id for the account behind this API key.
	UID  int64  `json:"uid"`
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// binanceAccountID asks Binance which account cred belongs to (GET
// /api/v3/account with timestamp+recvWindow=5000). A response with no uid is an
// ERROR, never "" — an empty id would compare equal to nothing and would sail
// upstream as a verified account.
func binanceAccount(ctx context.Context, cred Credential, opt Options) (AccountInfo, error) {
	const path = "/api/v3/account"

	params := make(map[string]string, 2)
	params["timestamp"] = strconv.FormatInt(opt.Now().UnixMilli(), 10)
	params["recvWindow"] = "5000"

	query := "recvWindow=" + params["recvWindow"] + "&timestamp=" + params["timestamp"]
	sig := SignBinance(cred.APISecret, query)
	full := opt.BaseURL + path + "?" + query + "&signature=" + sig

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return AccountInfo{}, err
	}
	req.Header.Set("X-MBX-APIKEY", cred.APIKey)

	raw, err := do(req, opt.HTTPClient)
	if err != nil {
		return AccountInfo{}, err
	}
	var out binanceAccountInfo
	if err := json.Unmarshal(raw, &out); err != nil {
		return AccountInfo{}, fmt.Errorf("exchangeauth: binance: decode account: %w", err)
	}
	if out.Code != 0 {
		return AccountInfo{}, &execution.APIError{Code: out.Code, Msg: out.Msg}
	}
	if out.UID == 0 {
		return AccountInfo{}, errors.New("exchangeauth: binance: GET /api/v3/account carried no uid — the exchange did not say " +
			"which account this API key belongs to, so it cannot be verified")
	}
	return AccountInfo{ID: strconv.FormatInt(out.UID, 10)}, nil
}
