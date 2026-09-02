package bridge

// Freshness gate tests (#970).
//
// The gate stands between a proposal and live MARKET orders, so the cases that
// matter are the ones where it must REFUSE. Each test below names the capital
// consequence of the refusal not happening, because a test that only asserts an
// error code cannot tell a reviewer whether the code is the right one.

import (
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/optimization"
)

var freshT0 = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func freshAt(t time.Time) optimization.RebalanceProposal {
	p := sampleProposal()
	p.AsOf = t
	return p
}

func fixedFreshness(maxAge time.Duration) Freshness {
	return Freshness{MaxAge: maxAge, Now: func() time.Time { return freshT0 }}
}

// A PROPOSAL WITHIN THE BOUND PASSES. Asserted first so every refusal below is
// known to be the gate discriminating rather than refusing everything — a gate
// that never admits would satisfy all the negative cases.
func TestAFreshProposalIsAdmitted(t *testing.T) {
	age, err := fixedFreshness(time.Hour).Check(freshAt(freshT0.Add(-30 * time.Minute)))
	if err != nil {
		t.Fatalf("a 30-minute-old proposal under a 1h bound was refused: %v", err)
	}
	if age != 30*time.Minute {
		t.Fatalf("age = %s, want 30m — the reported age is what an operator triages by", age)
	}
	// And the boundary itself is inclusive: exactly at the bound is still fresh.
	if _, err := fixedFreshness(time.Hour).Check(freshAt(freshT0.Add(-time.Hour))); err != nil {
		t.Fatalf("a proposal exactly at the bound was refused: %v", err)
	}
}

// THE DEFECT ITSELF. A proposal whose inputs predate the bound describes a book
// that has plausibly moved, and its trades are a DELTA against that book — so
// materializing it sends the wrong quantity as a MARKET order.
func TestAStaleProposalIsRefused(t *testing.T) {
	age, err := fixedFreshness(time.Hour).Check(freshAt(freshT0.Add(-4 * time.Hour)))
	if !errors.Is(err, ErrProposalStale) {
		t.Fatalf("a 4-hour-old proposal under a 1h bound was admitted (err=%v) — its trade list is a "+
			"delta against a book that has since moved, and every child is a MARKET order", err)
	}
	if age != 4*time.Hour {
		t.Fatalf("age = %s, want 4h", age)
	}
	if got := RefusalCode(err); got != "STALE_PROPOSAL" {
		t.Fatalf("refusal code = %q, want STALE_PROPOSAL", got)
	}
}

// A FUTURE-DATED PROPOSAL DEFEATS THE BOUND ENTIRELY. as_of is caller-supplied,
// so without this check any caller wanting an unbounded proposal need only date
// it forward — the age is then negative and never exceeds MaxAge.
func TestAFutureDatedProposalIsRefused(t *testing.T) {
	err := func() error {
		_, err := fixedFreshness(time.Hour).Check(freshAt(freshT0.Add(time.Hour)))
		return err
	}()
	if !errors.Is(err, ErrProposalFuture) {
		t.Fatalf("a proposal dated an hour in the FUTURE was admitted (err=%v) — this is how a caller "+
			"escapes the age bound: a negative age never exceeds MaxAge", err)
	}
	if got := RefusalCode(err); got != "STALE_PROPOSAL" {
		t.Fatalf("refusal code = %q, want STALE_PROPOSAL (same operator action as stale)", got)
	}
}

// CLOCK SKEW IS TOLERATED, AND ONLY JUST. The optimizer and this service are
// different processes on different hosts; refusing on millisecond drift would be
// a refusal no operator could act on. The allowance must not be wide enough to
// hide a book moving.
func TestClockSkewIsToleratedButNotUsableAsABound(t *testing.T) {
	within := freshT0.Add(ClockSkewAllowance - time.Second)
	if _, err := fixedFreshness(time.Hour).Check(freshAt(within)); err != nil {
		t.Fatalf("a proposal %s ahead was refused inside the %s allowance: %v",
			ClockSkewAllowance-time.Second, ClockSkewAllowance, err)
	}
	beyond := freshT0.Add(ClockSkewAllowance + time.Second)
	if _, err := fixedFreshness(time.Hour).Check(freshAt(beyond)); !errors.Is(err, ErrProposalFuture) {
		t.Fatalf("a proposal beyond the skew allowance was admitted: %v", err)
	}
	if ClockSkewAllowance > time.Minute {
		t.Fatalf("ClockSkewAllowance is %s — wide enough to be used as a freshness bound rather than "+
			"a drift tolerance", ClockSkewAllowance)
	}
}

// AN UNDATED PROPOSAL STATED NO HORIZON, which is not the same as stating a
// recent one. Refused for the reason MandateUnchecked is: an absent claim must
// not read as a clean one.
func TestAnUndatedProposalIsRefused(t *testing.T) {
	var zero time.Time
	_, err := fixedFreshness(time.Hour).Check(freshAt(zero))
	if !errors.Is(err, ErrProposalUndated) {
		t.Fatalf("a proposal with no as_of was admitted (err=%v)", err)
	}
	if got := RefusalCode(err); got != "UNDATED_PROPOSAL" {
		t.Fatalf("refusal code = %q, want UNDATED_PROPOSAL — it is a different operator problem "+
			"from a stale one", got)
	}
}

// AN UNSET BOUND IS UNKNOWN, NOT UNLIMITED. This is the fail-closed direction and
// the whole reason Freshness has no default: a deployment that forgot the setting
// must not silently restore the pre-#970 behaviour of materializing anything.
func TestAnUnconfiguredBoundRefusesEverything(t *testing.T) {
	for _, maxAge := range []time.Duration{0, -time.Hour} {
		_, err := Freshness{MaxAge: maxAge, Now: func() time.Time { return freshT0 }}.
			Check(freshAt(freshT0))
		if !errors.Is(err, ErrFreshnessUnbounded) {
			t.Fatalf("MaxAge %s admitted a proposal (err=%v) — an unset bound must refuse, not "+
				"pass everything", maxAge, err)
		}
	}
	// The zero Freshness — what a composition root that forgot the option leaves
	// behind — must refuse too.
	if _, err := (Freshness{}).Check(freshAt(freshT0)); !errors.Is(err, ErrFreshnessUnbounded) {
		t.Fatalf("the zero Freshness admitted a proposal: %v", err)
	}
}

// THE UNCONFIGURED CASE IS A DIFFERENT CODE, because it is a different owner's
// problem: nothing is wrong with the proposal, and telling the caller to re-run
// the optimizer would send them round a loop that refuses forever.
func TestUnconfiguredIsNotReportedAsStale(t *testing.T) {
	_, err := (Freshness{}).Check(freshAt(freshT0))
	if got := RefusalCode(err); got != "FRESHNESS_UNCONFIGURED" {
		t.Fatalf("refusal code = %q, want FRESHNESS_UNCONFIGURED", got)
	}
	if !IsFreshnessRefusal(err) {
		t.Fatal("IsFreshnessRefusal did not recognise its own error")
	}
	if IsFreshnessRefusal(errors.New("something else")) {
		t.Fatal("IsFreshnessRefusal claimed an unrelated error")
	}
	if got := RefusalCode(ErrMandateInfeasible); got != "" {
		t.Fatalf("RefusalCode claimed a mandate error as a freshness refusal: %q", got)
	}
}

// THE GATE IS AT THE CHOKE POINT. ToOrders is the one function every
// materialization path passes through — server.materialize calls it directly, so
// a check placed only in Materialize would be skipped by the HTTP route.
func TestToOrdersRefusesAStaleProposalBeforeBuildingAnything(t *testing.T) {
	stale := freshAt(freshT0.Add(-4 * time.Hour))
	cmds, err := ToOrders(stale, "alice", fixedFreshness(time.Hour))
	if !errors.Is(err, ErrProposalStale) {
		t.Fatalf("ToOrders admitted a stale proposal: err=%v", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("ToOrders built %d commands for a refused proposal — nothing may be constructed "+
			"before the refusal", len(cmds))
	}
}

// STALENESS IS REPORTED BEFORE INFEASIBILITY. A mandate breach computed against a
// four-hour-old book is not a fact about today's mandate, and reporting it as one
// sends somebody to investigate a limit that may never have been breached.
func TestAStaleAndInfeasibleProposalIsReportedStale(t *testing.T) {
	p := freshAt(freshT0.Add(-4 * time.Hour))
	p.MandateStatus = optimization.MandateInfeasible
	p.Violations = []string{"sector cap"}
	_, err := ToOrders(p, "alice", fixedFreshness(time.Hour))
	if !errors.Is(err, ErrProposalStale) {
		t.Fatalf("a stale AND infeasible proposal was reported as %v — staleness is the more "+
			"useful answer, because the infeasibility was computed against a book nobody holds", err)
	}
}

// A FRESH PROPOSAL STILL FACES THE MANDATE GATE. The freshness check must not
// have replaced #646's refusal, only preceded it.
func TestFreshnessDoesNotReplaceTheMandateCheck(t *testing.T) {
	p := freshAt(freshT0)
	p.MandateStatus = optimization.MandateUnchecked
	if _, err := ToOrders(p, "alice", fixedFreshness(time.Hour)); !errors.Is(err, ErrMandateUnchecked) {
		t.Fatalf("a fresh but UNCHECKED proposal was admitted or misreported: %v — #646's refusal "+
			"must survive #970", err)
	}
}

// A NIL CLOCK RESOLVES TO WALL TIME rather than the zero instant, which would
// make every proposal look decades in the future.
func TestANilClockResolvesToNow(t *testing.T) {
	f := Freshness{MaxAge: time.Hour}
	if _, err := f.Check(freshAt(time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("a one-minute-old proposal was refused under a nil clock: %v", err)
	}
}
