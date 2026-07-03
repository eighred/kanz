package config

import (
	"os"
	"path/filepath"
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

// secret() is the SEC-01d seam that keeps the DSN out of plaintext env: a
// CSI-mounted file must win over a plaintext env var, with a clean fall-back.
func TestSecret(t *testing.T) {
	const key = "RISK_ENGINE_TEST_SECRET"

	t.Run("file wins over plaintext env and is trimmed", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "dsn")
		if err := os.WriteFile(p, []byte("  postgres://from-file  \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, "postgres://from-env")
		t.Setenv(key+"_FILE", p)
		if got := secret(key); got != "postgres://from-file" {
			t.Fatalf("got %q, want the trimmed file value", got)
		}
	})

	t.Run("falls back to env when no file is set", func(t *testing.T) {
		t.Setenv(key, "postgres://from-env")
		os.Unsetenv(key + "_FILE")
		if got := secret(key); got != "postgres://from-env" {
			t.Fatalf("got %q, want the env value", got)
		}
	})

	t.Run("unreadable file path falls through to env", func(t *testing.T) {
		t.Setenv(key, "postgres://from-env")
		t.Setenv(key+"_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
		if got := secret(key); got != "postgres://from-env" {
			t.Fatalf("got %q, want the env fallback", got)
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
