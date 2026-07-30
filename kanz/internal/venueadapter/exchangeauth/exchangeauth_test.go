package exchangeauth

// exchangeauth is the one place that knows how to sign an exchange REST call and
// ask the exchange which account a credential belongs to. These tests drive both
// venues against a fake HTTP server and assert the signature/header shape matches
// what the venue-okx and venue-binance REST clients do today, plus the invariants
// that make this package safe for the operator's pre-write key proof (S4b Task 4):
// an exchange response with no account id is always an error, never "", and no
// credential material ever appears in an error string.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
)

// testCred uses distinctive sentinel values so TestNoCredentialInErrors can prove
// none of them ever leak into an error string.
var testCred = Credential{
	APIKey:     "KEYSENTINEL",
	APISecret:  "SECSENTINEL",
	Passphrase: "PASSENTINEL",
}

// okxAccountServer fakes GET /api/v5/account/config. It independently recomputes
// the OK-ACCESS-SIGN the caller should have sent and fails the test if it does not
// match, then responds with status/body.
func okxAccountServer(t *testing.T, secret string, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/account/config" {
			t.Errorf("okx: path = %q, want /api/v5/account/config", r.URL.Path)
		}
		if got := r.Header.Get("OK-ACCESS-KEY"); got != testCred.APIKey {
			t.Errorf("okx: OK-ACCESS-KEY = %q, want %q", got, testCred.APIKey)
		}
		if got := r.Header.Get("OK-ACCESS-PASSPHRASE"); got != testCred.Passphrase {
			t.Errorf("okx: OK-ACCESS-PASSPHRASE = %q, want %q", got, testCred.Passphrase)
		}
		ts := r.Header.Get("OK-ACCESS-TIMESTAMP")
		if _, err := time.Parse("2006-01-02T15:04:05.000Z", ts); err != nil {
			t.Errorf("okx: OK-ACCESS-TIMESTAMP = %q, bad format: %v", ts, err)
		}
		prehash := ts + "GET" + "/api/v5/account/config"
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(prehash))
		wantSign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if got := r.Header.Get("OK-ACCESS-SIGN"); got != wantSign {
			t.Errorf("okx: OK-ACCESS-SIGN = %q, want %q", got, wantSign)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// binanceAccountServer fakes GET /api/v3/account. It independently recomputes the
// &signature= value the caller should have sent (hex HMAC-SHA256 over the query
// string without "&signature=") and fails the test if it does not match.
func binanceAccountServer(t *testing.T, secret string, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/account" {
			t.Errorf("binance: path = %q, want /api/v3/account", r.URL.Path)
		}
		if got := r.Header.Get("X-MBX-APIKEY"); got != testCred.APIKey {
			t.Errorf("binance: X-MBX-APIKEY = %q, want %q", got, testCred.APIKey)
		}
		raw := r.URL.RawQuery
		idx := strings.Index(raw, "&signature=")
		if idx < 0 {
			t.Errorf("binance: query %q missing &signature=", raw)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		query := raw[:idx]
		sig := raw[idx+len("&signature="):]

		q, err := url.ParseQuery(query)
		if err != nil {
			t.Errorf("binance: bad query %q: %v", query, err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if q.Get("timestamp") == "" {
			t.Errorf("binance: missing timestamp query param")
		}
		if got := q.Get("recvWindow"); got != "5000" {
			t.Errorf("binance: recvWindow = %q, want 5000", got)
		}

		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(query))
		wantSig := hex.EncodeToString(mac.Sum(nil))
		if sig != wantSig {
			t.Errorf("binance: signature = %q, want %q", sig, wantSig)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestAccountIDOKX(t *testing.T) {
	srv := okxAccountServer(t, testCred.APISecret, http.StatusOK, `{"code":"0","data":[{"uid":"4711"}]}`)
	defer srv.Close()

	got, err := AccountID(context.Background(), "okx", testCred, Options{BaseURL: srv.URL, OKXTrading: OKXDemo})
	if err != nil {
		t.Fatalf("AccountID: %v", err)
	}
	if got != "4711" {
		t.Errorf("AccountID = %q, want 4711", got)
	}
}

func TestAccountIDBinance(t *testing.T) {
	srv := binanceAccountServer(t, testCred.APISecret, http.StatusOK, `{"uid":8822}`)
	defer srv.Close()

	got, err := AccountID(context.Background(), "binance", testCred, Options{BaseURL: srv.URL, OKXTrading: OKXDemo})
	if err != nil {
		t.Fatalf("AccountID: %v", err)
	}
	if got != "8822" {
		t.Errorf("AccountID = %q, want 8822", got)
	}
}

func TestAccountIDRejectsMissingUID(t *testing.T) {
	cases := []struct {
		name  string
		venue string
		body  string
	}{
		{"okx empty data", "okx", `{"code":"0","data":[]}`},
		{"okx empty uid", "okx", `{"code":"0","data":[{"uid":""}]}`},
		{"binance zero uid", "binance", `{"uid":0}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var srv *httptest.Server
			if tc.venue == "okx" {
				srv = okxAccountServer(t, testCred.APISecret, http.StatusOK, tc.body)
			} else {
				srv = binanceAccountServer(t, testCred.APISecret, http.StatusOK, tc.body)
			}
			defer srv.Close()

			got, err := AccountID(context.Background(), tc.venue, testCred, Options{BaseURL: srv.URL, OKXTrading: OKXDemo})
			if err == nil {
				t.Fatal("AccountID: want error for a response carrying no account id, got nil")
			}
			if got != "" {
				t.Errorf("AccountID = %q, want empty string alongside the error", got)
			}
		})
	}
}

func TestAccountIDMapsAuthFailure(t *testing.T) {
	for _, venue := range []string{"okx", "binance"} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			t.Run(fmt.Sprintf("%s-%d", venue, status), func(t *testing.T) {
				var srv *httptest.Server
				if venue == "okx" {
					srv = okxAccountServer(t, testCred.APISecret, status, `{}`)
				} else {
					srv = binanceAccountServer(t, testCred.APISecret, status, `{}`)
				}
				defer srv.Close()

				_, err := AccountID(context.Background(), venue, testCred, Options{BaseURL: srv.URL, OKXTrading: OKXDemo})
				if !errors.Is(err, execution.ErrEgressDenied) {
					t.Errorf("AccountID err = %v, want wrapping execution.ErrEgressDenied", err)
				}
			})
		}
	}
}

func TestAccountIDTypedError(t *testing.T) {
	cases := []struct {
		name, venue, body string
	}{
		{"okx", "okx", `{"code":"50111","msg":"Invalid API key"}`},
		{"binance", "binance", `{"code":-2015,"msg":"Invalid API-key"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var srv *httptest.Server
			if tc.venue == "okx" {
				srv = okxAccountServer(t, testCred.APISecret, http.StatusOK, tc.body)
			} else {
				srv = binanceAccountServer(t, testCred.APISecret, http.StatusOK, tc.body)
			}
			defer srv.Close()

			_, err := AccountID(context.Background(), tc.venue, testCred, Options{BaseURL: srv.URL, OKXTrading: OKXDemo})
			var apiErr *execution.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("AccountID err = %v, want *execution.APIError via errors.As", err)
			}
		})
	}
}

func TestAccountIDUnsupportedVenue(t *testing.T) {
	_, err := AccountID(context.Background(), "kraken", testCred, Options{BaseURL: "http://example.invalid"})
	if !errors.Is(err, ErrUnsupportedVenue) {
		t.Errorf("AccountID err = %v, want ErrUnsupportedVenue", err)
	}
}

// TestNoCredentialInErrors is a real invariant of this slice: none of the failure
// paths above may ever put the api key, secret, or passphrase into an error
// string. testCred's sentinel values make a leak trivially detectable.
func TestNoCredentialInErrors(t *testing.T) {
	var errs []error

	for _, tc := range []struct{ venue, body string }{
		{"okx", `{"code":"0","data":[]}`},
		{"okx", `{"code":"0","data":[{"uid":""}]}`},
		{"binance", `{"uid":0}`},
	} {
		var srv *httptest.Server
		if tc.venue == "okx" {
			srv = okxAccountServer(t, testCred.APISecret, http.StatusOK, tc.body)
		} else {
			srv = binanceAccountServer(t, testCred.APISecret, http.StatusOK, tc.body)
		}
		_, err := AccountID(context.Background(), tc.venue, testCred, Options{BaseURL: srv.URL, OKXTrading: OKXDemo})
		errs = append(errs, err)
		srv.Close()
	}

	for _, venue := range []string{"okx", "binance"} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			var srv *httptest.Server
			if venue == "okx" {
				srv = okxAccountServer(t, testCred.APISecret, status, `{}`)
			} else {
				srv = binanceAccountServer(t, testCred.APISecret, status, `{}`)
			}
			_, err := AccountID(context.Background(), venue, testCred, Options{BaseURL: srv.URL, OKXTrading: OKXDemo})
			errs = append(errs, err)
			srv.Close()
		}
	}

	for _, tc := range []struct{ venue, body string }{
		{"okx", `{"code":"50111","msg":"Invalid API key"}`},
		{"binance", `{"code":-2015,"msg":"Invalid API-key"}`},
	} {
		var srv *httptest.Server
		if tc.venue == "okx" {
			srv = okxAccountServer(t, testCred.APISecret, http.StatusOK, tc.body)
		} else {
			srv = binanceAccountServer(t, testCred.APISecret, http.StatusOK, tc.body)
		}
		_, err := AccountID(context.Background(), tc.venue, testCred, Options{BaseURL: srv.URL, OKXTrading: OKXDemo})
		errs = append(errs, err)
		srv.Close()
	}

	_, err := AccountID(context.Background(), "kraken", testCred, Options{BaseURL: "http://example.invalid"})
	errs = append(errs, err)

	for i, err := range errs {
		if err == nil {
			t.Fatalf("case %d: expected a non-nil error", i)
		}
		s := err.Error()
		if strings.Contains(s, testCred.APIKey) {
			t.Errorf("case %d: error string leaked the api key: %q", i, s)
		}
		if strings.Contains(s, testCred.APISecret) {
			t.Errorf("case %d: error string leaked the api secret: %q", i, s)
		}
		if strings.Contains(s, testCred.Passphrase) {
			t.Errorf("case %d: error string leaked the passphrase: %q", i, s)
		}
	}
}

func TestVenues(t *testing.T) {
	got := Venues()
	want := []string{"binance", "okx"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Venues() = %v, want %v", got, want)
	}
}
