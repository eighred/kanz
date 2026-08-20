package venuemargin

import (
	"testing"
	"time"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
)

// PRESENCE IS THE SIGNAL, and the View is where it would be lost (#408 control
// 3, #527).
//
// domain.v1.InputCoverage's contract: absent means "this publisher does not
// report coverage", present with excluded_count = 0 means "it reports, and the
// venue answered everything asked of it". The View held only the COUNT, which
// renders both as zero — and the collapse hides the worse of the two, because
// the publisher that says nothing is the one nobody has checked.

// A REPORTED, EMPTY COVERAGE IS COMPLETE. The non-vacuity arm: without it a
// Coverage that answered "not complete" to everything would pass every test
// below and turn the gate above it into a permanent refusal.
func TestCoverage_ReportedAndEmptyIsComplete(t *testing.T) {
	v := New(WithClock(frozen))
	fold(t, v, state(t)) // state() carries Coverage{Contributed: 3}

	cov, ok := v.Coverage("OKX", "acct-1")
	if !ok {
		t.Fatal("coverage UNKNOWN after a complete observation")
	}
	if !cov.Reported() {
		t.Error("Reported() = false for an observation that carried a coverage record")
	}
	if cov.ExcludedCount() != 0 {
		t.Errorf("ExcludedCount() = %d, want 0", cov.ExcludedCount())
	}
	if !cov.Complete() {
		t.Error("Complete() = false for a reported coverage excluding nothing")
	}
}

// AN ABSENT COVERAGE RECORD IS NOT COMPLETE. This is the whole reason the type
// exists: an observation that does not say what it left out cannot be shown to
// have left out nothing, and a margin gate must refuse it.
func TestCoverage_AbsentIsNotComplete(t *testing.T) {
	msg := state(t)
	msg.Coverage = nil

	v := New(WithClock(frozen))
	fold(t, v, msg)

	cov, ok := v.Coverage("OKX", "acct-1")
	if !ok {
		t.Fatal("coverage UNKNOWN after a current observation that simply carried no coverage record")
	}
	if cov.Reported() {
		t.Error("Reported() = true for an observation with NO coverage record")
	}
	if cov.Complete() {
		t.Error("an observation that reports no coverage was read as COMPLETE — this is " +
			"InputCoverage's zero value being taken for 'everything resolved'")
	}
}

// A NON-EMPTY COVERAGE IS REPORTED AND NOT COMPLETE, and the count survives.
func TestCoverage_ExclusionsSurviveWithTheirCount(t *testing.T) {
	msg := state(t)
	msg.Coverage = &domainpb.InputCoverage{Contributed: 1, ExcludedCount: 4}

	v := New(WithClock(frozen))
	fold(t, v, msg)

	cov, ok := v.Coverage("OKX", "acct-1")
	if !ok {
		t.Fatal("coverage UNKNOWN after a current observation")
	}
	if !cov.Reported() || cov.ExcludedCount() != 4 {
		t.Errorf("got Reported=%v Excluded=%d, want true/4", cov.Reported(), cov.ExcludedCount())
	}
	if cov.Complete() {
		t.Error("an observation excluding 4 quantities was read as COMPLETE")
	}
}

// COVERAGE IS BOUND BY THE SAME FRESHNESS RULE AS THE QUANTITIES.
//
// If it were not, a gate could satisfy its completeness check from one poll and
// take its number from another — the stale book with an extra step.
func TestCoverage_StaleIsUnknown(t *testing.T) {
	now := observed
	v := New(WithClock(func() time.Time { return now }))
	fold(t, v, state(t))

	if _, ok := v.Coverage("OKX", "acct-1"); !ok {
		t.Fatal("coverage read as unknown while the observation was fresh — the rest proves nothing")
	}
	now = observed.Add(DefaultMaxAge + time.Second)
	if _, ok := v.Coverage("OKX", "acct-1"); ok {
		t.Fatalf("coverage from an observation older than DefaultMaxAge (%s) still answered",
			DefaultMaxAge)
	}
}

// A NEVER-OBSERVED ACCOUNT HAS NO COVERAGE, and the zero Coverage it yields is
// not "complete".
func TestCoverage_NeverObservedIsUnknownAndNotComplete(t *testing.T) {
	v := New(WithClock(frozen))

	cov, ok := v.Coverage("OKX", "acct-1")
	if ok {
		t.Fatal("an account nothing has observed answered with a coverage record")
	}
	if cov.Complete() {
		t.Error("the zero Coverage reports itself COMPLETE — every unknown would read as a clean bill")
	}
}
