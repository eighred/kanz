package app

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// #110's posture. The interesting case is the UNSHARDED one, because it is the
// state the estate is actually in and the one that produced no signal at all.

func shardPosture(t *testing.T, sharded bool, members []string, self string) (*prometheus.Registry, string) {
	return shardPostureWithSkips(t, sharded, members, self, 0)
}

func shardPostureWithSkips(t *testing.T, sharded bool, members []string, self string, foreignSkipped int) (*prometheus.Registry, string) {
	t.Helper()
	reg := prometheus.NewRegistry()
	var logs bytes.Buffer
	ShardPosture(reg, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		sharded, members, self, foreignSkipped)
	return reg, logs.String()
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		if len(f.GetMetric()) == 0 {
			t.Fatalf("%s has no series — an empty family reads as zero to every consumer, so an "+
				"alert on it cannot fire", name)
		}
		return f.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatalf("%s was never registered", name)
	return 0
}

// AN UNSHARDED FLEET REPORTS A ZERO AND SAYS SO AT WARN.
//
// This is the whole point. The previous code logged only when sharding was ON,
// so the state that needs attention was the one with no signal.
func TestShardPosture_UnshardedIsVisible(t *testing.T) {
	reg, logs := shardPosture(t, false, nil, "")

	if got := gaugeValue(t, reg, "kanz_risk_shard_enabled"); got != 0 {
		t.Errorf("kanz_risk_shard_enabled = %v on an unsharded replica, want 0", got)
	}
	if got := gaugeValue(t, reg, "kanz_risk_shard_members"); got != 0 {
		t.Errorf("kanz_risk_shard_members = %v with no ring, want 0", got)
	}
	if !bytes.Contains([]byte(logs), []byte("level=WARN")) {
		t.Errorf("an unsharded fleet logged no warning:\n%s", logs)
	}
	// THE MESSAGE MUST NAME THE MECHANISM, not the setting. An operator who
	// reads "sharding is off" concludes it is a performance choice.
	for _, want := range []string{"PARTIAL", "per-replica", "Nothing errors"} {
		if !bytes.Contains([]byte(logs), []byte(want)) {
			t.Errorf("the warning does not mention %q — it reads as a tuning knob rather than a "+
				"correctness posture:\n%s", want, logs)
		}
	}
}

// A SHARDED FLEET REPORTS 1 AND ITS MEMBER COUNT.
func TestShardPosture_ShardedReportsItsRing(t *testing.T) {
	reg, logs := shardPosture(t, true, []string{"a", "b", "c"}, "b")

	if got := gaugeValue(t, reg, "kanz_risk_shard_enabled"); got != 1 {
		t.Errorf("kanz_risk_shard_enabled = %v when sharding, want 1", got)
	}
	if got := gaugeValue(t, reg, "kanz_risk_shard_members"); got != 3 {
		t.Errorf("kanz_risk_shard_members = %v, want 3", got)
	}
	if bytes.Contains([]byte(logs), []byte("level=WARN")) {
		t.Errorf("a correctly sharded fleet warned:\n%s", logs)
	}
}

// MEMBERS WITHOUT AN IDENTITY IS STILL UNSHARDED, AND THE COUNT SAYS WHICH
// MISCONFIGURATION IT IS.
//
// "no members" and "members but no RISK_ENGINE_SHARD_SELF" produce the identical
// symptom — a replica that owns everything — and an operator fixing one needs to
// know which they have. The gauge pair separates them: enabled=0 with members>0
// is the second.
func TestShardPosture_MembersWithoutAnIdentityIsDistinguishable(t *testing.T) {
	reg, logs := shardPosture(t, false, []string{"a", "b", "c"}, "")

	if got := gaugeValue(t, reg, "kanz_risk_shard_enabled"); got != 0 {
		t.Errorf("enabled = %v, want 0 — no self means no ownership filter", got)
	}
	if got := gaugeValue(t, reg, "kanz_risk_shard_members"); got != 3 {
		t.Errorf("members = %v, want 3 — the configured ring is still what it is, and the count "+
			"is how an operator tells this apart from an unconfigured fleet", got)
	}
	if !bytes.Contains([]byte(logs), []byte("level=WARN")) {
		t.Error("a fleet with members but no identity did not warn")
	}
}

// A RING OF ONE IS SHARDING AND IS STILL A SINGLE-INSTANCE FLEET.
//
// enabled=1 alone would read as healthy. The member count is what makes a
// one-member ring distinguishable from a real split, which matters because it is
// what a half-finished rollout looks like.
func TestShardPosture_ARingOfOneIsNotAFleet(t *testing.T) {
	reg, _ := shardPosture(t, true, []string{"only-me"}, "only-me")

	if got := gaugeValue(t, reg, "kanz_risk_shard_enabled"); got != 1 {
		t.Errorf("enabled = %v, want 1", got)
	}
	if got := gaugeValue(t, reg, "kanz_risk_shard_members"); got != 1 {
		t.Errorf("members = %v, want 1 — sum(enabled) alone cannot tell a one-member ring from a "+
			"three-member one", got)
	}
}

// THE GAUGE EXISTS BEFORE ANYTHING HAPPENS. A metric family with no series reads
// as zero to every consumer, so an alert on it can never fire (#283).
func TestShardPosture_TheSeriesExistsImmediately(t *testing.T) {
	reg, _ := shardPosture(t, false, nil, "")
	for _, name := range []string{
		"kanz_risk_shard_enabled",
		"kanz_risk_shard_members",
		"kanz_risk_shard_foreign_records_skipped",
	} {
		if got := testutil.CollectAndCount(reg, name); got == 0 {
			t.Errorf("%s has no series at startup", name)
		}
	}
}

// THE BOOT PATH'S REFUSALS ARE COUNTED, AND THE ZERO IS REPORTED.
//
// A sharded replica that restored every portfolio in the database instead of
// only its own is the state this metric exists to rule out — and the reading
// that says so is a ZERO on a fleet that should have skipped some. A series that
// only appeared once it was non-zero could not express that.
func TestShardPosture_ForeignRecordsSkippedIsReported(t *testing.T) {
	reg, _ := shardPostureWithSkips(t, true, []string{"a", "b", "c"}, "b", 7)
	if got := gaugeValue(t, reg, "kanz_risk_shard_foreign_records_skipped"); got != 7 {
		t.Errorf("kanz_risk_shard_foreign_records_skipped = %v, want 7", got)
	}

	unsharded, _ := shardPosture(t, false, nil, "")
	if got := gaugeValue(t, unsharded, "kanz_risk_shard_foreign_records_skipped"); got != 0 {
		t.Errorf("an unsharded replica skipped %v records — it owns everything, so nothing is "+
			"foreign to it", got)
	}
}
