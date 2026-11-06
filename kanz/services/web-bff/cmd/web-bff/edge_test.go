package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEdgeConfigurationDoesNotLoadApplicationCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WEB_BFF_SIGNING_SECRET_FILE", filepath.Join(dir, "missing-synthetic-secret"))
	t.Setenv("WEB_BFF_SESSION_MODE", "invalid-synthetic-mode")
	t.Setenv("WEB_BFF_GATEWAY_URL", "")
	t.Setenv("WEB_BFF_IDENTITY_URL", "")
	path := filepath.Join(dir, "envoy.json")
	if runEdgeConfig([]string{"--output", path}) != 0 {
		t.Fatal("static configuration attempted application startup")
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatal("static configuration was not written")
	}
	for _, args := range [][]string{nil, {"--output", path, "--unknown"},
		{"--output", path, "--listen-port", "80"},
		{"--output", path, "--requests-per-second", "0"}} {
		if runEdgeConfig(args) != 2 {
			t.Fatal("invalid init command must fail without starting the service")
		}
	}
}
