package exchangeauth_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

// Physical OKX demo read of GET /api/v5/account/config — the call that backs
// account_verified (#417, #408 control 1). Gated on TEST_OKX_TESTNET=1 plus
// OKX_TESTNET_KEY / _SECRET / _PASSPHRASE, exactly as
// services/venue-okx's TestOKXTestnet_SignedRoundTrip is.
//
// GET ONLY, AND THAT IS STRUCTURAL RATHER THAN A PROMISE: exchangeauth.AccountID
// is a read, and this file imports nothing that can place an order. Order
// placement is exercised against the fake server, and the sibling harness's
// header says the same thing for the same reason.
//
// # WHY THIS IS WORTH A LIVE CALL WHEN THE FAKE-SERVER TESTS PASS
//
// The fake server answers what we told it to answer. What it cannot tell us is
// whether OKX returns a uid for THIS key at all — and the whole of
// account_verified rests on that. `venues.go` treats an unverified account as a
// named, counted warning and refuses only under OMS_REQUIRE_VERIFIED_ACCOUNT, so
// a deployment where this call quietly returns nothing looks exactly like a
// deployment nobody configured. The estate has never once run it for real.
//
// # THE DEMO/LIVE AXIS IS THE POINT, NOT AN ASIDE
//
// Demo and live are SEPARATE OKX accounts with separate keys. Asking live "which
// account is this?" with a demo key does not merely fail — before the
// x-simulated-trading header existed it was the only thing this call could do,
// and a verified-looking uid from the wrong environment is worse than a refusal.
// So this runs in OKXDemo mode and a uid returned here is a uid from the book the
// credential actually belongs to.
func TestOKXDemo_AccountConfigCarriesAUID(t *testing.T) {
	if os.Getenv("TEST_OKX_TESTNET") == "" {
		t.Skip("set TEST_OKX_TESTNET=1 + OKX_TESTNET_KEY/_SECRET/_PASSPHRASE to run the live demo account-config read")
	}
	cred := exchangeauth.Credential{
		APIKey:     os.Getenv("OKX_TESTNET_KEY"),
		APISecret:  os.Getenv("OKX_TESTNET_SECRET"),
		Passphrase: os.Getenv("OKX_TESTNET_PASSPHRASE"),
	}
	if cred.APIKey == "" || cred.APISecret == "" || cred.Passphrase == "" {
		t.Fatal("OKX_TESTNET_KEY, OKX_TESTNET_SECRET and OKX_TESTNET_PASSPHRASE are required")
	}

	base := os.Getenv("OKX_TESTNET_BASE")
	if base == "" {
		base = "https://www.okx.com"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	uid, err := exchangeauth.AccountID(ctx, "okx", cred, exchangeauth.Options{
		BaseURL:    base,
		OKXTrading: exchangeauth.OKXDemo,
	})
	if err != nil {
		t.Fatalf("account-config read failed: %v\n\n"+
			"This is the call account_verified rests on. Failing here means the OMS cannot prove "+
			"an adapter's credential belongs to the collateral pool it names — and an exchange "+
			"liquidates per account.", err)
	}
	if uid == "" {
		t.Fatal("the exchange returned an EMPTY uid with no error — an empty id compares equal to " +
			"nothing and would ride upstream as a verified account")
	}
	// NOT LOGGED. The uid identifies the account behind a live credential; the
	// assertion is that one exists, and printing it would put an account
	// identifier in CI output for no verification gain.
	t.Logf("account/config returned a non-empty uid (%d chars) in OKXDemo mode", len(uid))
}
