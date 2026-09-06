package main

import (
	"strings"
	"testing"
	"time"
)

func validConfig(now time.Time) config {
	return config{
		posture:           postureOKXDemo,
		gatewayURL:        "https://gateway.test.kanz.example",
		environment:       "testnet-dr",
		tenant:            "certification",
		portfolio:         "PF-CERT",
		instrument:        "BTC-USDT",
		venue:             "XOKX",
		account:           "okx-demo-cert",
		quantity:          "0.0001",
		limitPrice:        "50000",
		maxNotional:       "10",
		token:             "opaque-bearer",
		gatewaySigningKey: "gateway-signing-key",
		natsURL:           "nats://nats.kanz-messaging.svc:4222",
		spiffeSocket:      "unix:///run/spiffe/spire-agent.sock",
		ledgerDSN:         "postgres://readonly@books/kanz",
		dr: drAttestation{
			Environment:    "testnet-dr",
			ClusterUID:     "cluster-123",
			BackupID:       "backup-456",
			VerifiedAt:     now.Add(-time.Hour),
			RPOSeconds:     25,
			RTOSeconds:     420,
			RestoreProven:  true,
			AuditVerified:  true,
			EvidenceSHA256: strings.Repeat("a", 64),
		},
	}
}

func TestValidateConfigAcceptsCappedDemoOrderWithRecentDRProof(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := validateConfig(validConfig(now), now); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
}

func TestValidateConfigAcceptsCanonicalBinanceTestnetMIC(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cfg := validConfig(now)
	cfg.posture = postureBinanceTestnet
	cfg.venue = "XBIN"
	if err := validateConfig(cfg, now); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
}

func TestValidateConfigRefusesEveryProductionShapedTarget(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		edit func(*config)
		want string
	}{
		{"unknown posture", func(c *config) { c.posture = "okx-live" }, "posture"},
		{"plain http", func(c *config) { c.gatewayURL = "http://gateway.test" }, "https"},
		{"localhost", func(c *config) { c.gatewayURL = "https://localhost:8080" }, "loopback"},
		{"production host", func(c *config) { c.gatewayURL = "https://api.kanz.example" }, "test"},
		{"misleading test substring", func(c *config) { c.gatewayURL = "https://contest.evil.example" }, "test"},
		{"embedded credentials", func(c *config) { c.gatewayURL = "https://user:pass@gateway.test.example" }, "origin"},
		{"non-origin path", func(c *config) { c.gatewayURL = "https://gateway.test.example/proxy" }, "origin"},
		{"wrong venue", func(c *config) { c.venue = "XNAS" }, "venue"},
		{"oversize", func(c *config) { c.quantity = "0.001" }, "notional"},
		{"raised safety ceiling", func(c *config) { c.maxNotional = "10.000000000000000001" }, "hard ceiling"},
		{"stale DR", func(c *config) { c.dr.VerifiedAt = now.Add(-25 * time.Hour) }, "stale"},
		{"wrong DR environment", func(c *config) { c.dr.Environment = "another" }, "environment"},
		{"restore unproven", func(c *config) { c.dr.RestoreProven = false }, "restore"},
		{"audit unproven", func(c *config) { c.dr.AuditVerified = false }, "audit"},
		{"RPO missed", func(c *config) { c.dr.RPOSeconds = 61 }, "RPO"},
		{"RTO missed", func(c *config) { c.dr.RTOSeconds = 901 }, "RTO"},
		{"missing evidence digest", func(c *config) { c.dr.EvidenceSHA256 = "" }, "digest"},
		{"missing workload identity", func(c *config) { c.spiffeSocket = "" }, "SPIFFE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(now)
			tc.edit(&cfg)
			err := validateConfig(cfg, now)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want refusal containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateConfigUsesExactDecimalNotional(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cfg := validConfig(now)
	cfg.quantity = "0.00020000000000000001"
	cfg.limitPrice = "50000"
	cfg.maxNotional = "10"
	if err := validateConfig(cfg, now); err == nil || !strings.Contains(err.Error(), "notional") {
		t.Fatalf("err=%v, want exact-decimal notional refusal", err)
	}
}
