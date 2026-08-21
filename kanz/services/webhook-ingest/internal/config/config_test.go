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
  "strategies": {"momentum": {"secret": "s3cr3t", "funds": ["fund-alpha"]}},
  "symbols":    {"BINANCE:BTCUSDT": "BTC-USD"},
  "prices":     {"BTC-USD": "50000.5"},
  "equity":     {"fund-alpha": "1000000"},
  "funds":      {"fund-alpha": {"tenant": "acme", "venues": [
    {"venue": "BINANCE", "weight": "0.6"},
    {"venue": "OKX", "weight": "0.4"}
  ]}},
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
	// THE BINDING IS BUILT AND THE TENANT COMES FROM THE FILE (#632).
	if cfg.Authority == nil {
		t.Fatal("Load returned no FundAuthority — the pipeline would refuse to construct, and " +
			"before this existed the fund_id was the tenant")
	}
	tenant, ok := cfg.Authority.TenantForFund("momentum", "fund-alpha")
	if !ok || tenant != "acme" {
		t.Errorf("TenantForFund(momentum, fund-alpha) = %q ok=%v, want acme — the tenant is the "+
			"`tenant` key of the fund entry, never the fund id", tenant, ok)
	}
}

// AN UNBOUND PAIR DOES NOT RESOLVE, straight off a loaded config. The bootstrap
// declares momentum and fund-alpha; a second fund it is not bound to must be
// refused even though the file describes it fully.
func TestLoad_TheBindingIsDenyByDefault(t *testing.T) {
	cfg, err := loadWith(t, `{
	  "strategies": {"momentum": {"secret":"s3cr3t","funds":["fund-alpha"]}},
	  "symbols": {}, "prices": {}, "equity": {},
	  "funds": {
	    "fund-alpha":  {"tenant":"acme",  "venues":[{"venue":"BINANCE","weight":"1"}]},
	    "fund-victim": {"tenant":"globex","venues":[{"venue":"BINANCE","weight":"1"}]}
	  }
	}`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.Authority.TenantForFund("momentum", "fund-victim"); ok {
		t.Fatal("a strategy resolved a tenant for a fund it is not bound to. That is #632: the " +
			"HMAC authenticates the strategy and the fund_id came from the request body.")
	}
}

// THE RETIRED SHAPES MUST BE LOUD, not silently tolerated.
//
// A pre-#632 file parsed under a lenient reader would leave every strategy bound
// to NO funds and every fund with NO tenant — a security fix that reads as a
// total outage, with a Go type name as the only explanation. Each of these is a
// refusal that says what to write instead.
func TestLoad_RefusesTheRetiredBootstrapShapes(t *testing.T) {
	for _, tc := range []struct{ name, json, want string }{
		{
			"strategies as bare secrets",
			`{"strategies":{"momentum":"s3cr3t"},"symbols":{},"prices":{},"equity":{},` +
				`"funds":{"fund-alpha":{"tenant":"acme","venues":[{"venue":"BINANCE","weight":"1"}]}}}`,
			"RETIRED pre-#632 shape",
		},
		{
			"funds as bare venue arrays",
			`{"strategies":{"momentum":{"secret":"s3cr3t","funds":["fund-alpha"]}},"symbols":{},` +
				`"prices":{},"equity":{},"funds":{"fund-alpha":[{"venue":"BINANCE","weight":"1"}]}}`,
			"RETIRED pre-#632 shape",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWith(t, tc.json)
			if err == nil {
				t.Fatal("a pre-#632 bootstrap file loaded clean")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q — an operator cannot see what to write", err, tc.want)
			}
		})
	}
}

// AN UNCONFIGURED BINDING TABLE MUST NOT SERVE TRAFFIC (#632).
//
// This is the state the estate was actually in: `TenantOf` was assigned at no
// composition root, so every deployment ran on the identity default and looked
// healthy. There is no default now, and each of these exits 2 at startup rather
// than answering a request.
func TestLoad_RefusesAnUnconfiguredOrIncoherentBinding(t *testing.T) {
	const fundsOK = `"funds":{"fund-alpha":{"tenant":"acme","venues":[{"venue":"BINANCE","weight":"1"}]}}`
	for _, tc := range []struct{ name, json, want string }{
		{
			"no strategies declared",
			`{"strategies":{},"symbols":{},"prices":{},"equity":{},` + fundsOK + `}`,
			"no strategy is bound to a fund",
		},
		{
			"no funds declared",
			`{"strategies":{"momentum":{"secret":"s3cr3t","funds":["fund-alpha"]}},"symbols":{},` +
				`"prices":{},"equity":{},"funds":{}}`,
			"no fund is bound to a tenant",
		},
		{
			"a strategy bound to nothing",
			`{"strategies":{"momentum":{"secret":"s3cr3t","funds":[]}},"symbols":{},"prices":{},` +
				`"equity":{},` + fundsOK + `}`,
			"bound to no fund",
		},
		{
			"a strategy bound to a fund that does not exist",
			`{"strategies":{"momentum":{"secret":"s3cr3t","funds":["fund-typo"]}},"symbols":{},` +
				`"prices":{},"equity":{},` + fundsOK + `}`,
			"which no `funds` entry declares",
		},
		{
			"a fund with no tenant",
			`{"strategies":{"momentum":{"secret":"s3cr3t","funds":["fund-alpha"]}},"symbols":{},` +
				`"prices":{},"equity":{},"funds":{"fund-alpha":{"venues":[{"venue":"BINANCE","weight":"1"}]}}}`,
			"declares no tenant",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWith(t, tc.json)
			if !errors.Is(err, ingest.ErrNoFundAuthority) {
				t.Fatalf("Load = %v, want ErrNoFundAuthority — this deployment would have started "+
					"and served the internet with a binding it cannot answer from", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// A STRATEGY WITH NO SECRET IS A STRATEGY ANYONE CAN SIGN FOR: HMAC with an empty
// key is a value any sender can compute.
func TestLoad_RefusesAStrategyWithNoSecret(t *testing.T) {
	_, err := loadWith(t, `{"strategies":{"momentum":{"secret":"","funds":["fund-alpha"]}},`+
		`"symbols":{},"prices":{},"equity":{},`+
		`"funds":{"fund-alpha":{"tenant":"acme","venues":[{"venue":"BINANCE","weight":"1"}]}}}`)
	if err == nil {
		t.Fatal("a strategy with an empty HMAC secret loaded clean — any sender can produce a " +
			"signature under an empty key")
	}
	if !strings.Contains(err.Error(), "no secret") {
		t.Errorf("error %q does not say the secret is missing", err)
	}
}

// A bootstrap whose weights are whole percents used to load clean and then scale
// every order the fund placed by 100. The service now refuses to start (#240).
func TestLoad_RefusesWeightsThatAreNotASplit(t *testing.T) {
	for _, tc := range []struct{ name, funds, want string }{
		{
			"whole percents",
			`{"fund-alpha": {"tenant":"acme","venues":[{"venue":"BINANCE","weight":"60"},{"venue":"OKX","weight":"40"}]}}`,
			"sum to 100",
		},
		{
			"duplicated leg",
			`{"fund-alpha": {"tenant":"acme","venues":[{"venue":"BINANCE","weight":"0.6"},{"venue":"BINANCE","weight":"0.6"}]}}`,
			"twice",
		},
		{
			"short of 1",
			`{"fund-alpha": {"tenant":"acme","venues":[{"venue":"BINANCE","weight":"0.5"}]}}`,
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

// REQUIRING A TIMESTAMP IS ON BY DEFAULT (owner ruling, 2026-08-12).
//
// An alert with no `ts` has no age, so the freshness bound cannot judge it — and
// a rule a sender opts out of by omitting a field is not a rule. It defaults ON
// because no strategies were live when this landed: making `ts` mandatory BEFORE
// onboarding costs nothing, while tightening it later means choosing a day to
// break whichever senders never sent it.
func TestRequireSignalTSDefaultsOn(t *testing.T) {
	t.Setenv("WEBHOOK_INGEST_REQUIRE_SIGNAL_TS", "")

	cfg, err := loadWith(t, bootstrapJSON)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RequireSignalTS {
		t.Error("RequireSignalTS defaults OFF — an alert with no timestamp is admitted, and the " +
			"freshness bound has nothing to judge it against")
	}
}

// It can be relaxed EXPLICITLY, for onboarding a sender that cannot stamp its
// alerts yet.
func TestRequireSignalTSCanBeRelaxedExplicitly(t *testing.T) {
	t.Setenv("WEBHOOK_INGEST_REQUIRE_SIGNAL_TS", "false")

	cfg, err := loadWith(t, bootstrapJSON)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RequireSignalTS {
		t.Error("an explicit false did not relax the requirement")
	}
}

// A TYPO KEEPS THE STRICT DEFAULT. "ture" must not quietly relax a trading
// control — the same rule parseSignalAge applies to an unparseable duration.
func TestATypoKeepsTheStrictDefault(t *testing.T) {
	t.Setenv("WEBHOOK_INGEST_REQUIRE_SIGNAL_TS", "ture")

	cfg, err := loadWith(t, bootstrapJSON)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RequireSignalTS {
		t.Error("a typo relaxed the requirement — a misspelled value must never disable a control")
	}
}
