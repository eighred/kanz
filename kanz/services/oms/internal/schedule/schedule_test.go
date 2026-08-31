package schedule

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution/algo"
	"github.com/eighred/kanz/internal/orderid"
)

// WHICH CHILDREN SHOULD EXIST RIGHT NOW (#435).
//
// #435's third Verified-when clause — "a cancelled parent cancels every unsent
// child" — is proved here, because this is where the decision to create a child
// is actually made. The other two clauses are properties of the arithmetic and
// live in internal/execution/algo.
//
// Everything in this file is a pure decision: no store, no bus, no venue. That is
// the point. A pod that has been running all day and a pod that booted a second
// ago must reach the same answer from the same three inputs, or a restart either
// re-sends children or skips them forever.

var (
	windowStart = time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC)
	windowEnd   = time.Date(2026, 8, 15, 15, 0, 0, 0, time.UTC)
)

// parent builds a 60-unit parent worked in 6 slices — one every 10 minutes.
//
// Algo IS NAMED HERE AND NOWHERE ELSE IN THIS FILE, which is the whole of what
// #868 changed about these tests. Every assertion below is untouched: the seam
// was required to leave TWAP's behaviour identical, and it did. What it removed
// is the ABILITY to leave it unnamed — Due now resolves the algorithm the parent
// names through the registry and refuses one it cannot work, so a fixture that
// says nothing is refused rather than silently worked as TWAP. Deleting this one
// line makes every test below fail with that refusal, which is the check working.
func parent(id string) Parent {
	return Parent{
		OrderID: id,
		Plan: algo.Plan{
			Algo:   algo.NameTWAP,
			Total:  new(big.Rat).SetInt64(60),
			Start:  windowStart,
			End:    windowEnd,
			Slices: 6,
		},
	}
}

// none is an "exists" predicate for a parent with no children yet.
func none(string) bool { return false }

// setOf builds an exists predicate over a fixed set of child IDs.
func setOf(ids ...string) func(string) bool {
	held := make(map[string]bool, len(ids))
	for _, id := range ids {
		held[id] = true
	}
	return func(id string) bool { return held[id] }
}

// ===== CLAUSE (c) OF #435's VERIFIED-WHEN =====

// A CANCELLED PARENT CANCELS EVERY UNSENT CHILD.
//
// This is the assertion #435 names. An unsent child is one that was never
// created, so cancelling it means never creating it — and a terminal parent
// yielding nothing IS that cancellation. Without this, a cancel would stop the
// parent while the driver kept working its schedule, putting orders in front of
// a venue for a parent an operator has already pulled: the single worst outcome
// this whole feature can produce, because the cancel is what somebody reaches for
// when something is already going wrong.
func TestScheduleDue_ATerminalParentEmitsNothing(t *testing.T) {
	// Well past the end of the window: every one of the 6 slices is due, and
	// none has been sent. The maximum possible leak.
	after := windowEnd.Add(time.Hour)

	live := parent("o1")
	if got, err := Due(live, none, after); err != nil || len(got) != 6 {
		t.Fatalf("precondition: a LIVE parent yields %d children (err %v), want 6 — if this is "+
			"wrong the cancellation assertion below proves nothing", len(got), err)
	}

	cancelled := parent("o1")
	cancelled.Terminal = true

	got, err := Due(cancelled, none, after)
	if err != nil {
		t.Fatalf("Due on a terminal parent: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a TERMINAL parent yielded %d children %v — every one of these would be placed "+
			"at a venue for an order an operator has already cancelled, which is the worst thing "+
			"this feature can do: a cancel is what somebody reaches for when something is already "+
			"going wrong", len(got), got)
	}
}

// A PARENT CANCELLED MID-SCHEDULE STOPS AT THE CHILDREN ALREADY SENT.
//
// The realistic shape of a cancel: three slices are out at a venue, an operator
// pulls the parent, and the remaining three must never be created. Those three
// already sent are cancelled through the ordinary per-order cancel path — this
// decision's job is only that no FOURTH one appears.
func TestScheduleDue_CancellingMidScheduleEmitsNoFurtherChildren(t *testing.T) {
	sent := setOf(ChildID("o1", 0), ChildID("o1", 1), ChildID("o1", 2))
	// 14:35 — slices 0,1,2 are sent, and slice 3 (due 14:30) is now due.
	at := windowStart.Add(35 * time.Minute)

	live := parent("o1")
	got, err := Due(live, sent, at)
	if err != nil || len(got) != 1 || got[0].Index != 3 {
		t.Fatalf("precondition: a live parent mid-schedule yields %v (err %v), want exactly "+
			"slice 3", got, err)
	}

	cancelled := parent("o1")
	cancelled.Terminal = true
	if got, _ := Due(cancelled, sent, at); len(got) != 0 {
		t.Fatalf("a parent cancelled with 3 of 6 slices working still yielded %v — the operator "+
			"pulled this order and the platform kept trading it", got)
	}
}

// ===== IDEMPOTENCE: THE RESTART STORY =====

// A CHILD THAT ALREADY EXISTS IS NOT CREATED AGAIN.
//
// This is what makes the driver safe to run on every tick, on every pod, without
// a cursor. Without it, a second pod — or the same pod one tick later — re-sends
// every child that has ever been due, and the parent trades its whole quantity
// again on every tick.
func TestScheduleDue_AlreadySentChildrenAreNotResent(t *testing.T) {
	at := windowEnd.Add(time.Hour) // all 6 due
	all := []string{}
	for i := range 6 {
		all = append(all, ChildID("o1", i))
	}

	got, err := Due(parent("o1"), setOf(all...), at)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a fully-sent parent yielded %d more children %v — the driver would re-trade the "+
			"whole parent quantity on every tick", len(got), got)
	}
}

// A HOLE IS REFILLED, NOT SKIPPED.
//
// A crash between two creations leaves slice 2 missing while 3 and 4 exist. A
// cursor ("next slice to send") would have moved past it and the parent would
// never complete: it rests part-filled forever with nothing left to send and no
// error anywhere. Asking which children EXIST, rather than counting how many were
// sent, is what makes the hole visible.
func TestScheduleDue_AMissingChildIsRefilledNotSkipped(t *testing.T) {
	at := windowEnd.Add(time.Hour)
	// 2 is missing; 0, 1, 3, 4 exist; 5 was never reached.
	got, err := Due(parent("o1"), setOf(
		ChildID("o1", 0), ChildID("o1", 1), ChildID("o1", 3), ChildID("o1", 4)), at)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(got) != 2 || got[0].Index != 2 || got[1].Index != 5 {
		t.Fatalf("got %v, want slices 2 and 5 — a cursor would have moved past the hole at 2 and "+
			"the parent would rest part-filled forever with nothing left to send", got)
	}
	// ORDERED, so a driver that stops on the first failure has sent a PREFIX.
	if got[0].Index > got[1].Index {
		t.Errorf("children came back out of index order: %v", got)
	}
}

// TWO PODS REACH THE SAME ANSWER. The property the whole design rests on: no
// cursor means no per-pod state, so a pod that just booted decides exactly what
// the pod it replaced would have decided.
func TestScheduleDue_TwoPodsDecideIdentically(t *testing.T) {
	at := windowStart.Add(25 * time.Minute)
	sent := setOf(ChildID("o1", 0))

	a, err := Due(parent("o1"), sent, at)
	if err != nil {
		t.Fatalf("pod A: %v", err)
	}
	b, err := Due(parent("o1"), sent, at)
	if err != nil {
		t.Fatalf("pod B: %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("two pods decided differently: %d vs %d children", len(a), len(b))
	}
	for i := range a {
		if a[i].OrderID != b[i].OrderID || a[i].Quantity.Cmp(b[i].Quantity) != 0 {
			t.Fatalf("child %d differs between pods: %+v vs %+v — a restarted pod cannot know "+
				"what the pod it replaced intended to send", i, a[i], b[i])
		}
	}
}

// ===== THE DERIVED CHILD ID =====

// A CHILD ID IS DERIVED, AND THE DERIVATION IS THE IDEMPOTENCY MECHANISM.
//
// Two pods that both decide slice 7 is due must compute the SAME id, so the
// store's primary key refuses the second. A random or allocated ID would make
// every retry a new order — and a retry that double-trades is precisely what
// #292 and the PENDING_NEW sweep exist to prevent.
func TestScheduleChildID_IsStableAndDistinctPerSlice(t *testing.T) {
	const parentID = "3f9a1c2e0b7d4e119a6f2c8d5e4b7a13" // 32 hex, as the estate mints

	// TWO SEPARATE CALLS, held in variables: staticcheck reads `f(x) != f(x)`
	// written inline as a mistake, and it is right to — the determinism being
	// asserted here is a property of ChildID, not of the expression.
	first, second := ChildID(parentID, 7), ChildID(parentID, 7)
	if first != second {
		t.Fatalf("ChildID is not stable for the same inputs (%q then %q) — the store's primary "+
			"key cannot deduplicate what two pods spell differently", first, second)
	}
	if ChildID(parentID, 7) == ChildID(parentID, 8) {
		t.Fatal("two different slices derived the SAME child id — the store would merge them " +
			"and the parent would silently under-trade")
	}
	if ChildID("other", 7) == ChildID(parentID, 7) {
		t.Fatal("two different parents derived the same child id for the same slice")
	}
}

// A CHILD ID CAN ACTUALLY BE PLACED AT A VENUE.
//
// This is the assertion the readable form could never have passed. A child is an
// ORDER, so its id is stamped as the exchange's client order id — and OKX accepts
// at most 32 characters, letters and digits only. "<parent>:<index>" broke both
// rules at once: the colon is refused outright, and a 32-character parent leaves
// no room for a suffix. Every scheduled child order would have been admitted,
// stored, announced, and then refused by the exchange.
func TestScheduleChildID_IsPlaceableAtEveryVenue(t *testing.T) {
	for _, parentID := range []string{
		"3f9a1c2e0b7d4e119a6f2c8d5e4b7a13", // the full 32 the estate mints
		"a",                                // and a short one
	} {
		for _, i := range []int{0, 7, 999} {
			id := ChildID(parentID, i)
			if err := orderid.Valid(id); err != nil {
				t.Errorf("ChildID(%q, %d) = %q is not placeable: %v", parentID, i, id, err)
			}
		}
	}
}

// A PARENT WHOSE OWN ID IS UNPLACEABLE IS REFUSED, by the platform-wide rule
// rather than a local one.
func TestScheduleUsableParentID_AppliesThePlatformRule(t *testing.T) {
	for _, id := range []string{
		"3f9a1c2e0b7d4e119a6f2c8d5e4b7a13", // 32 hex, as the gateway now mints
		"clientorder42",
	} {
		if err := UsableParentID(id); err != nil {
			t.Errorf("UsableParentID(%q) = %v, want nil — this is an id the estate really issues", id, err)
		}
	}
	for _, id := range []string{
		"",                                     // no id at all
		"3f9a1c2e-0b7d-4e11-9a6f-2c8d5e4b7a13", // what api-gateway used to mint
		"acct:1",                               // a client-supplied id with punctuation
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",    // 33 characters
	} {
		if err := UsableParentID(id); !errors.Is(err, ErrUnusableParentID) {
			t.Errorf("UsableParentID(%q) = %v, want ErrUnusableParentID", id, err)
		}
	}
}

// ===== THE SCHEDULE ITSELF =====

// ONLY WHAT IS DUE. A driver must not run ahead of the schedule: the whole point
// of working an order over a window is that the quantity reaches the market
// spread out over that window.
func TestScheduleDue_EmitsOnlyWhatTheClockHasReached(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
		want int
	}{
		{"before the window opens", windowStart.Add(-time.Minute), 0},
		{"exactly at the open", windowStart, 1},
		{"25 minutes in", windowStart.Add(25 * time.Minute), 3},
		{"at the close", windowEnd, 6},
		{"long after", windowEnd.Add(24 * time.Hour), 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Due(parent("o1"), none, tt.at)
			if err != nil {
				t.Fatalf("Due: %v", err)
			}
			if len(got) != tt.want {
				t.Fatalf("got %d children, want %d", len(got), tt.want)
			}
		})
	}
}

// THE CHILDREN CARRY THE SCHEDULE'S QUANTITIES, and they still sum to the parent.
// The conservation property is proved exactly in internal/execution/algo; this
// asserts it SURVIVES the trip through this layer, which is where a rounding or
// a re-derivation would quietly reintroduce the loss.
func TestScheduleDue_ChildrenStillSumToTheParent(t *testing.T) {
	p := parent("o1")
	p.Plan.Total = new(big.Rat).SetInt64(10)
	p.Plan.Slices = 3 // 10/3 — the division that does not close

	got, err := Due(p, none, windowEnd.Add(time.Hour))
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	sum := new(big.Rat)
	for _, c := range got {
		sum.Add(sum, c.Quantity)
	}
	if sum.Cmp(new(big.Rat).SetInt64(10)) != 0 {
		t.Fatalf("children sum to %s, want exactly 10 — conservation held in the scheduler and "+
			"was lost on the way out of it", sum.FloatString(20))
	}
}

// THE CHILD RECORDS WHEN IT WAS DUE, not when the driver got round to it. The two
// differ by up to one tick, and only the scheduled time shows that a child went
// out late.
func TestScheduleDue_ChildCarriesItsScheduledTimeNotTheTickTime(t *testing.T) {
	// Ticking at 14:47 — slice 4 was due at 14:40, seven minutes ago.
	got, err := Due(parent("o1"), setOf(
		ChildID("o1", 0), ChildID("o1", 1), ChildID("o1", 2), ChildID("o1", 3)),
		windowStart.Add(47*time.Minute))
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d children, want 1", len(got))
	}
	want := windowStart.Add(40 * time.Minute)
	if !got[0].Due.Equal(want) {
		t.Errorf("child Due = %s, want its SCHEDULED time %s — recording the tick time instead "+
			"hides that this slice went out seven minutes late", got[0].Due, want)
	}
	if got[0].ParentID != "o1" {
		t.Errorf("child ParentID = %q, want o1 — the relation must be durable in both "+
			"directions, not only recoverable by parsing the id", got[0].ParentID)
	}
}

// ===== REFUSALS =====

// A PARENT RESTING WITH NO SCHEDULE IS AN ERROR, NOT SILENCE.
//
// The driver selects parents BY their resting status, so one without a schedule
// means the status and the schedule have diverged in the store. Emitting nothing
// would leave that parent resting forever with nobody told — "nothing configured"
// and "checked, and fine" looking identical, which is the one thing this platform
// refuses to allow.
func TestScheduleDue_AParentWithNoScheduleIsRefusedLoudly(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    Parent
	}{
		{"no quantity", Parent{OrderID: "o1", Plan: algo.Plan{Slices: 6, Start: windowStart, End: windowEnd}}},
		{"no slices", Parent{OrderID: "o1", Plan: algo.Plan{Total: new(big.Rat).SetInt64(60), Start: windowStart, End: windowEnd}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Due(tt.p, none, windowEnd)
			if !errors.Is(err, ErrNoSchedule) {
				t.Fatalf("err = %v, want ErrNoSchedule — a parent resting with no schedule would "+
					"otherwise rest forever with nobody told", err)
			}
			if got != nil {
				t.Errorf("a refused parent yielded %v", got)
			}
		})
	}
}

// The two ID tests that stood here are gone, and what replaced them is in the
// ID section above (TestScheduleUsableParentID_AppliesThePlatformRule).
//
// They asserted the OPPOSITE of what is now true, and they were not wrong when
// written — they encoded a belief that turned out to be false. One held that a
// hyphenated UUID and "client-order-42" were ids "the estate really issues" and
// must be accepted; both are now REFUSED, because OKX accepts neither and every
// order carrying one was unplaceable there. The other refused an id containing
// the child separator, which stopped mattering when child ids became hashes.
//
// Left as a note rather than silently deleted: a test that has to be inverted is
// evidence about the belief it was protecting, and that is worth more than the
// diff hiding it.

// AN UNWORKABLE SCHEDULE SURFACES THE SCHEDULER'S OWN REFUSAL rather than being
// swallowed here. A parent whose cap and slice count disagree must reach an
// operator with the reason, not rest silently.
func TestScheduleDue_AnUnworkableScheduleSurfacesTheReason(t *testing.T) {
	p := parent("o1")
	p.Plan.Total = new(big.Rat).SetInt64(1000)
	p.Plan.Slices = 10
	p.Plan.MaxSlice = new(big.Rat).SetInt64(10) // 100 per child, over the cap

	_, err := Due(p, none, windowEnd)
	if !errors.Is(err, algo.ErrCapUnsatisfiable) {
		t.Fatalf("err = %v, want the scheduler's ErrCapUnsatisfiable to survive this layer — an "+
			"operator needs the reason, not a parent that rests and says nothing", err)
	}
}

// ===== COMPLETE =====

// COMPLETE MEANS EVERY CHILD WAS SENT — NOT THAT THE PARENT IS DONE.
//
// Conflating the two would retire a parent while its last slice was still working
// at a venue, and the fills arriving afterwards would fold onto an order the
// platform had stopped watching.
func TestScheduleComplete_MeansEveryChildExists(t *testing.T) {
	p := parent("o1")
	all := []string{}
	for i := range 6 {
		all = append(all, ChildID("o1", i))
	}

	if Complete(p, setOf(all[:5]...)) {
		t.Error("Complete with 5 of 6 children — the sixth slice would never be sent")
	}
	if !Complete(p, setOf(all...)) {
		t.Error("Complete false with every child created")
	}
	// A HOLE IS NOT COMPLETE, even though the LAST child exists. A count would
	// say 5-of-6 either way; only checking each index sees which one is missing.
	if Complete(p, setOf(all[0], all[1], all[3], all[4], all[5])) {
		t.Error("Complete true with slice 2 missing — a count of children cannot tell a hole " +
			"from a short tail, and the parent would be retired having never sent slice 2")
	}
}
