package config

import (
	"os"
	"strings"
	"testing"
)

func clearFoldEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"ENABLED", "EXECUTABLE", "EXPECTED_VERSION", "TIMEOUT"} {
		k := "OMS_RUST_FILL_SHADOW_" + key
		old, exists := os.LookupEnv(k)
		_ = os.Unsetenv(k)
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(k, old)
			} else {
				_ = os.Unsetenv(k)
			}
		})
	}
}
func TestFillShadowDefaultAndPartialRefusal(t *testing.T) {
	clearFoldEnv(t)
	if c, e := fillFoldShadow(); e != nil || c.Enabled {
		t.Fatal("default not off", e)
	}
	for _, item := range []struct{ k, v string }{{"ENABLED", "bad"}, {"EXECUTABLE", "relative"}, {"EXPECTED_VERSION", "stamp"}, {"TIMEOUT", "bad"}, {"TIMEOUT", "1s"}} {
		t.Run(item.k+item.v, func(t *testing.T) {
			t.Setenv("OMS_RUST_FILL_SHADOW_"+item.k, item.v)
			if _, e := fillFoldShadow(); e == nil {
				t.Fatal("partial config accepted")
			}
		})
	}
}
func TestFillShadowRequiresDurableConsumers(t *testing.T) {
	clearFoldEnv(t)
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	for k, v := range map[string]string{"OMS_RUST_FILL_SHADOW_ENABLED": "true", "OMS_RUST_FILL_SHADOW_EXECUTABLE": exe, "OMS_RUST_FILL_SHADOW_EXPECTED_VERSION": "stamp", "OMS_DATABASE_URL": "", "OMS_DATABASE_URL_FILE": "", "OMS_NATS_URL": ""} {
		t.Setenv(k, v)
	}
	if _, e := Load(); e == nil || !strings.Contains(e.Error(), "durable OMS database") {
		t.Fatal(e)
	}
}
