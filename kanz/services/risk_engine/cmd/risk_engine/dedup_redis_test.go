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

func TestSharedDedupRefusesImplicitHostAndInvalidPort(t *testing.T) {
	for _, endpoint := range []string{
		"redis://", "redis://:6379", "redis://dummy-user:dummy-password@:6379",
		"redis://cache.example:", "redis://cache.example:0", "redis://cache.example:65536",
		"redis://cache.example:999999999999999999999999", "redis://::1",
		"redis://[::1]:", "redis://[::1]:0", "redis://[::1]:65536", "redis://[not-an-ip]:6379",
	} {
		if _, err := loadDedupConfig(config.Config{RedisURL: endpoint}); err == nil {
			t.Fatal("explicit endpoint fabricated a default hostname or accepted an invalid port/authority")
		} else if strings.Contains(err.Error(), "dummy-") || strings.Contains(err.Error(), "cache.example") || strings.Contains(err.Error(), "::1") {
			t.Fatal("configuration diagnostic disclosed endpoint data")
		}
	}
}

func TestSharedDedupRetainsExplicitHostAndIPv6(t *testing.T) {
	for _, example := range []struct {
		endpoint string
		address  string
		database int
		tls      bool
	}{
		{"redis://cache.example", "cache.example:6379", 0, false},
		{"redis://localhost:1", "localhost:1", 0, false},
		{"redis://127.0.0.1:65535/3", "127.0.0.1:65535", 3, false},
		{"redis://[::1]", "[::1]:6379", 0, false},
		{"rediss://dummy-user:dummy-password@[2001:db8::1]:6380/2", "[2001:db8::1]:6380", 2, true},
	} {
		cfg, err := loadDedupConfig(config.Config{RedisURL: example.endpoint})
		if err != nil {
			t.Fatal("valid explicit endpoint was refused")
		}
		if cfg.opts.Addr != example.address || cfg.opts.DB != example.database || (cfg.opts.TLSConfig != nil) != example.tls {
			t.Fatal("explicit host/port/database/TLS selection changed")
		}
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
	for name, endpoint := range map[string]string{
		"malformed":              "not-a-redis-url-dummy-password",
		"missing-host":           "redis://",
		"missing-host-with-port": "redis://:6379",
		"zero-port":              "redis://127.0.0.1:0",
		"out-of-range-port":      "redis://127.0.0.1:65536",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("RISK_ENGINE_REDIS_URL", endpoint)
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
		})
	}
}
