// Package exchangeauth is the one place that knows how to sign an exchange REST
// call and ask the exchange which account a credential belongs to. It centralises
// what the venue-okx and venue-binance adapters already did privately (each
// behind its own `internal` boundary, unreachable from the operator) so both the
// venue adapters and the operator's pre-write key proof can call the same code
// instead of drifting apart.
//
// Rate limiting is deliberately NOT this package's job: the venue adapters spend
// their own weight budget before delegating here, and the operator has no budget
// to spend.
package exchangeauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/execution"
)

// Credential is a candidate exchange API key set. It is an input only: no
// function in this package logs it, returns it, or puts it in an error.
type Credential struct {
	APIKey     string
	APISecret  string
	Passphrase string // OKX only; empty for Binance
}

// Options tunes one call. BaseURL is required; the rest default.
type Options struct {
	BaseURL    string           // e.g. "https://www.okx.com" — required
	HTTPClient *http.Client     // default: &http.Client{Timeout: 10 * time.Second}
	Now        func() time.Time // default: time.Now

	// OKXTrading states which OKX book a request reaches. REQUIRED for venue
	// "okx" and ignored for every other venue (#147).
	//
	// It lives here rather than being derived from BaseURL because for OKX it
	// CANNOT be derived: demo and production share www.okx.com and differ only by
	// the `x-simulated-trading: 1` header. There is deliberately no default —
	// resolve() below does not fill this in, because the safe value depends on
	// what the caller meant, and guessing is what #147 was.
	OKXTrading OKXTradingMode
}

// resolve applies Options defaults, returning a copy that is safe to use without
// further nil checks.
func (o Options) resolve() Options {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// ErrUnsupportedVenue names a venue exchangeauth cannot prove.
var ErrUnsupportedVenue = errors.New("exchangeauth: unsupported venue")

// Venues returns the venue ids this package can prove, sorted.
func Venues() []string {
	venues := []string{"binance", "okx"}
	sort.Strings(venues)
	return venues
}

// AccountInfo is the exchange's own identity and capability metadata for the
// account behind a credential. AccountLevel is populated by OKX from acctLv and
// intentionally remains opaque here; only the OKX adapter interprets it.
type AccountInfo struct {
	ID           string
	AccountLevel string
}

// Account asks the venue which exchange account cred belongs to. An exchange
// response carrying no id is an error, never an empty, verified-looking record.
func Account(ctx context.Context, venue string, cred Credential, opt Options) (AccountInfo, error) {
	opt = opt.resolve()
	switch venue {
	case "okx":
		return okxAccount(ctx, cred, opt)
	case "binance":
		return binanceAccount(ctx, cred, opt)
	default:
		return AccountInfo{}, fmt.Errorf("%w: %q", ErrUnsupportedVenue, venue)
	}
}

// AccountID preserves the narrow account-proof API for callers that do not need
// venue capability metadata.
func AccountID(ctx context.Context, venue string, cred Credential, opt Options) (string, error) {
	info, err := Account(ctx, venue, cred, opt)
	return info.ID, err
}

// do executes req and returns the body for any status <500. It reads at most
// 1<<20 bytes, maps 401/403 to execution.ErrEgressDenied, and maps >=500 to a
// plain status error — the same shape the venue clients use today.
func do(req *http.Request, client *http.Client) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %s status %d", execution.ErrEgressDenied, req.URL.Path, resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("exchangeauth: %s: status %d", req.URL.Path, resp.StatusCode)
	}
	return raw, nil
}
