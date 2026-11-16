package config

import (
	"os"
	"strings"
	"testing"
)

func TestGatewayWorkloadConfiguration(t *testing.T) {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "WEB_BFF_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("WEB_BFF_IDENTITY_URL", "http://identity.invalid")
	for _, tc := range []struct {
		name, url, socket string
		allowed           bool
	}{
		{"legacy", "http://gateway.invalid", "", true},
		{"workload", "https://gateway.invalid:9443", "unix:///synthetic-spire.sock", true},
		{"plaintext", "http://gateway.invalid", "unix:///synthetic-spire.sock", false},
		{"credential_url", "https://user:synthetic@gateway.invalid", "unix:///synthetic-spire.sock", false},
		{"query_url", "https://gateway.invalid?override=yes", "unix:///synthetic-spire.sock", false},
		{"fragment_url", "https://gateway.invalid#fragment", "unix:///synthetic-spire.sock", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WEB_BFF_GATEWAY_URL", tc.url)
			t.Setenv("WEB_BFF_GATEWAY_SPIFFE_SOCKET", tc.socket)
			cfg, err := Load()
			if (err == nil) != tc.allowed {
				t.Fatal("gateway identity configuration outcome differs")
			}
			if err == nil && cfg.GatewaySPIFFESocket != tc.socket {
				t.Fatal("workload socket was discarded")
			}
		})
	}
}
