package master

import "testing"

func TestPairSurvivorshipNeverCombinesVendorLegs(t *testing.T) {
	tests := []struct {
		name                string
		rows                []VendorRecord
		base, quote, vendor string
	}{
		{"complete assertion", []VendorRecord{
			{Vendor: "first", AssetClass: "CRYPTO", BaseAsset: "BTC", QuoteAsset: "USDT"},
			{Vendor: "second", Priority: 1, AssetClass: "CRYPTO", BaseAsset: "BTC", QuoteAsset: "USD"},
		}, "BTC", "USDT", "first"},
		{"incomplete assertions remain unknown", []VendorRecord{
			{Vendor: "first", AssetClass: "CRYPTO", BaseAsset: "BTC"},
			{Vendor: "second", Priority: 1, AssetClass: "CRYPTO", QuoteAsset: "USD"},
		}, "", "", ""},
		{"complete lower priority assertion", []VendorRecord{
			{Vendor: "first", AssetClass: "FX", BaseAsset: "GBP"},
			{Vendor: "second", Priority: 1, AssetClass: "FX", BaseAsset: "EUR", QuoteAsset: "USD"},
		}, "EUR", "USD", "second"},
		{"conflicting class is not a pair source", []VendorRecord{
			{Vendor: "first", AssetClass: "CRYPTO"},
			{Vendor: "second", Priority: 1, AssetClass: "FX", BaseAsset: "EUR", QuoteAsset: "USD"},
		}, "", "", ""},
		{"equity cannot acquire pair semantics", []VendorRecord{
			{Vendor: "first", AssetClass: "EQUITY", BaseAsset: "BTC", QuoteAsset: "USD"},
		}, "", "", ""},
		{"identical legs are not a pair", []VendorRecord{
			{Vendor: "first", AssetClass: "CRYPTO", BaseAsset: "USD", QuoteAsset: "USD"},
		}, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := Resolve(tt.rows)
			if got.BaseAsset != tt.base || got.QuoteAsset != tt.quote || got.Provenance["base_asset"] != tt.vendor || got.Provenance["quote_asset"] != tt.vendor {
				t.Fatalf("fabricated or lost pair: %+v", got)
			}
		})
	}
}
