package config

import "testing"

// AUTO-PUBLISH WITHOUT A BROKER IS REFUSED (#409).
//
// The two settings together are the whole difference between a service that
// proposes and one that trades. Accepting the flag alone yields a deployment
// whose configuration says it trades and whose behaviour is a dry run — and the
// direction of that mistake is the dangerous one: an operator reads the flag,
// believes orders are going out, and learns otherwise from a fill that never
// arrives.
func TestAutoPublishWithoutABrokerIsRefused(t *testing.T) {
	t.Setenv("OPTIMIZATION_AUTO_PUBLISH", "true")
	t.Setenv("OPTIMIZATION_NATS_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("config.Load accepted OPTIMIZATION_AUTO_PUBLISH=true with no broker. That " +
			"deployment reports that it trades on its own recommendation and silently does not.")
	}
}

// Both set is the armed posture and loads.
func TestAutoPublishWithABrokerLoads(t *testing.T) {
	t.Setenv("OPTIMIZATION_AUTO_PUBLISH", "true")
	t.Setenv("OPTIMIZATION_NATS_URL", "nats://localhost:4222")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AutoPublish {
		t.Error("AutoPublish is false with the flag set")
	}
}

// THE DEFAULT IS OFF. A deployment must never acquire the ability to trade on
// its own recommendation by omission — this is the assertion that pins it.
func TestAutoPublishDefaultsOff(t *testing.T) {
	t.Setenv("OPTIMIZATION_AUTO_PUBLISH", "")
	t.Setenv("OPTIMIZATION_NATS_URL", "nats://localhost:4222")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AutoPublish {
		t.Error("auto-publish defaults ON. The optimizer proposes and a human approves unless " +
			"someone asks otherwise by name.")
	}
}
