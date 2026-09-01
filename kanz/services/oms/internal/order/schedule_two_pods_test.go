package order

import (
	"context"
	"math/big"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/marketedge/volprofile"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/volprofilefeed"
	"github.com/eighred/kanz/services/oms/internal/schedule"
)

// TWO PODS, ONE PARENT, ONE SCHEDULE (#897).
//
// # The failure this file exists against
//
// A parent order's schedule is derived FOUR times over its life — at admission,
// at every inbound child's admission, on every driver tick, and again after any
// restart — and each of those may happen on a different pod. authorizeChild
// re-derives the whole schedule and compares the inbound child's quantity as an
// EXACT RATIONAL, so any difference in any digit refuses a legitimate slice under
// NOT_A_SLICE or SLICE_QUANTITY_MISMATCH.
//
// For TWAP that is free: every input is a durable field of the order. For a
// volume-driven schedule there is a fifth input the order does not carry and the
// client never sent — the shape of the market — and it MOVES. A session
// completes, the market-data edge publishes a new profile, and a pod reading "the
// current curve" derives a different parent from the same order. On two pods it
// is worse still: their registries need not even be at the same version.
//
// # What is actually asserted below
//
// The two services are built INDEPENDENTLY: separate constructions, separate
// registries, separate views of the feed. They share exactly what two real pods
// share and nothing else — the order store, which is the database. Then the
// market MOVES underneath the second one, which is the case a test that simply
// gave both pods the same registry would miss entirely.

// twoPods builds two independently-constructed services over one store.
//
// SEPARATE REGISTRIES ON PURPOSE. Handing both pods one registry would prove that
// a schedule is a function of a curve, which nobody doubts; what has to be proved
// is that it is a function of the curve the ORDER NAMES, which only shows when
// the two registries hold different things.
func twoPods(t *testing.T, store *MemoryStore, at *time.Time,
	regA, regB *volprofilefeed.Registry) (*Service, *Service) {
	t.Helper()
	build := func(reg *volprofilefeed.Registry) *Service {
		router := execution.NewRouter([]execution.Venue{execution.NewSimVenue(profileVenue)})
		svc, err := NewService(testTenant, store, NewEmitter(&fakeBus{}), nil, router, nil, nil,
			WithHaltGate(halt.OpenGate(nil)), WithVolumeProfiles(reg))
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		svc.now = func() time.Time { return *at }
		return svc
	}
	return build(regA), build(regB)
}

// A SECOND POD THAT NEVER SAW THE FIRST DERIVES THE IDENTICAL SCHEDULE — even
// after the market has moved under it.
//
// This is #897's decision 3 as an assertion. Pod A admits the parent and pins the
// curve that was current then. Pod B holds that curve AND a newer one, so if it
// derived against "current" it would produce different children — and it must not.
func TestScheduleE2E_ASecondPodDerivesTheIdenticalSchedule(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	store := NewMemoryStore()

	// POD A's VIEW OF THE FEED: one published curve, 1/4 early and 3/4 at noon.
	regA := volprofilefeed.NewRegistry(0)
	pinned := publishedCurve(t, regA, profileDay, 100, 400)

	// POD B's VIEW: the same curve, and then a LATER one with a different shape —
	// 3/4 early and 1/4 at noon. Two independent folds; nothing is shared.
	regB := volprofilefeed.NewRegistry(0)
	if v := publishedCurve(t, regB, profileDay, 100, 400); v != pinned {
		t.Fatalf("two independent folds of one curve produced versions %q and %q — a pin taken "+
			"on one pod would be unresolvable on the other", pinned, v)
	}
	moved := publishedCurve(t, regB, profileDay.Add(volprofile.Session), 300, 400)
	if moved == pinned {
		t.Fatal("the fixture's second curve is the same version as the first, so this test cannot " +
			"distinguish a pinned derivation from a current one")
	}

	podA, podB := twoPods(t, store, &at, regA, regB)

	// POD A ADMITS.
	if err := podA.Handle(testCtx(), submitEnv(),
		mustMarshal(t, vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_VWAP, profileVenue))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	st, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("pod A did not admit the parent: %v", err)
	}
	if got := st.GetExecutionSchedule().GetVolumeProfileVersion(); got != pinned {
		t.Fatalf("pinned version = %q, want %q", got, pinned)
	}

	// POD B DRIVES. Its registry's NEWEST curve is the moved one, so a driver
	// reading "current" would work 45 then 15 instead of 15 then 45.
	at = profileDay.Add(volprofile.Session)
	n, err := podB.DriveSchedules(driveCtx())
	if err != nil {
		t.Fatalf("pod B could not advance a parent pod A admitted: %v", err)
	}
	if n != 2 {
		t.Fatalf("pod B created %d children, want 2", n)
	}

	children, err := store.ListByParent(context.Background(), "p1")
	if err != nil {
		t.Fatalf("ListByParent: %v", err)
	}
	want := map[string]int64{
		schedule.ChildID("p1", 0): 15, // 1/4 of a 60-unit parent, from the PINNED curve
		schedule.ChildID("p1", 1): 45,
	}
	for _, c := range children {
		w, ok := want[c.GetOrderId()]
		if !ok {
			t.Fatalf("unexpected child %s", c.GetOrderId())
		}
		got := dec.FromProto(c.GetOrderedQuantity())
		if got.Cmp(new(big.Rat).SetInt64(w)) != 0 {
			t.Fatalf("pod B worked child %s as %s, want %d — it derived the parent against the "+
				"curve that is CURRENT on pod B rather than the one the ORDER names, so two pods "+
				"working one parent disagree and authorizeChild refuses the legitimate slice",
				c.GetOrderId(), got.RatString(), w)
		}
	}

	// AND POD A AUTHORIZES POD B's CHILD. This is the check that actually breaks
	// in production when the two disagree: the quantity comparison is an exact
	// rational, so a child sized against a different curve is refused as a forgery.
	child := findChild(t, children, schedule.ChildID("p1", 0))
	cmd := submitFromState(child, nil)
	cmd.OrderId = child.GetOrderId()
	cmd.ParentOrderId = "p1"
	cmd.Quantity = child.GetOrderedQuantity()
	cmd.ExecutionSchedule = nil
	if _, rej, err := podA.authorizeChild(context.Background(), cmd); err != nil || rej != nil {
		t.Fatalf("pod A refused a child pod B derived from the same order: rej=%v err=%v", rej, err)
	}
}

// A PIN THIS POD CANNOT RESOLVE STOPS THE PARENT, LOUDLY — it does not re-plan it.
//
// The registry retains versions for a horizon and the MARKET stream retains 24h,
// so a pin can genuinely go unresolvable: a parent worked across more than a day
// on a pod that then rolled. The fail-closed answer is the only safe one — the
// alternative keeps the order working against a curve nobody chose, and every
// fill is attributed to a schedule that was never derived.
func TestScheduleE2E_AnUnresolvablePinStopsTheParentRatherThanRePlanningIt(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	store := NewMemoryStore()

	regA := volprofilefeed.NewRegistry(0)
	publishedCurve(t, regA, profileDay, 100, 400)

	// POD B HOLDS A DIFFERENT CURVE AND NOT THE PINNED ONE — a pod that booted
	// after the pinned version aged off the stream.
	regB := volprofilefeed.NewRegistry(0)
	publishedCurve(t, regB, profileDay.Add(volprofile.Session), 300, 400)

	podA, podB := twoPods(t, store, &at, regA, regB)
	if err := podA.Handle(testCtx(), submitEnv(),
		mustMarshal(t, vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_VWAP, profileVenue))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	at = profileDay.Add(volprofile.Session)
	n, err := podB.DriveSchedules(driveCtx())
	if err == nil {
		t.Fatal("pod B advanced a parent whose pinned curve it does not hold — it re-planned a " +
			"working order against a market it was never sized for, and nothing would say so")
	}
	if n != 0 {
		t.Fatalf("pod B created %d children from an unresolvable pin", n)
	}
	children, lerr := store.ListByParent(context.Background(), "p1")
	if lerr != nil {
		t.Fatalf("ListByParent: %v", lerr)
	}
	if len(children) != 0 {
		t.Fatalf("children = %d, want none", len(children))
	}
}

func findChild(t *testing.T, children []*orderpb.OrderState, id string) *orderpb.OrderState {
	t.Helper()
	for _, c := range children {
		if c.GetOrderId() == id {
			return c
		}
	}
	t.Fatalf("no child %s among %d", id, len(children))
	return nil
}
