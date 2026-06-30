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
