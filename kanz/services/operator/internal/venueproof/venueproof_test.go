package venueproof

// venueproof is the operator's pre-write proof caller: given a venue id and a
// candidate key set, it looks up that venue's configured exchange base URL and
// delegates to exchangeauth.AccountID. These tests cover the endpoint lookup
// and error propagation; the signing/parsing behaviour itself is exchangeauth's
// own test suite (kanz/internal/venueadapter/exchangeauth).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kanz-eng/kanz/internal/venueadapter/exchangeauth"
	"github.com/kanz-eng/kanz/services/operator/internal/secrets"
)

// testKeys uses distinctive sentinel values so the auth-failure test can prove
// none of them ever leak into an error string.
var testKeys = secrets.VenueKeys{
	APIKey:     "KEYSENTINEL",
	APISecret:  "SECSENTINEL",
	Passphrase: "PASSENTINEL",
}

func okxServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func binanceServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestProveAccountReturnsUID(t *testing.T) {
	okx := okxServer(t, http.StatusOK, `{"code":"0","data":[{"uid":"4711"}]}`)
	defer okx.Close()
	binance := binanceServer(t, http.StatusOK, `{"uid":8822}`)
	defer binance.Close()

	p := New(map[string]string{"okx": okx.URL, "binance": binance.URL}, nil)

	got, err := p.ProveAccount(context.Background(), "okx", testKeys)
	if err != nil {
		t.Fatalf("ProveAccount(okx): %v", err)
	}
	if got != "4711" {
		t.Errorf("ProveAccount(okx) = %q, want 4711", got)
	}

	got, err = p.ProveAccount(context.Background(), "binance", testKeys)
	if err != nil {
		t.Fatalf("ProveAccount(binance): %v", err)
	}
	if got != "8822" {
		t.Errorf("ProveAccount(binance) = %q, want 8822", got)
	}
}

func TestProveAccountNoEndpoint(t *testing.T) {
	p := New(map[string]string{"okx": "http://example.invalid"}, nil)

	_, err := p.ProveAccount(context.Background(), "binance", testKeys)
	if !errors.Is(err, ErrNoEndpoint) {
		t.Errorf("ProveAccount err = %v, want ErrNoEndpoint", err)
	}
}

func TestProveAccountUnsupportedVenue(t *testing.T) {
	p := New(map[string]string{"kraken": "http://example.invalid"}, nil)

	_, err := p.ProveAccount(context.Background(), "kraken", testKeys)
	if err == nil {
		t.Fatal("ProveAccount: want error for an unsupported venue, got nil")
	}
	if !errors.Is(err, exchangeauth.ErrUnsupportedVenue) {
		t.Errorf("ProveAccount err = %v, want exchangeauth.ErrUnsupportedVenue", err)
	}
}

func TestProveAccountPropagatesAuthFailure(t *testing.T) {
	okx := okxServer(t, http.StatusUnauthorized, `{}`)
	defer okx.Close()

	p := New(map[string]string{"okx": okx.URL}, nil)

	_, err := p.ProveAccount(context.Background(), "okx", testKeys)
	if err == nil {
		t.Fatal("ProveAccount: want error for an auth failure, got nil")
	}
	s := err.Error()
	if strings.Contains(s, testKeys.APIKey) || strings.Contains(s, testKeys.APISecret) || strings.Contains(s, testKeys.Passphrase) {
		t.Errorf("ProveAccount error leaked credential material: %q", s)
	}
}
