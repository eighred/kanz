package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/tv-sync/internal/config"
)

func TestListenerDefaultsAndOverrides(t *testing.T) {
	requireBook(t)
	t.Setenv("TV_SYNC_LISTEN", "")
	t.Setenv("TV_SYNC_API_LISTEN", "")
	if err := os.Unsetenv("TV_SYNC_API_LISTEN"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8091" || cfg.APIListen != "" {
		t.Fatalf("default listeners: operations=%q API=%q", cfg.Listen, cfg.APIListen)
	}
	t.Setenv("TV_SYNC_LISTEN", "127.0.0.1:18091")
	t.Setenv("TV_SYNC_API_LISTEN", "127.0.0.1:18113")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:18091" || cfg.APIListen != "127.0.0.1:18113" {
		t.Fatalf("overridden listeners: operations=%q API=%q", cfg.Listen, cfg.APIListen)
	}
}

func TestExplicitInvalidAPIListenerRefusesStartup(t *testing.T) {
	for _, address := range []string{"", " ", "\t", " :8113", ":8113 ", "8113", "http://localhost:8113", ":invalid", ":0", ":65536"} {
		t.Run(address, func(t *testing.T) {
			requireBook(t)
			t.Setenv("TV_SYNC_API_LISTEN", address)
			if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "TV_SYNC_API_LISTEN") {
				t.Fatalf("explicit API address %q did not refuse configuration: %v", address, err)
			}
		})
	}
}
