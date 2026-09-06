package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigReadsSensitiveValuesOnlyThroughSecretBoundary(t *testing.T) {
	now := testNow()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	dsnFile := filepath.Join(dir, "dsn")
	signingFile := filepath.Join(dir, "signing")
	drFile := filepath.Join(dir, "dr.json")
	mustWriteTestFile(t, tokenFile, []byte("mounted-token\n"))
	mustWriteTestFile(t, dsnFile, []byte("postgres://readonly@ledger/kanz\n"))
	mustWriteTestFile(t, signingFile, []byte("mounted-signing-key\n"))
	drRaw, _ := json.Marshal(validConfig(now).dr)
	mustWriteTestFile(t, drFile, drRaw)

	values := map[string]string{
		"CAPITALPATH_POSTURE": "okx-demo", "CAPITALPATH_GATEWAY_URL": "https://gateway.test.kanz.example",
		"CAPITALPATH_ENVIRONMENT": "testnet-dr", "CAPITALPATH_TENANT": "certification",
		"CAPITALPATH_PORTFOLIO": "PF-CERT", "CAPITALPATH_INSTRUMENT": "BTC-USDT",
		"CAPITALPATH_VENUE": "XOKX", "CAPITALPATH_VENUE_ACCOUNT": "okx-demo-cert",
		"CAPITALPATH_QUANTITY": "0.0001", "CAPITALPATH_LIMIT_PRICE": "50000", "CAPITALPATH_MAX_NOTIONAL": "10",
		"CAPITALPATH_NATS_URL": "nats://nats.test:4222", "CAPITALPATH_SPIFFE_SOCKET": "unix:///run/spiffe/socket",
		"CAPITALPATH_GATEWAY_TOKEN_FILE": tokenFile, "CAPITALPATH_LEDGER_DSN_FILE": dsnFile,
		"CAPITALPATH_GATEWAY_SIGNING_SECRET_FILE": signingFile,
		"CAPITALPATH_DR_ATTESTATION_FILE":         drFile, "CAPITALPATH_TIMEOUT": "3m",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, timeout, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.token != "mounted-token" || cfg.gatewaySigningKey != "mounted-signing-key" || cfg.ledgerDSN != "postgres://readonly@ledger/kanz" || timeout != 3*time.Minute {
		t.Fatalf("loaded token/dsn/timeout = %q/%q/%s", cfg.token, cfg.ledgerDSN, timeout)
	}
}

func TestLoadConfigRefusesDeclaredButUnreadableSecretMount(t *testing.T) {
	t.Setenv("CAPITALPATH_GATEWAY_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig accepted an unreadable declared secret mount")
	}
}

func mustWriteTestFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
