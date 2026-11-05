package config

import "testing"

func TestExternalDepthEnvironment(t *testing.T) {
	t.Setenv("MARKET_INGEST_INSTRUMENTS", "BTC-USD")
	t.Setenv("MARKET_INGEST_BINANCE_SYMBOLS", "BTC-USD=BTCUSDT")
	for _, tc := range []struct {
		raw             string
		valid, external bool
	}{{"", true, false}, {"BTC-USD", true, true}, {"BTC-USD,", false, true}, {"BTC-USD,BTC-USD", false, true}, {"ETH-USD", false, false}, {" BTC-USD", false, false}} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("MARKET_INGEST_BINANCE_EXTERNAL_DEPTH_INSTRUMENTS", tc.raw)
			cfg := Load()
			if (cfg.ValidateExternalDepth() == nil) != tc.valid {
				t.Fatal("validation mismatch")
			}
			if cfg.BinanceDepthExternal("BTC-USD") != tc.external {
				t.Fatal("assignment mismatch")
			}
		})
	}
}
