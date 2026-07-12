package okx

// Physical OKX demo/testnet round-trip (M4 parity mandate). Gated on
// TEST_OKX_TESTNET=1 plus OKX_TESTNET_KEY / _SECRET / _PASSPHRASE — it needs
// live network egress + demo-trading keys, so it skips by default (like the
// Binance testnet test). It verifies the signed OKX v5 REST handshake end to
// end: a public ticker and a signed account-balance read. It deliberately does
// NOT place a live order (read-only verification); order placement is exercised
// against the fake server hermetically.
//
// Run:
//   TEST_OKX_TESTNET=1 OKX_TESTNET_KEY=... OKX_TESTNET_SECRET=... OKX_TESTNET_PASSPHRASE=... \
//     go test -tags okx -run TestOKXTestnet_SignedRoundTrip \
//     ./services/oms/internal/execution/

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestOKXTestnet_SignedRoundTrip(t *testing.T) {
	if os.Getenv("TEST_OKX_TESTNET") == "" {
		t.Skip("set TEST_OKX_TESTNET=1 + OKX_TESTNET_KEY/_SECRET/_PASSPHRASE to run the live demo round-trip")
	}
	key, secret, pass := os.Getenv("OKX_TESTNET_KEY"), os.Getenv("OKX_TESTNET_SECRET"), os.Getenv("OKX_TESTNET_PASSPHRASE")
	if key == "" || secret == "" || pass == "" {
		t.Fatal("OKX_TESTNET_KEY, OKX_TESTNET_SECRET and OKX_TESTNET_PASSPHRASE are required")
	}
	base := envOrDefault("OKX_TESTNET_BASE", "https://www.okx.com")
	rest := newOKXREST(okxRestConfig{
		BaseURL: base, APIKey: key, APISecret: secret, Passphrase: pass,
		Bucket:     NewWeightBucket(60, 2*time.Second, nil),
		HTTPClient: NewExchangeHTTPClient(0),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Public ticker (unsigned).
	if px, err := rest.tickerPrice(ctx, "BTC-USDT"); err != nil || px == "" {
		t.Fatalf("ticker round-trip failed: px=%q err=%v", px, err)
	}
	// Signed account read — proves the base64-HMAC + passphrase handshake.
	bals, err := rest.balances(ctx)
	if err != nil {
		t.Fatalf("signed balance round-trip failed: %v", err)
	}
	if bals == nil {
		t.Fatal("balance call returned a nil map")
	}
}

func envOrDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
