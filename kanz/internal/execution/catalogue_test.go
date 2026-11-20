package execution

import "testing"

func TestCheckExchangeCatalogueRequiresMatchingLiveExchangeMetadata(t *testing.T) {
	configured := StaticSymbolMap{"BTC-USDT": "BTCUSDT"}.Instruments()
	valid := []ExchangeInstrument{{Symbol: "BTCUSDT", Base: "BTC", Quote: "USDT", State: "TRADING"}}
	if err := CheckExchangeCatalogue(configured, valid, "TRADING"); err != nil {
		t.Fatalf("matching catalogue: %v", err)
	}
	for name, changed := range map[string][]ExchangeInstrument{
		"missing":    nil,
		"inactive":   {{Symbol: "BTCUSDT", Base: "BTC", Quote: "USDT", State: "BREAK"}},
		"quote":      {{Symbol: "BTCUSDT", Base: "BTC", Quote: "USD", State: "TRADING"}},
		"base":       {{Symbol: "BTCUSDT", Base: "WBTC", Quote: "USDT", State: "TRADING"}},
		"incomplete": {{Symbol: "BTCUSDT", Base: "BTC", State: "TRADING"}},
		"duplicate":  {valid[0], valid[0]},
	} {
		t.Run(name, func(t *testing.T) {
			if err := CheckExchangeCatalogue(configured, changed, "TRADING"); err == nil {
				t.Fatal("accepted catalogue without exchange proof of the configured pair")
			}
		})
	}
	if err := CheckExchangeCatalogue(StaticSymbolMap{"BTC-USD": "BTCUSDT"}.Instruments(), valid, "TRADING"); err == nil {
		t.Fatal("accepted instrument identity that records the wrong settlement asset")
	}
	if err := CheckExchangeCatalogue(StaticSymbolMap{"ETH-USDT": "BTCUSDT"}.Instruments(), valid, "TRADING"); err == nil {
		t.Fatal("accepted instrument identity that records the wrong base asset")
	}
}
