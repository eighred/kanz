package config

import (
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
)

// A MALFORMED INTERVAL REFUSES THE START (#692).
//
// datamaster parsed five durations through a local helper that swallowed the
// parse error and returned the default, so a typo left this service on a
// schedule the operator did not choose while the deployment reported a clean
// start — "nothing configured" and "checked, and fine" looking the same, which
// is the rule AGENTS.md states.
//
// TWO OF THE FIVE BOUND AN AUTHORIZATION WINDOW. DATAMASTER_DUAL_CONTROL_TTL is
// how long a dual-control proposal stays approvable and
// DATAMASTER_LAPSED_PROPOSAL_RETENTION how long a lapsed one stays visible to
// the person who proposed it. A silent fallback there changes who can approve
// what, and for how long, with nothing said.
//
// The sibling in accounting had the correct behaviour AND the paragraph
// explaining it. The copy without the doc was the copy with the defect, which is
// the argument for there being one implementation at all — they now share
// env.Duration.

func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATAMASTER_DATABASE_URL", "postgres://user:pass@localhost:5432/db?sslmode=disable")
}

// THE MUTATION THE ISSUE NAMES, verbatim: DATAMASTER_DUAL_CONTROL_TTL=5minutes
// must refuse rather than run on dualcontrol.DefaultTTL.
func TestLoadRefusesAMalformedDualControlTTL(t *testing.T) {
	minimalEnv(t)
	t.Setenv("DATAMASTER_DUAL_CONTROL_TTL", "5minutes")

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load accepted DATAMASTER_DUAL_CONTROL_TTL=5minutes and returned TTL=%s. That is "+
			"dualcontrol.DefaultTTL (%s) — the approval window is now whatever the default happens "+
			"to be, chosen by nobody, on a service that reported a clean start (#692)",
			cfg.DualControlTTL, dualcontrol.DefaultTTL)
	}
	for _, want := range []string{"DATAMASTER_DUAL_CONTROL_TTL", "5minutes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q — an operator cannot tell which of five "+
				"intervals was wrong: %v", want, err)
		}
	}
}

// EVERY ONE OF THE FIVE, not just the one the issue used as its example. A
// repair that fixed the named key and left the other four is the half-measure
// this guard exists to prevent.
func TestLoadRefusesAMalformedValueOnEveryInterval(t *testing.T) {
	for _, key := range []string{
		"DATAMASTER_REFRESH_INTERVAL",
		"DATAMASTER_OUTBOX_INTERVAL",
		"DATAMASTER_DUAL_CONTROL_TTL",
		"DATAMASTER_LAPSED_PROPOSAL_RETENTION",
		"DATAMASTER_PROPOSAL_PURGE_INTERVAL",
	} {
		t.Run(key, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv(key, "1minute")
			if _, err := Load(); err == nil {
				t.Fatalf("%s=1minute was accepted; the service runs on a default nobody chose", key)
			}
		})
	}
}

// THE OUTBOX INTERVAL IS THE SHARPEST CASE. Its default is 0, and the old helper
// folded "unparseable" into "non-positive" — so a typo and the intended setting
// produced the same value, and no signal could distinguish them.
func TestAMalformedOutboxIntervalIsNotTheSameAsZero(t *testing.T) {
	minimalEnv(t)
	t.Setenv("DATAMASTER_OUTBOX_INTERVAL", "0s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("an explicit 0 was refused: %v", err)
	}
	if cfg.OutboxInterval != 0 {
		t.Fatalf("OutboxInterval = %s, want 0", cfg.OutboxInterval)
	}

	minimalEnv(t)
	t.Setenv("DATAMASTER_OUTBOX_INTERVAL", "0seconds")
	if _, err := Load(); err == nil {
		t.Fatal("0seconds was accepted and became 0 — indistinguishable from the operator " +
			"deliberately disabling the relay, which is exactly what the old helper could not tell apart")
	}
}

// A WELL-FORMED VALUE STILL LANDS. The non-vacuity arm: a Load that refused
// everything would pass every assertion above.
func TestLoadReadsWellFormedIntervals(t *testing.T) {
	minimalEnv(t)
	t.Setenv("DATAMASTER_REFRESH_INTERVAL", "45s")
	t.Setenv("DATAMASTER_DUAL_CONTROL_TTL", "12h")
	t.Setenv("DATAMASTER_PROPOSAL_PURGE_INTERVAL", "15m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RefreshInterval != 45*time.Second {
		t.Errorf("RefreshInterval = %s, want 45s", cfg.RefreshInterval)
	}
	if cfg.DualControlTTL != 12*time.Hour {
		t.Errorf("DualControlTTL = %s, want 12h", cfg.DualControlTTL)
	}
	if cfg.ProposalPurgeInterval != 15*time.Minute {
		t.Errorf("ProposalPurgeInterval = %s, want 15m", cfg.ProposalPurgeInterval)
	}
}

// THE POSTURES TOO. boolOr swallowed a malformed value the same way, and
// DATAMASTER_REQUIRE_DUAL_CONTROL is the one that arms maker-checker on the
// pricing override: `yes` is not a Go bool, and reading it as false is a control
// that reports itself armed and is not.
func TestLoadRefusesAMalformedPosture(t *testing.T) {
	minimalEnv(t)
	t.Setenv("DATAMASTER_REQUIRE_DUAL_CONTROL", "yes")
	cfg, err := Load()
	if err == nil {
		t.Fatalf("DATAMASTER_REQUIRE_DUAL_CONTROL=yes was accepted as %v — an operator who wrote "+
			"`yes` believes maker-checker is armed on the override path and it is not (#410)",
			cfg.RequireDualControl)
	}
}
