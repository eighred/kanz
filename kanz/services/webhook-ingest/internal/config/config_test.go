package config

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
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
  "max_quantity": "12.5",
  "max_leverage": "10"
}`

// loadWith writes a bootstrap file and runs Load against it.
func loadWith(t *testing.T, bootstrap string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(path, []byte(bootstrap), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEBHOOK_INGEST_CONFIG", path)
	return Load()
}

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
	// max_quantity is an exact rational in the RESOLVED base-asset unit (#240).
	if !cfg.MaxQuantity.IsSet() || cfg.MaxQuantity.Rat().Cmp(big.NewRat(25, 2)) != 0 {
		t.Errorf("max_quantity = %s, want 25/2", cfg.MaxQuantity.RatString())
	}
	if len(cfg.Allowlist) != 2 {
		t.Errorf("allowlist = %d entries, want 2", len(cfg.Allowlist))
	}
}

// A bootstrap whose weights are whole percents used to load clean and then scale
// every order the fund placed by 100. The service now refuses to start (#240).
func TestLoad_RefusesWeightsThatAreNotASplit(t *testing.T) {
	for _, tc := range []struct{ name, funds, want string }{
		{
			"whole percents",
			`{"fund-alpha": [{"venue":"BINANCE","weight":"60"},{"venue":"OKX","weight":"40"}]}`,
			"sum to 100",
		},
		{
			"duplicated leg",
			`{"fund-alpha": [{"venue":"BINANCE","weight":"0.6"},{"venue":"BINANCE","weight":"0.6"}]}`,
			"twice",
		},
		{
			"short of 1",
			`{"fund-alpha": [{"venue":"BINANCE","weight":"0.5"}]}`,
			"sum to 1/2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWith(t, `{"strategies":{},"symbols":{},"prices":{},"equity":{},"funds":`+tc.funds+`}`)
			if !errors.Is(err, ingest.ErrBadAllocation) {
				t.Fatalf("Load = %v, want ErrBadAllocation", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// The retired key must be an ERROR, not an ignored field. json.Unmarshal drops
// unknown keys, so a silent rename would take a deployment that HAD a bound and
// leave it with none — worse than the defect being fixed.
func TestLoad_RefusesTheRetiredMaxSizeKey(t *testing.T) {
	_, err := loadWith(t, `{"strategies":{},"symbols":{},"prices":{},"equity":{},"funds":{},`+
		`"max_size":"1000000"}`)
	if err == nil {
		t.Fatal("a bootstrap carrying the retired `max_size` loaded clean — its bound is silently gone")
	}
	for _, want := range []string{"max_size", "max_quantity", "1000000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q — the operator cannot tell what to change", err, want)
		}
	}
}

func TestLoad_RefusesANonPositiveMaxQuantity(t *testing.T) {
	_, err := loadWith(t, `{"strategies":{},"symbols":{},"prices":{},"equity":{},"funds":{},`+
		`"max_quantity":"0"}`)
	if err == nil {
		t.Fatal("max_quantity of 0 loaded clean — it would refuse every order while looking configured")
	}
}

func TestLoad_MissingConfigFails(t *testing.T) {
	t.Setenv("WEBHOOK_INGEST_CONFIG", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error when WEBHOOK_INGEST_CONFIG is unset")
	}
}

// THE FRESHNESS BOUND SHIPS ARMED (#416).
//
// A safety control that defaults to disabled and waits for someone to set it is
// the shape of every "we had the fix but it was not turned on" incident. This
// pins the default so it cannot drift back to zero unnoticed.
func TestMaxSignalAgeDefaultsToARealBound(t *testing.T) {
	t.Setenv("WEBHOOK_INGEST_MAX_SIGNAL_AGE", "")

	cfg, err := loadWith(t, bootstrapJSON)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxSignalAge <= 0 {
		t.Fatal("MaxSignalAge defaults to unbounded. A thirty-minute-old alert then executes at " +
			"full size on a fresh deployment, which is the defect #416 exists to close.")
	}
	if cfg.MaxSignalAge > 5*time.Minute {
		t.Errorf("MaxSignalAge defaults to %s — a bound that loose readmits the delayed-retry case "+
			"it exists to refuse", cfg.MaxSignalAge)
	}
}

// It can still be turned OFF, and that has to be an explicit act rather than an
// omission.
func TestMaxSignalAgeCanBeDisabledExplicitly(t *testing.T) {
	t.Setenv("WEBHOOK_INGEST_MAX_SIGNAL_AGE", "0")

	cfg, err := loadWith(t, bootstrapJSON)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxSignalAge != 0 {
		t.Errorf("MaxSignalAge = %s with an explicit 0", cfg.MaxSignalAge)
	}
}

// Requiring a timestamp is OFF by default: armed, it takes every strategy whose
// alert template omits the field offline, and the field is still advisory in the
// webhook contract.
func TestRequireSignalTSDefaultsOff(t *testing.T) {
	t.Setenv("WEBHOOK_INGEST_REQUIRE_SIGNAL_TS", "")

	cfg, err := loadWith(t, bootstrapJSON)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RequireSignalTS {
		t.Error("RequireSignalTS defaults ON — every strategy that omits ts stops trading on upgrade")
	}
}
