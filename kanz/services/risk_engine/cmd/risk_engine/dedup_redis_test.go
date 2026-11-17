//go:build redis

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/risk_engine/internal/config"
)

func TestSharedDedupRefusesMalformedEndpointWithoutDisclosure(t *testing.T) {
	for _, endpoint := range []string{
		"https://dummy-user:dummy-password@127.0.0.1:6379",
		"redis://dummy-user:dummy-password@127.0.0.1:6379?unknown=dummy-marker",
		"redis://dummy-user:dummy-password@127.0.0.1:not-a-port",
		"unix:///dummy-password.sock",
		"redis://dummy-user:dummy-password@127.0.0.1:6379?read_timeout=1h",
		"redis://dummy-user:dummy-password@127.0.0.1:6379?max_retries=100",
	} {
		if _, err := loadDedupConfig(config.Config{RedisURL: endpoint}); err == nil {
			t.Fatal("malformed explicit configuration silently selected per-instance dedup")
		} else if strings.Contains(err.Error(), "dummy-") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatal("configuration diagnostic disclosed endpoint data")
		}
	}
}

func TestSharedDedupCoordinationBudgetsArePolicyOwned(t *testing.T) {
	cfg, err := loadDedupConfig(config.Config{RedisURL: "rediss://dummy-user:dummy-password@127.0.0.1:6379/3"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.opts.DB != 3 || cfg.opts.TLSConfig == nil || cfg.opts.Username != "dummy-user" {
		t.Fatal("endpoint/auth/database selection was not retained")
	}
	if cfg.opts.DialTimeout != time.Second || cfg.opts.ReadTimeout != time.Second ||
		cfg.opts.WriteTimeout != time.Second || cfg.opts.PoolTimeout != time.Second ||
		cfg.opts.MaxRetries != -1 || !cfg.opts.ContextTimeoutEnabled {
		t.Fatal("coordination lost its trusted per-operation budgets or disabled context timeouts")
	}
}

func TestSharedDedupSelectionDoesNotClaimBackendReadiness(t *testing.T) {
	cfg, err := loadDedupConfig(config.Config{RedisURL: "rediss://dummy-user:dummy-password@127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	var logs bytes.Buffer
	deduper, closer := newDeduper(cfg, slog.New(slog.NewTextHandler(&logs, nil)), reg)
	if deduper == nil || closer == nil {
		t.Fatal("valid shared configuration was silently replaced by per-instance dedup")
	}
	t.Cleanup(func() { _ = closer.Close() })
	assertDedupMetric(t, reg, "kanz_risk_shared_dedup_configured", "", 1)
	if !strings.Contains(logs.String(), "backend_verified=false") || strings.Contains(logs.String(), "dummy-") {
		t.Fatal("selection must distinguish configuration from backend verification without endpoint data")
	}
}

func TestSharedDedupRefusesMalformedEndpointBeforeListenerOrBroker(t *testing.T) {
	t.Setenv("RISK_ENGINE_REDIS_URL", "not-a-redis-url-dummy-password")
	t.Setenv("RISK_ENGINE_REDIS_URL_FILE", "")
	t.Setenv("RISK_ENGINE_LISTEN", "invalid-listener")
	t.Setenv("RISK_ENGINE_NATS_URL", "nats://127.0.0.1:1")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	if code := run(); code != 2 || !strings.Contains(logs.String(), "dedup config rejected") {
		t.Fatalf("run() = %d; required pre-I/O dedup refusal was absent", code)
	}
	if strings.Contains(logs.String(), "dummy-password") || strings.Contains(logs.String(), "127.0.0.1") {
		t.Fatal("startup refusal disclosed endpoint data")
	}
}
