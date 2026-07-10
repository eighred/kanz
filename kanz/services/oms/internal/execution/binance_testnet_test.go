//go:build binance

package execution

// Physical Binance Testnet round-trip (M3 mandate). Gated on
// TEST_BINANCE_TESTNET=1 plus BINANCE_TESTNET_KEY / _SECRET — it needs live
// network egress + testnet keys, so it skips by default (like the NATS
// integration test). It verifies the signed REST handshake against
// testnet.binance.vision end to end: a signed /account read and a public
// ticker. It deliberately does NOT place a live order (read-only verification);
// order placement is exercised against the fake server hermetically.
//
// Run:
//   TEST_BINANCE_TESTNET=1 BINANCE_TESTNET_KEY=... BINANCE_TESTNET_SECRET=... \
//     go test -tags binance -run TestTestnet_SignedRoundTrip \
//     ./services/oms/internal/execution/

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestTestnet_SignedRoundTrip(t *testing.T) {
	if os.Getenv("TEST_BINANCE_TESTNET") == "" {
		t.Skip("set TEST_BINANCE_TESTNET=1 + BINANCE_TESTNET_KEY/_SECRET to run the live testnet round-trip")
	}
	key, secret := os.Getenv("BINANCE_TESTNET_KEY"), os.Getenv("BINANCE_TESTNET_SECRET")
	if key == "" || secret == "" {
		t.Fatal("BINANCE_TESTNET_KEY and BINANCE_TESTNET_SECRET are required")
	}
	base := "https://testnet.binance.vision"
	rest := newBinanceREST(restConfig{
		BaseURL: base, APIKey: key, APISecret: secret,
		Bucket:     newWeightBucket(1200, time.Minute, nil),
		HTTPClient: newExchangeHTTPClient(0),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Public ticker (unsigned).
	if px, err := rest.tickerPrice(ctx, "BTCUSDT"); err != nil || px == "" {
		t.Fatalf("ticker round-trip failed: px=%q err=%v", px, err)
	}
	// Signed account read — proves the HMAC handshake against the live venue.
	acct, err := rest.account(ctx)
	if err != nil {
		t.Fatalf("signed account round-trip failed: %v", err)
	}
	if len(acct.Balances) == 0 {
		t.Fatal("account returned no balances")
	}
}
