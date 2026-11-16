package config

import (
	"os"
	"testing"
)

func TestLoadWorkloadIdentity(t *testing.T) {
	t.Setenv("LAKE_SINK_BROKERS", "localhost:9092")
	t.Setenv("LAKE_SINK_TOPICS", "order.order")
	t.Setenv("LAKE_SINK_OUTPUT_DIR", t.TempDir())
	for _, socket := range []string{"", "unix:///run/spiffe/spire-agent.sock", "invalid-socket"} {
		t.Run(socket, func(t *testing.T) {
			t.Setenv("SPIFFE_ENDPOINT_SOCKET", socket)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SPIFFESocket != socket {
				t.Fatalf("socket = %q, want %q", cfg.SPIFFESocket, socket)
			}
			// Invalid configured values remain explicit so NewMesh can refuse them.
			// Config must never silently turn one into the plaintext empty value.
		})
	}
}

func TestLoadRawRetention(t *testing.T) {
	t.Setenv("LAKE_SINK_BROKERS", "localhost:9092")
	t.Setenv("LAKE_SINK_TOPICS", "order.order")
	t.Setenv("LAKE_SINK_OUTPUT_DIR", t.TempDir())
	t.Setenv("LAKE_SINK_RETAIN_RAW_FRAME", "placeholder")
	if err := os.Unsetenv("LAKE_SINK_RETAIN_RAW_FRAME"); err != nil {
		t.Fatal(err)
	}
	absent, err := Load()
	if err != nil || absent.RetainRawFrame {
		t.Fatal("absent raw mode must be disabled")
	}
	for _, value := range []string{"", "false", "true", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("LAKE_SINK_RETAIN_RAW_FRAME", value)
			cfg, err := Load()
			if value == "invalid" || value == "" {
				if err == nil {
					t.Fatal("invalid boolean accepted")
				}
				return
			}
			if err != nil || cfg.RetainRawFrame != (value == "true") {
				t.Fatalf("config=%+v err=%v", cfg, err)
			}
		})
	}
}
