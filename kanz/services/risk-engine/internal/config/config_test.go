package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parseDuration is the PARITY-02f deploy-time snapshot-cadence knob: a valid Go
// duration tunes the cadence, anything else falls back to 0 (the snapshotter's
// DefaultSnapshotInterval) — a bad env value must never crash the boot.
func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"30s":     30 * time.Second,
		"2m":      2 * time.Minute,
		"":        0,
		"garbage": 0,
		"-5s":     -5 * time.Second,
	}
	for in, want := range cases {
		if got := parseDuration(in); got != want {
			t.Fatalf("parseDuration(%q) = %v, want %v", in, got, want)
		}
	}
}

// DSN resolution is the SEC-01d seam that keeps the DSN out of plaintext env: a
// CSI-mounted file must win over a plaintext env var. This exercises it through
// Load rather than through a local helper, because there no longer is one —
// pkg/secret owns the mechanism and tests it directly; what belongs here is that
// the risk-engine's own DSN is wired to it.
func TestLoadDatabaseURL(t *testing.T) {
	const key = "RISK_ENGINE_DATABASE_URL"

	t.Run("file wins over plaintext env and is trimmed", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "dsn")
		if err := os.WriteFile(p, []byte("  postgres://from-file  \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, "postgres://from-env")
		t.Setenv(key+"_FILE", p)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DatabaseURL != "postgres://from-file" {
			t.Fatalf("got %q, want the trimmed file value", cfg.DatabaseURL)
		}
	})

	t.Run("falls back to env when no file is set", func(t *testing.T) {
		t.Setenv(key, "postgres://from-env")
		os.Unsetenv(key + "_FILE")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DatabaseURL != "postgres://from-env" {
			t.Fatalf("got %q, want the env value", cfg.DatabaseURL)
		}
	})

	// This case used to assert the OPPOSITE — that an unreadable mount falls
	// through to the plaintext env — and that assertion is why the defect
	// survived: it pinned the fall-through in place as intended behaviour. An
	// empty DatabaseURL puts the engine in memory-only state with no bootstrap
	// restore, which is a legitimate configuration, so nothing downstream could
	// ever have told a failed Vault mount from a deliberate one.
	t.Run("declared but unreadable mount refuses to load", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		t.Setenv(key, "postgres://from-env")
		t.Setenv(key+"_FILE", missing)
		cfg, err := Load()
		if err == nil {
			t.Fatalf("Load returned DatabaseURL=%q and no error for an unreadable mount", cfg.DatabaseURL)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error %q must name the unreadable path", err)
		}
	})
}

// The WIRE-01c calibration knobs default safely (scheduler off, nightly 24h,
// market subjects = MARKET wildcard) and parse their overrides.
func TestLoadCalibration(t *testing.T) {
	t.Run("defaults: scheduler off", func(t *testing.T) {
		for _, k := range []string{
			"RISK_ENGINE_CALIBRATION_INTERVAL", "RISK_ENGINE_CALIBRATION_NIGHTLY_INTERVAL",
			"RISK_ENGINE_CALIBRATION_RATES", "RISK_ENGINE_MARKET_SUBJECTS",
		} {
			t.Setenv(k, "")
		}
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.CalibrationInterval != 0 {
			t.Fatalf("intraday cadence = %v, want 0 (disabled)", cfg.CalibrationInterval)
		}
		if cfg.CalibrationNightly != DefaultCalibrationNightly {
			t.Fatalf("nightly cadence = %v, want default %v", cfg.CalibrationNightly, DefaultCalibrationNightly)
		}
		if cfg.CalibrationRates != "" {
			t.Fatalf("rates = %q, want empty", cfg.CalibrationRates)
		}
		if len(cfg.MarketSubjects) != 1 || cfg.MarketSubjects[0] != "market.>" {
			t.Fatalf("market subjects = %v, want [market.>]", cfg.MarketSubjects)
		}
	})

	t.Run("overrides parse", func(t *testing.T) {
		t.Setenv("RISK_ENGINE_CALIBRATION_INTERVAL", "5m")
		t.Setenv("RISK_ENGINE_CALIBRATION_NIGHTLY_INTERVAL", "12h")
		t.Setenv("RISK_ENGINE_CALIBRATION_RATES", "USD-DEP-3M:USD:deposit:0.25")
		t.Setenv("RISK_ENGINE_MARKET_SUBJECTS", "market.rate.quote, market.rate.swap")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.CalibrationInterval != 5*time.Minute {
			t.Fatalf("intraday cadence = %v, want 5m", cfg.CalibrationInterval)
		}
		if cfg.CalibrationNightly != 12*time.Hour {
			t.Fatalf("nightly cadence = %v, want 12h", cfg.CalibrationNightly)
		}
		if len(cfg.MarketSubjects) != 2 {
			t.Fatalf("market subjects = %v, want two", cfg.MarketSubjects)
		}
	})
}
