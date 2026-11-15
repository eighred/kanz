package config

import (
	"os"
	"strings"
	"testing"
)

func TestBFFWorkloadConfiguration(t *testing.T) {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "API_GATEWAY_") {
			t.Setenv(key, "")
		}
	}
	devAuthEnv(t)
	for _, tc := range []struct {
		name, listen, socket string
		allowed              bool
	}{
		{"disabled", "", "", true},
		{"enabled", ":9443", "unix:///synthetic-spire.sock", true},
		{"no_identity", ":9443", "", false},
		{"same_listener", ":8080", "unix:///synthetic-spire.sock", false},
		{"privileged", ":443", "unix:///synthetic-spire.sock", false},
		{"not_tcp", "unix:///socket", "unix:///synthetic-spire.sock", false},
		{"no_port", "gateway.invalid", "unix:///synthetic-spire.sock", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("API_GATEWAY_LISTEN", ":8080")
			t.Setenv("API_GATEWAY_BFF_TLS_LISTEN", tc.listen)
			t.Setenv("API_GATEWAY_SPIFFE_SOCKET", tc.socket)
			cfg, err := Load()
			if (err == nil) != tc.allowed {
				t.Fatal("BFF workload listener configuration outcome differs")
			}
			if err == nil && cfg.BFFTLSListen != tc.listen {
				t.Fatal("workload listener was discarded")
			}
		})
	}
}
