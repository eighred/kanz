package natsrebuild

import (
	"strings"
	"testing"
	"time"
)

func TestWindowForStateTopicReadsFromTheEarliestRetainedOffset(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	state := map[string]bool{"compliance.mandate": true}

	r := WindowFor("compliance.mandate", state, 24*time.Hour, now)

	// A compacted topic retains the latest record per key REGARDLESS of age.
	// A time lower bound would skip a mandate armed before the window and the
	// control would come back DISARMED — so the lower bound must be offset 0.
	if r.StartOffset == nil || *r.StartOffset != 0 {
		t.Fatalf("StartOffset = %v, want 0", r.StartOffset)
	}
	if r.StartTime != nil {
		t.Fatalf("StartTime = %v, want nil (mutually exclusive with StartOffset)", r.StartTime)
	}
	if r.EndTime == nil || !r.EndTime.Equal(now) {
		t.Fatalf("EndTime = %v, want %v", r.EndTime, now)
	}
}

func TestWindowForEventTopicReadsTheBoundedRecentWindow(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	state := map[string]bool{"compliance.mandate": true}

	r := WindowFor("order.order", state, 24*time.Hour, now)

	want := now.Add(-24 * time.Hour)
	if r.StartTime == nil || !r.StartTime.Equal(want) {
		t.Fatalf("StartTime = %v, want %v", r.StartTime, want)
	}
	if r.StartOffset != nil {
		t.Fatalf("StartOffset = %v, want nil (mutually exclusive with StartTime)", r.StartOffset)
	}
	if r.EndTime == nil || !r.EndTime.Equal(now) {
		t.Fatalf("EndTime = %v, want %v", r.EndTime, now)
	}
}

func TestRequireStateTopicsRefusesAnEmptyList(t *testing.T) {
	// Fail closed: an empty NATS_REBUILD_STATE_TOPICS has no orphans, so
	// ValidateTopicClasses' subset check alone would pass it — an operator
	// who drops the state list following the DR runbook gets compacted
	// topics read through the time window instead of in full, and the run
	// still exits 0 with a plausible non-zero published count. This check is
	// what refuses that configuration outright.
	err := RequireStateTopics(nil)
	if err == nil {
		t.Fatal("want an error refusing an empty state-topic list, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "NATS_REBUILD_STATE_TOPICS") {
		t.Fatalf("error %q does not name NATS_REBUILD_STATE_TOPICS", got)
	}
}

func TestRequireStateTopicsRefusesAnEmptySliceTooNotJustNil(t *testing.T) {
	// splitList (main.go) returns an empty, non-nil slice for an empty env
	// var — make sure the check does not accidentally key off nil-ness.
	err := RequireStateTopics([]string{})
	if err == nil {
		t.Fatal("want an error refusing an empty state-topic list, got nil")
	}
}

func TestRequireStateTopicsAcceptsANonEmptyList(t *testing.T) {
	// Non-vacuity: a check that rejected everything would pass the two
	// tests above. This proves the accept path still accepts.
	if err := RequireStateTopics([]string{"compliance.mandate", "risk.position"}); err != nil {
		t.Fatalf("non-empty state-topic list rejected: %v", err)
	}
}

func TestValidateTopicClassesRefusesAStateTopicThatIsNotBeingRebuilt(t *testing.T) {
	// Fail closed: a state topic nobody reads is a config typo, and silently
	// ignoring it means the operator believes state is being restored when it
	// is not — the exact failure this whole change exists to remove.
	err := ValidateTopicClasses(
		[]string{"order.order"},
		[]string{"compliance.mandate"},
	)
	if err == nil {
		t.Fatal("want an error naming compliance.mandate, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "compliance.mandate") {
		t.Fatalf("error %q does not name the offending topic", got)
	}
}

func TestValidateTopicClassesReportsEveryOrphanNotJustTheFirst(t *testing.T) {
	// Fail closed means the operator gets one shot to fix the config before
	// re-running into the same wall: an error naming only the first of several
	// misconfigured state topics sends them back to fix one and hit the next.
	err := ValidateTopicClasses(
		[]string{"order.order"},
		[]string{"compliance.mandate", "risk.limit"},
	)
	if err == nil {
		t.Fatal("want an error naming compliance.mandate and risk.limit, got nil")
	}
	got := err.Error()
	if !strings.Contains(got, "compliance.mandate") {
		t.Fatalf("error %q does not name compliance.mandate", got)
	}
	if !strings.Contains(got, "risk.limit") {
		t.Fatalf("error %q does not name risk.limit", got)
	}
}

func TestValidateTopicClassesAcceptsAProperSubset(t *testing.T) {
	// Non-vacuity: a validator that rejected everything would pass the test
	// above. This proves the accept path still accepts.
	if err := ValidateTopicClasses(
		[]string{"order.order", "compliance.mandate"},
		[]string{"compliance.mandate"},
	); err != nil {
		t.Fatalf("valid subset rejected: %v", err)
	}
}
