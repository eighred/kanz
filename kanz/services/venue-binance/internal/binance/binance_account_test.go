package binance

// SOV-02a: the adapter holds the API key, so the adapter is the only process that can
// ask Binance whose account it is. It asks over the SAME signed REST client it already
// reconciles balances with — GET /api/v3/account, which carries the account's uid.

import (
	"context"
	"testing"
	"time"
)

func restOverFake(f *fakeBinance) *binanceREST {
	return newBinanceREST(restConfig{
		BaseURL:   f.srv.URL,
		APIKey:    "k",
		APISecret: "s",
		Bucket:    newWeightBucket(1200, time.Minute, nil),
	})
}

// TestExchangeAccountIDReadsTheUID: Binance answers with the uid its key belongs to.
func TestExchangeAccountIDReadsTheUID(t *testing.T) {
	f := newFakeBinance(t)
	f.accountBody = `{"uid":12345678,"balances":[{"asset":"BTC","free":"1.0","locked":"0"}]}`

	got, err := restOverFake(f).ExchangeAccountID(context.Background())
	if err != nil {
		t.Fatalf("ExchangeAccountID: %v", err)
	}
	if got != "12345678" {
		t.Errorf("uid = %q, want 12345678", got)
	}
}

// TestExchangeAccountIDRefusesAnAnswerWithNoUID: an account response that carries no
// uid has not told us whose key this is. Returning "" would be read upstream as a
// verified empty account and would compare equal to nothing — so it must be an error,
// not a zero value. A silent "" here is how an unproven adapter would boot looking
// proven.
func TestExchangeAccountIDRefusesAnAnswerWithNoUID(t *testing.T) {
	f := newFakeBinance(t)
	f.accountBody = `{"balances":[{"asset":"BTC","free":"1.0","locked":"0"}]}`

	if _, err := restOverFake(f).ExchangeAccountID(context.Background()); err == nil {
		t.Error("ExchangeAccountID accepted an account response carrying no uid")
	}
}

// TestExchangeAccountIDSurfacesAnAPIError: a rejected key (bad signature, wrong IP
// allowlist) must not be reported as "no account".
func TestExchangeAccountIDSurfacesAnAPIError(t *testing.T) {
	f := newFakeBinance(t)
	f.accountBody = `{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`

	if _, err := restOverFake(f).ExchangeAccountID(context.Background()); err == nil {
		t.Error("ExchangeAccountID swallowed an API error")
	}
}
