package config

import "testing"

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
