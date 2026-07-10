package config

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"
)

const bootstrapJSON = `{
  "strategies": {"momentum": "s3cr3t"},
  "symbols":    {"BINANCE:BTCUSDT": "BTC-USD"},
  "prices":     {"BTC-USD": "50000.5"},
  "equity":     {"fund-alpha": "1000000"},
  "funds":      {"fund-alpha": [
    {"venue": "BINANCE", "weight": "0.6"},
    {"venue": "OKX", "weight": "0.4"}
  ]},
  "max_leverage": "10"
}`

func TestLoad_BootstrapParsesExactDecimals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(path, []byte(bootstrapJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEBHOOK_INGEST_CONFIG", path)
	t.Setenv("WEBHOOK_INGEST_IP_ALLOWLIST", "203.0.113.0/24, 198.51.100.7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s, ok := cfg.Secrets.SecretFor("momentum"); !ok || s != "s3cr3t" {
		t.Errorf("secret = %q ok=%v", s, ok)
	}
	if inst, ok := cfg.Symbols.Resolve("BINANCE:BTCUSDT"); !ok || inst != "BTC-USD" {
		t.Errorf("symbol resolve = %q ok=%v", inst, ok)
	}
	// Price parsed as an exact rational (50000.5 = 100001/2), no float.
	if got := cfg.Prices["BTC-USD"]; got == nil || got.Cmp(big.NewRat(100001, 2)) != 0 {
		t.Errorf("price = %v, want 100001/2", got)
	}
	legs, err := cfg.Alloc.VenuesFor("fund-alpha")
	if err != nil || len(legs) != 2 {
		t.Fatalf("alloc = %v (err %v)", legs, err)
	}
	total := new(big.Rat)
	for _, l := range legs {
		total.Add(total, l.Weight)
	}
	if total.Cmp(big.NewRat(1, 1)) != 0 {
		t.Errorf("weights sum = %s, want 1", total.RatString())
	}
	if cfg.MaxLeverage == nil || cfg.MaxLeverage.Cmp(big.NewRat(10, 1)) != 0 {
		t.Errorf("max_leverage = %v, want 10", cfg.MaxLeverage)
	}
	if len(cfg.Allowlist) != 2 {
		t.Errorf("allowlist = %d entries, want 2", len(cfg.Allowlist))
	}
}

func TestLoad_MissingConfigFails(t *testing.T) {
	t.Setenv("WEBHOOK_INGEST_CONFIG", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error when WEBHOOK_INGEST_CONFIG is unset")
	}
}
