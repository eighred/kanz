package config

// secret() resolves a <K>_FILE / <K> pair (SEC-01d). Setting <K>_FILE is the
// deployment's declaration that a durable secret mount was intended — see
// venue-binance-deploy.yaml's VENUE_BINANCE_DATABASE_URL_FILE. Before this test
// existed, a declared-but-unreadable file (bad mount, wrong permissions, typo'd
// path) fell straight through to the plaintext env and then to "", which for
// DatabaseURL is indistinguishable from "no durable store was ever configured"
// — the adapter would boot on an in-memory order view with nothing logged and
// readyz green. These tests pin the fail-fast behaviour that closes that gap.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnreadableDatabaseURLFileFailsFast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist")
	t.Setenv("VENUE_BINANCE_DATABASE_URL_FILE", path)

	_, err := Load()
	if err == nil {
		t.Fatalf("Load() with an unreadable VENUE_BINANCE_DATABASE_URL_FILE = nil error, want an error — " +
			"a mounted-but-unreadable secret must not silently fall through to \"\" (indistinguishable from " +
			"never having been configured)")
	}
	if !strings.Contains(err.Error(), "VENUE_BINANCE_DATABASE_URL_FILE") {
		t.Fatalf("error = %q, want it to name VENUE_BINANCE_DATABASE_URL_FILE", err.Error())
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %q, want it to name the path %q", err.Error(), path)
	}
}

func TestDatabaseURLFromSecretFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "database-url")
	// Trailing newline is what a Vault/CSI file mount actually writes.
	if err := os.WriteFile(path, []byte("postgres://u:p@db:5432/venue\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("VENUE_BINANCE_DATABASE_URL_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DatabaseURL != "postgres://u:p@db:5432/venue" {
		t.Fatalf("DSN from file = %q, want the trimmed DSN", cfg.DatabaseURL)
	}
}

func TestDatabaseURLFileWinsOverPlaintextEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "database-url")
	if err := os.WriteFile(path, []byte("postgres://from-file/venue"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("VENUE_BINANCE_DATABASE_URL_FILE", path)
	t.Setenv("VENUE_BINANCE_DATABASE_URL", "postgres://from-env/venue")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// SEC-01d: the CSI file mount is the source of truth for a secret.
	if cfg.DatabaseURL != "postgres://from-file/venue" {
		t.Fatalf("DSN = %q, want the file mount to win over the plaintext env", cfg.DatabaseURL)
	}
}

func TestNoDatabaseConfiguredLeavesDSNEmptyWithoutError(t *testing.T) {
	// The dev/rig default: neither _FILE nor the plaintext env set ⇒ empty DSN,
	// no error. openView's caller is responsible for warning about the
	// in-memory consequence; Load() itself must not refuse to start.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DatabaseURL != "" {
		t.Fatalf("DSN = %q, want empty when neither VENUE_BINANCE_DATABASE_URL nor _FILE is set", cfg.DatabaseURL)
	}
}

// secret() backs APIKey/APISecret too — verify the fail-fast and precedence
// rules hold generically, not just for the DSN.
func TestSecretTableDriven(t *testing.T) {
	t.Run("file set and readable wins over plaintext env", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "v")
		if err := os.WriteFile(path, []byte(" from-file \n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Setenv("TEST_SECRET_K_FILE", path)
		t.Setenv("TEST_SECRET_K", "from-env")

		got, err := secret("TEST_SECRET_K")
		if err != nil {
			t.Fatalf("secret() error = %v, want nil", err)
		}
		if got != "from-file" {
			t.Fatalf("secret() = %q, want trimmed file contents %q", got, "from-file")
		}
	})

	t.Run("file set and unreadable is an error naming var and path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing")
		t.Setenv("TEST_SECRET_K_FILE", path)

		_, err := secret("TEST_SECRET_K")
		if err == nil {
			t.Fatalf("secret() error = nil, want an error for an unreadable declared file")
		}
		if !strings.Contains(err.Error(), "TEST_SECRET_K_FILE") || !strings.Contains(err.Error(), path) {
			t.Fatalf("error = %q, want it to name TEST_SECRET_K_FILE and %q", err.Error(), path)
		}
	})

	t.Run("file unset, env set uses env", func(t *testing.T) {
		t.Setenv("TEST_SECRET_K", "from-env")
		got, err := secret("TEST_SECRET_K")
		if err != nil {
			t.Fatalf("secret() error = %v, want nil", err)
		}
		if got != "from-env" {
			t.Fatalf("secret() = %q, want %q", got, "from-env")
		}
	})

	t.Run("file unset, env unset returns empty, no error", func(t *testing.T) {
		got, err := secret("TEST_SECRET_K")
		if err != nil {
			t.Fatalf("secret() error = %v, want nil", err)
		}
		if got != "" {
			t.Fatalf("secret() = %q, want empty", got)
		}
	})
}
