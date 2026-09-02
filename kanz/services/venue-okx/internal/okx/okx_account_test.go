package okx

// SOV-02a: the adapter holds the API key, so it is the only process that can ask OKX
// whose account it is. GET /api/v5/account/config returns the uid behind the
// credential — the exchange's own name for the collateral pool this adapter's fills
// margin against.

import (
	"context"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"testing"
	"time"
)

func okxRESTOver(f *fakeOKX) *okxREST {
	return newOKXREST(okxRestConfig{
		BaseURL:    f.srv.URL,
		APIKey:     "k",
		APISecret:  "s",
		Passphrase: "p",
		Buckets:    newOKXBuckets(NewWeightBucket(60, 2*time.Second, nil), nil),
		Mode:       exchangeauth.OKXDemo,
	})
}

// TestOKXExchangeAccountIDReadsTheUID: OKX names the account behind the key.
func TestOKXExchangeAccountIDReadsTheUID(t *testing.T) {
	f := newFakeOKX(t)
	f.configBody = `{"code":"0","msg":"","data":[{"uid":"44556677","acctLv":"2"}]}`

	got, err := okxRESTOver(f).ExchangeAccountID(context.Background())
	if err != nil {
		t.Fatalf("ExchangeAccountID: %v", err)
	}
	if got != "44556677" {
		t.Errorf("uid = %q, want 44556677", got)
	}
}

// TestOKXExchangeAccountIDRefusesAnEmptyUID: an OK envelope carrying no uid has not
// told us whose key this is. It must error, never return "" — an empty id would be
// carried upstream as a verified account.
func TestOKXExchangeAccountIDRefusesAnEmptyUID(t *testing.T) {
	f := newFakeOKX(t)
	f.configBody = `{"code":"0","msg":"","data":[{"acctLv":"2"}]}`

	if _, err := okxRESTOver(f).ExchangeAccountID(context.Background()); err == nil {
		t.Error("ExchangeAccountID accepted an account config carrying no uid")
	}
}

// TestOKXExchangeAccountIDRefusesAnEmptyEnvelope: code 0 with no data rows at all.
func TestOKXExchangeAccountIDRefusesAnEmptyEnvelope(t *testing.T) {
	f := newFakeOKX(t)
	f.configBody = `{"code":"0","msg":"","data":[]}`

	if _, err := okxRESTOver(f).ExchangeAccountID(context.Background()); err == nil {
		t.Error("ExchangeAccountID accepted an account config with no account in it")
	}
}

// TestOKXExchangeAccountIDSurfacesAnAPIError: a rejected key must not read as "no
// account".
func TestOKXExchangeAccountIDSurfacesAnAPIError(t *testing.T) {
	f := newFakeOKX(t)
	f.configBody = `{"code":"50111","msg":"Invalid OK-ACCESS-KEY","data":[]}`

	if _, err := okxRESTOver(f).ExchangeAccountID(context.Background()); err == nil {
		t.Error("ExchangeAccountID swallowed an API error")
	}
}
