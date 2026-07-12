package config

// The DSN resolution path is load-bearing for correctness, not just for
// connectivity. infra/deploy/oms-deploy.yaml runs replicas: 2 and delivers the
// order-store DSN as a CSI secret FILE via OMS_DATABASE_URL_FILE. If that
// resolution silently yields "", the OMS falls back to the in-memory store whose
// admission gate is a process-local mutex — and two pods then both admit the
// same order and both route it to the venue. So the fallback must never be
// reachable by accident: these tests pin the file path the manifest depends on.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDatabaseURLFromSecretFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "database-url")
	// Trailing newline is what a Vault/CSI file mount actually writes.
	if err := os.WriteFile(path, []byte("postgres://u:p@db:5432/oms\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("OMS_DATABASE_URL_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DatabaseURL != "postgres://u:p@db:5432/oms" {
		t.Fatalf("DSN from file = %q, want the trimmed DSN (an empty value silently downgrades the OMS to the in-memory store)", cfg.DatabaseURL)
	}
}

func TestDatabaseURLFileWinsOverPlaintextEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "database-url")
	if err := os.WriteFile(path, []byte("postgres://from-file/oms"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("OMS_DATABASE_URL_FILE", path)
	t.Setenv("OMS_DATABASE_URL", "postgres://from-env/oms")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// SEC-01d: the CSI file mount is the source of truth for a secret.
	if cfg.DatabaseURL != "postgres://from-file/oms" {
		t.Fatalf("DSN = %q, want the file mount to win over the plaintext env", cfg.DatabaseURL)
	}
}

func TestNoDatabaseConfiguredLeavesDSNEmpty(t *testing.T) {
	// The single-replica / test default: no DSN ⇒ the in-memory store. Correct
	// ONLY at replicas: 1, which is why oms-deploy.yaml ties the two together.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DatabaseURL != "" {
		t.Fatalf("DSN = %q, want empty when neither OMS_DATABASE_URL nor _FILE is set", cfg.DatabaseURL)
	}
	if cfg.Tenant != "__system__" {
		t.Fatalf("Tenant = %q, want the __system__ default (the risk-engine convention)", cfg.Tenant)
	}
}
