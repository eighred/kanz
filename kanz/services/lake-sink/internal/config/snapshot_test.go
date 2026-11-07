package config

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestSnapshotRequiresExplicitCompleteConfiguration(t *testing.T) {
	t.Setenv("LAKE_SNAPSHOT_ENABLED", "false")
	if cfg, err := loadSnapshot(Config{}); err != nil || cfg != nil {
		t.Fatal("default landing mode changed")
	}
	t.Setenv("LAKE_SNAPSHOT_ENABLED", "")
	if _, err := loadSnapshot(Config{}); err == nil {
		t.Fatal("empty opt-in accepted")
	}
	t.Setenv("LAKE_SNAPSHOT_ENABLED", "true")
	base := Config{RetainRawFrame: true, SPIFFESocket: "unix:///fixture", OutputDir: t.TempDir()}
	settings := map[string]string{
		"LAKE_SNAPSHOT_TENANT":        "fixture-tenant",
		"LAKE_SNAPSHOT_LISTEN":        "127.0.0.1:8086",
		"LAKE_SNAPSHOT_OUTPUT_DIR":    t.TempDir(),
		"LAKE_SNAPSHOT_DATABASE_URL":  "postgres://fixture@127.0.0.1/fixture?sslmode=disable",
		"LAKE_SNAPSHOT_NATS_URL":      "tls://127.0.0.1:4222",
		"LAKE_SNAPSHOT_NATS_ACCOUNT":  "fixture-account",
		"LAKE_SNAPSHOT_POLICY_FILE":   filepath.Join(t.TempDir(), "fixture-policy.json"),
		"LAKE_SNAPSHOT_WORKER_PATH":   filepath.Join(t.TempDir(), "worker"),
		"LAKE_SNAPSHOT_WORKER_SHA256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"LAKE_SNAPSHOT_GATEWAY_ID":    "spiffe://fixture.test/ns/kanz-services/sa/api-gateway",
	}
	for key, value := range settings {
		t.Setenv(key, value)
	}
	if _, err := loadSnapshot(base); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAKE_SINK_BROKERS", "127.0.0.1:9092")
	t.Setenv("LAKE_SINK_TOPICS", "order.order,market.trade")
	t.Setenv("LAKE_SINK_OUTPUT_DIR", base.OutputDir)
	t.Setenv("LAKE_SINK_RETAIN_RAW_FRAME", "true")
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", base.SPIFFESocket)
	for _, enabled := range []string{"false", "true"} {
		t.Run("topics-"+enabled, func(t *testing.T) {
			t.Setenv("LAKE_SNAPSHOT_ENABLED", enabled)
			loaded, err := Load()
			want := []string{"order.order", "market.trade"}
			if enabled == "true" {
				want = append(want, "lake.snapshot")
			}
			if err != nil || !slices.Equal(loaded.Topics, want) {
				t.Fatal("optional topic changed legacy landing or was omitted")
			}
			if enabled == "true" {
				t.Setenv("LAKE_SINK_TOPICS", "order.order,market.trade,lake.snapshot")
				loaded, err = Load()
				if err != nil || !slices.Equal(loaded.Topics, want) {
					t.Fatal("optional topic duplicated")
				}
			}
		})
	}
	for key := range settings {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "")
			if _, err := loadSnapshot(base); err == nil {
				t.Fatal("missing required setting accepted")
			}
		})
	}
	for _, listen := range []string{"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:http"} {
		t.Run(listen, func(t *testing.T) {
			t.Setenv("LAKE_SNAPSHOT_LISTEN", listen)
			if _, err := loadSnapshot(base); err == nil {
				t.Fatal("unbounded or ambiguous listen port accepted")
			}
		})
	}
	base.RetainRawFrame = false
	if _, err := loadSnapshot(base); err == nil {
		t.Fatal("snapshot without raw custody accepted")
	}
}
