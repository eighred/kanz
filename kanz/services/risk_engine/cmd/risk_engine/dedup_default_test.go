//go:build !redis

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/risk_engine/internal/config"
)

func TestDefaultDedupBuildRefusesExplicitSharedSelection(t *testing.T) {
	for _, endpoint := range []string{"redis://dummy-user:dummy-password@127.0.0.1:6379", "invalid-explicit-selection"} {
		if _, err := loadDedupConfig(config.Config{RedisURL: endpoint}); err == nil {
			t.Fatal("explicit shared selection was silently replaced by per-instance dedup")
		} else if strings.Contains(err.Error(), endpoint) || strings.Contains(err.Error(), "dummy-password") {
			t.Fatal("configuration diagnostic disclosed the endpoint")
		}
	}
}

func TestDefaultDedupBuildRefusesBeforeListenerOrBroker(t *testing.T) {
	t.Setenv("RISK_ENGINE_REDIS_URL", "redis://dummy-user:dummy-password@127.0.0.1:6379")
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
