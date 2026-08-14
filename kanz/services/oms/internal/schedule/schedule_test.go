package schedule

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution/algo"
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
func parent(id string) Parent {
	return Parent{
		OrderID: id,
		Plan: algo.Plan{
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

	live := parent("o-1")
	if got, err := Due(live, none, after); err != nil || len(got) != 6 {
		t.Fatalf("precondition: a LIVE parent yields %d children (err %v), want 6 — if this is "+
			"wrong the cancellation assertion below proves nothing", len(got), err)
	}

	cancelled := parent("o-1")
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
	sent := setOf(ChildID("o-1", 0), ChildID("o-1", 1), ChildID("o-1", 2))
	// 14:35 — slices 0,1,2 are sent, and slice 3 (due 14:30) is now due.
	at := windowStart.Add(35 * time.Minute)

	live := parent("o-1")
	got, err := Due(live, sent, at)
	if err != nil || len(got) != 1 || got[0].Index != 3 {
		t.Fatalf("precondition: a live parent mid-schedule yields %v (err %v), want exactly "+
			"slice 3", got, err)
	}

	cancelled := parent("o-1")
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
		all = append(all, ChildID("o-1", i))
	}

	got, err := Due(parent("o-1"), setOf(all...), at)
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
	got, err := Due(parent("o-1"), setOf(
		ChildID("o-1", 0), ChildID("o-1", 1), ChildID("o-1", 3), ChildID("o-1", 4)), at)
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
	sent := setOf(ChildID("o-1", 0))

	a, err := Due(parent("o-1"), sent, at)
	if err != nil {
		t.Fatalf("pod A: %v", err)
	}
	b, err := Due(parent("o-1"), sent, at)
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
func TestScheduleChildID_IsStableAndReversible(t *testing.T) {
	const parentID = "3f9a1c2e-0b7d-4e11-9a6f-2c8d5e4b7a13" // a UUID, as real order ids are
	id := ChildID(parentID, 7)

	if id != ChildID(parentID, 7) {
		t.Fatal("ChildID is not stable for the same inputs — the store's primary key cannot " +
			"deduplicate what two pods spell differently")
	}
	if id == ChildID(parentID, 8) {
		t.Fatal("two different slices derived the SAME child id — the store would merge them and " +
			"the parent would silently under-trade")
	}
	gotParent, gotIndex, ok := ParseChildID(id)
	if !ok || gotParent != parentID || gotIndex != 7 {
		t.Fatalf("ParseChildID(%q) = (%q, %d, %v), want (%q, 7, true)",
			id, gotParent, gotIndex, ok, parentID)
	}
}

// TWO DIFFERENT PARENTS NEVER DERIVE THE SAME CHILD ID.
//
// The separator has to be a character an order ID cannot contain. If it could,
// parent "a:1" slice 0 and parent "a" slice 1 would collide — two different
// funds' orders merged by the store's primary key, which is a loss of money, not
// of tidiness.
func TestScheduleChildID_DistinctParentsCannotCollide(t *testing.T) {
	if ChildID("a"+childSeparator+"1", 0) == ChildID("a", 1) {
		t.Fatalf("parents %q and %q derived the same child id — order ids must not be able to "+
			"contain %q, or two orders merge at the store's primary key",
			"a"+childSeparator+"1", "a", childSeparator)
	}
}

// A NON-DERIVED ID IS NOT CLAIMED AS A CHILD. An ordinary order must not parse as
// somebody's slice, or an operator tracing a parent would be shown orders that
// have nothing to do with it.
func TestScheduleParseChildID_RejectsIdsItDidNotDerive(t *testing.T) {
	for _, id := range []string{
		"3f9a1c2e-0b7d-4e11-9a6f-2c8d5e4b7a13", // a plain order id
		"",
		":",
		":5",         // no parent
		"parent:",    // no index
		"parent:abc", // not an index
		"parent:-1",  // a negative slice
	} {
		if _, _, ok := ParseChildID(id); ok {
			t.Errorf("ParseChildID(%q) claimed it as a derived child id", id)
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
			got, err := Due(parent("o-1"), none, tt.at)
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
	p := parent("o-1")
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
	got, err := Due(parent("o-1"), setOf(
		ChildID("o-1", 0), ChildID("o-1", 1), ChildID("o-1", 2), ChildID("o-1", 3)),
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
	if got[0].ParentID != "o-1" {
		t.Errorf("child ParentID = %q, want o-1 — the relation must be durable in both "+
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
		{"no quantity", Parent{OrderID: "o-1", Plan: algo.Plan{Slices: 6, Start: windowStart, End: windowEnd}}},
		{"no slices", Parent{OrderID: "o-1", Plan: algo.Plan{Total: new(big.Rat).SetInt64(60), Start: windowStart, End: windowEnd}}},
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

// A PARENT WHOSE ID CANNOT DERIVE UNAMBIGUOUS CHILDREN IS REFUSED.
//
// THE COMMENT THIS REPLACES WAS WRONG, and the error it would have caused is the
// expensive kind. It claimed the separator was safe "because order ids are UUIDs
// and a UUID contains no colon". The gateway only DEFAULTS order_id to a UUID
// when the client leaves it empty — a client may supply any string — so parent
// "a:1" slice 0 and parent "a" slice 1 both spell "a:1:0". That id is the store's
// primary key AND the venue's clientOrderId, so the collision does not raise an
// error anywhere: it merges two orders.
func TestScheduleDue_AParentWhoseIDCannotDeriveChildrenIsRefused(t *testing.T) {
	for _, tt := range []struct{ name, id string }{
		{"no id at all", ""},
		{"a client-supplied id containing the separator", "acct" + childSeparator + "1"},
		{"a child being scheduled as a parent", ChildID("o-1", 3)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Due(parent(tt.id), none, windowEnd); !errors.Is(err, ErrUnusableParentID) {
				t.Fatalf("err = %v, want ErrUnusableParentID — this parent's children could not "+
					"be told apart from another parent's, and the store would MERGE them rather "+
					"than refuse them", err)
			}
		})
	}
}

// AND AN ORDINARY ID IS NOT REFUSED. A rule that rejected everything would be a
// trading outage wearing the shape of a control.
func TestScheduleUsableParentID_AcceptsTheIdsThisEstateActuallyIssues(t *testing.T) {
	for _, id := range []string{
		"3f9a1c2e-0b7d-4e11-9a6f-2c8d5e4b7a13", // api-gateway's uuid.NewString()
		"8d1f0c3b9a2e4d5f6071829304a5b6c7",     // translate.DeterministicID, 32 hex
		"client-order-42",                      // a client-supplied id, no separator
	} {
		if err := UsableParentID(id); err != nil {
			t.Errorf("UsableParentID(%q) = %v, want nil — this is an id the estate really issues", id, err)
		}
	}
}

// AN UNWORKABLE SCHEDULE SURFACES THE SCHEDULER'S OWN REFUSAL rather than being
// swallowed here. A parent whose cap and slice count disagree must reach an
// operator with the reason, not rest silently.
func TestScheduleDue_AnUnworkableScheduleSurfacesTheReason(t *testing.T) {
	p := parent("o-1")
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
	p := parent("o-1")
	all := []string{}
	for i := range 6 {
		all = append(all, ChildID("o-1", i))
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
