package config_test

import (
	"testing"

	"github.com/kanz-eng/kanz/services/archiver/internal/config"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func valid() map[string]string {
	return map[string]string{
		"ARCHIVER_TENANT":        "acme",
		"ARCHIVER_NATS_URL":      "nats://localhost:4222",
		"ARCHIVER_KAFKA_BROKERS": "localhost:9092",
	}
}

func TestLoad_Valid(t *testing.T) {
	setEnv(t, valid())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tenant != "acme" {
		t.Errorf("Tenant = %q, want acme", cfg.Tenant)
	}
	if len(cfg.Brokers) != 1 || cfg.Brokers[0] != "localhost:9092" {
		t.Errorf("Brokers = %v", cfg.Brokers)
	}
	// The in-scope streams default in — market and observability stay OUT.
	if len(cfg.Subjects) == 0 {
		t.Fatal("Subjects is empty — the archiver would archive nothing")
	}
	for _, s := range cfg.Subjects {
		if s == "market.>" || s == "observability.>" {
			t.Errorf("subject %q is out of scope for DATA-M1", s)
		}
	}
}

// FAIL CLOSED AT STARTUP. An archiver with no tenant cannot route; an archiver with
// no Kafka has nowhere to archive TO. Either one, running, looks healthy and
// silently retains nothing — so it must not start.
func TestLoad_FailsClosed(t *testing.T) {
	for _, missing := range []string{"ARCHIVER_TENANT", "ARCHIVER_NATS_URL", "ARCHIVER_KAFKA_BROKERS"} {
		t.Run("missing "+missing, func(t *testing.T) {
			env := valid()
			delete(env, missing)
			// t.Setenv on the others; the missing one is simply never set.
			setEnv(t, env)
			t.Setenv(missing, "")

			if _, err := config.Load(); err == nil {
				t.Fatalf("Load succeeded without %s — a silently non-archiving archiver reports healthy", missing)
			}
		})
	}
}
