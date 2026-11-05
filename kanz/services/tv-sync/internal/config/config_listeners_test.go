package config_test

import (
	"testing"

	"github.com/eighred/kanz/services/tv-sync/internal/config"
)

func TestListenerDefaultsAndOverrides(t *testing.T) {
	requireBook(t)
	t.Setenv("TV_SYNC_LISTEN", "")
	t.Setenv("TV_SYNC_API_LISTEN", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8091" || cfg.APIListen != ":8113" {
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
