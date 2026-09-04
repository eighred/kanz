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
// so a pin can genuinely go unresolvable. The fail-closed answer is the only safe
// one — the alternative keeps the order working against a curve nobody chose, and
// every fill is attributed to a schedule that was never derived.
//
// # WHICH ORDERS CAN STILL REACH THIS STATE, after #943
//
// Only the ones admitted BEFORE #943: they carry a version and no curve, so the
// registry is the only thing that can answer for them and a cold pod's registry
// cannot. That population shrinks to nothing as those parents complete, and until
// it does its behaviour must not change — which is why this test did not move.
//
// The fixture therefore strips the durable curve from the stored order rather than
// pretending admission never wrote one. Writing the test any other way would prove
// the fallback is unreachable instead of proving it is safe.
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
	agedToPre943(t, store, "p1")

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

// agedToPre943 rewrites a stored parent into one admitted before the schedule
// carried its own curve: a version, and nothing to resolve it with locally.
//
// IT IS A FIXTURE FOR A REAL POPULATION, not a way to keep an old test green. Every
// VWAP and POV parent working when #943 deploys is in exactly this shape, and the
// registry fallback exists for them and for nothing else.
func agedToPre943(t *testing.T, store *MemoryStore, orderID string) {
	t.Helper()
	st, ver, err := store.Load(context.Background(), orderID)
	if err != nil {
		t.Fatalf("Load %s: %v", orderID, err)
	}
	if st.GetExecutionSchedule().GetVolumeProfile() == nil {
		t.Fatalf("%s was admitted without a durable curve, so this fixture is not ageing "+
			"anything — admission has stopped stamping it and #943's gap is back", orderID)
	}
	st.ExecutionSchedule.VolumeProfile = nil
	if err := store.Save(context.Background(), st, ver, nil, ""); err != nil {
		t.Fatalf("Save %s: %v", orderID, err)
	}
}

// A POD THAT NEVER RECEIVED THE PROFILE AT ALL STILL DERIVES THE PARENT (#943).
//
// # The state this is written against
//
// #897 stamped the profile VERSION on the parent and left the curve it names on
// the bus. A pod resolves the version by replaying market.crypto.volume_profile
// into its registry, and the MARKET stream retains 24h
// (infra/nats/bootstrap-job.yaml) while the registry retains seven days. So the
// pin was resolvable for a week on the pod that admitted the order and for a day
// on the pod that replaced it: a parent worked over a longer window, on a pod that
// then rolled, stopped advancing and needed a human to cancel and re-submit it.
//
// # Why the registry here is EMPTY rather than merely stale
//
// Because an empty one is the strictly harder case and it is the one that
// actually happens. A pod that boots after the version has aged off the stream
// replays a history that does not contain it, and if the series has been quiet it
// replays nothing at all. A fixture holding "some other curve" would still pass if
// the code fell back to a nearest match; a fixture holding NOTHING can only pass if
// the schedule was derived from the order itself.
func TestScheduleE2E_APodThatNeverReceivedTheProfileStillDerivesTheParent(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	store := NewMemoryStore()

	regA := volprofilefeed.NewRegistry(0)
	pinned := publishedCurve(t, regA, profileDay, 100, 400)

	// POD B'S FEED IS EMPTY. Nothing folded, no series, no versions — a pod that
	// booted after the profile aged off the MARKET stream.
	regB := volprofilefeed.NewRegistry(0)
	if got := regB.Versions(profileSeries()); got != 0 {
		t.Fatalf("pod B's registry holds %d version(s); the fixture is not testing a cold pod", got)
	}

	podA, podB := twoPods(t, store, &at, regA, regB)
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

	at = profileDay.Add(volprofile.Session)
	n, err := podB.DriveSchedules(driveCtx())
	if err != nil {
		t.Fatalf("a pod holding NO volume profile could not advance a parent that carries its "+
			"own curve: %v — the schedule is still a function of what this pod happened to "+
			"replay, so a deploy during a long working window strands the parent", err)
	}
	if n != 2 {
		t.Fatalf("pod B created %d children, want 2", n)
	}

	children, err := store.ListByParent(context.Background(), "p1")
	if err != nil {
		t.Fatalf("ListByParent: %v", err)
	}
	// THE SHAPE MUST BE THE PINNED ONE, not a flat one. 15/45 is the measured
	// curve; 30/30 would be TWAP wearing VWAP's name, which is the degradation
	// #869 refuses and which an "it advanced" assertion alone would not catch.
	want := map[string]int64{schedule.ChildID("p1", 0): 15, schedule.ChildID("p1", 1): 45}
	for _, c := range children {
		w, ok := want[c.GetOrderId()]
		if !ok {
			t.Fatalf("unexpected child %s", c.GetOrderId())
		}
		if got := dec.FromProto(c.GetOrderedQuantity()); got.Cmp(new(big.Rat).SetInt64(w)) != 0 {
			t.Fatalf("child %s = %s, want %d — the cold pod advanced the parent against something "+
				"other than the curve the order carries", c.GetOrderId(), got.RatString(), w)
		}
	}

	// AND POD A AUTHORIZES IT. The exact-rational comparison is what actually
	// breaks in production when two pods disagree.
	child := findChild(t, children, schedule.ChildID("p1", 0))
	cmd := submitFromState(child, nil)
	cmd.OrderId = child.GetOrderId()
	cmd.ParentOrderId = "p1"
	cmd.Quantity = child.GetOrderedQuantity()
	cmd.ExecutionSchedule = nil
	if _, rej, err := podA.authorizeChild(context.Background(), cmd); err != nil || rej != nil {
		t.Fatalf("pod A refused a child the cold pod derived: rej=%v err=%v", rej, err)
	}
}

// A STORED CURVE THAT DOES NOT HASH TO ITS PIN STOPS THE PARENT (#943).
//
// # Why this is the sharp edge of storing the curve
//
// The version is a hash of the curve's own content, so the two fields on the
// order are checkable against each other. If they are ever NOT checked, the
// durable curve becomes the one place in the system where a schedule can be
// derived from a shape nobody published while the audit record names a version
// that was — the exact mislabelling the pin was bought to prevent, arriving from
// inside the order instead of from the feed.
//
// # AND IT MUST NOT FALL BACK
//
// Pod B here holds the pinned version in its registry, so a fall-through would
// SUCCEED and produce perfectly correct children. That is the failure being
// excluded: a corrupt blob quietly papered over by a good registry answer is a
// corruption no log would ever mention, and on the next pod — one whose registry
// has aged — the same order would suddenly stop. "Present and wrong" is a refusal,
// not a miss.
func TestScheduleE2E_AStoredCurveThatDoesNotHashToItsPinStopsTheParent(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	store := NewMemoryStore()

	regA := volprofilefeed.NewRegistry(0)
	publishedCurve(t, regA, profileDay, 100, 400)

	// BOTH PODS CAN RESOLVE THE PIN FROM THE FEED. Only the stored curve is wrong.
	regB := volprofilefeed.NewRegistry(0)
	publishedCurve(t, regB, profileDay, 100, 400)

	podA, podB := twoPods(t, store, &at, regA, regB)
	if err := podA.Handle(testCtx(), submitEnv(),
		mustMarshal(t, vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_VWAP, profileVenue))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	st, ver, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	curve := st.GetExecutionSchedule().GetVolumeProfile()
	if len(curve.GetExpectedVolume()) == 0 {
		t.Fatal("the admitted parent carries no curve to corrupt — admission has stopped " +
			"stamping it and this test can prove nothing")
	}
	// ONE BUCKET, DOUBLED: a curve that is still well-formed, still decodes, and
	// still claims the pinned version. Only the hash disagrees.
	curve.ExpectedVolume[0].Coefficient *= 2
	if err := store.Save(context.Background(), st, ver, nil, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	at = profileDay.Add(volprofile.Session)
	n, err := podB.DriveSchedules(driveCtx())
	if err == nil {
		t.Fatal("pod B advanced a parent whose stored curve does not hash to the version beside " +
			"it — either the curve is never checked against its pin, or a bad one falls through " +
			"to the registry. Both mean an order can be worked against a shape nobody published " +
			"while its audit record names one that was")
	}
	if n != 0 {
		t.Fatalf("children = %d from a curve that failed its own version check, want 0", n)
	}
	children, lerr := store.ListByParent(context.Background(), "p1")
	if lerr != nil {
		t.Fatalf("ListByParent: %v", lerr)
	}
	if len(children) != 0 {
		t.Fatalf("children = %d, want none", len(children))
	}
}

// A STORED CURVE SUBSTITUTED FOR ANOTHER PUBLISHED ONE STOPS THE PARENT (#943).
//
// # Why this is a SEPARATE test from the corrupted one above, and not a variant
//
// Because the two are stopped by different code, and a mutation proved it. The
// test above rewrites a bucket, which makes the message stop describing its own
// version — volprofilefeed.Decode recomputes that hash and refuses it, so deleting
// the OMS's own comparison of the curve's version against the ORDER's pin changed
// nothing and the mutation survived.
//
// SUBSTITUTION IS THE CASE THAT COMPARISON EXISTS FOR, and it is the realistic
// one. The curve here is genuine: published, well-formed, and it hashes to its own
// content perfectly — it is simply not the curve this parent was sized against. A
// bad write, a mis-keyed restore, or a replay that put yesterday's session behind
// today's pin all produce exactly this, and every check that reads the message
// alone passes it. Only the ORDER knows which version it chose.
//
// The consequence of missing it is the one the pin exists to prevent, arriving
// from inside the order: the parent works against a real market it was never sized
// for, and every fill is attributed to a version that was never derived.
func TestScheduleE2E_AStoredCurveOfAnotherVersionStopsTheParent(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	store := NewMemoryStore()

	regA := volprofilefeed.NewRegistry(0)
	pinned := publishedCurve(t, regA, profileDay, 100, 400)

	// A SECOND, GENUINELY PUBLISHED CURVE — a later session with a different shape.
	// Nothing about this message is malformed.
	other := volprofilefeed.NewRegistry(0)
	otherVersion := publishedCurve(t, other, profileDay.Add(volprofile.Session), 300, 400)
	if otherVersion == pinned {
		t.Fatal("the fixture's two curves share a version, so substituting one for the other " +
			"cannot be detected and this test proves nothing")
	}
	otherProfile, ok := other.Current(profileSeries())
	if !ok {
		t.Fatal("the fixture did not fold its second curve")
	}

	// BOTH PODS CAN RESOLVE THE REAL PIN FROM THE FEED, so a fall-through to the
	// registry would produce perfectly correct children and hide the substitution.
	regB := volprofilefeed.NewRegistry(0)
	publishedCurve(t, regB, profileDay, 100, 400)

	podA, podB := twoPods(t, store, &at, regA, regB)
	if err := podA.Handle(testCtx(), submitEnv(),
		mustMarshal(t, vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_VWAP, profileVenue))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	st, ver, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.GetExecutionSchedule().GetVolumeProfileVersion(); got != pinned {
		t.Fatalf("pinned version = %q, want %q", got, pinned)
	}
	// THE PIN IS LEFT ALONE AND ONLY THE CURVE IS SWAPPED. The order still says
	// which version it chose; what sits beside it is a different one.
	st.ExecutionSchedule.VolumeProfile = otherProfile.Wire
	if err := store.Save(context.Background(), st, ver, nil, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	at = profileDay.Add(volprofile.Session)
	n, err := podB.DriveSchedules(driveCtx())
	if err == nil {
		t.Fatalf("pod B advanced a parent whose stored curve is version %q while the order pins "+
			"%q — the curve is never compared against the pin, so a well-formed message that "+
			"describes a DIFFERENT market is scheduled against and every fill is attributed to "+
			"a version that was never derived", otherVersion, pinned)
	}
	if n != 0 {
		t.Fatalf("children = %d from a substituted curve, want 0", n)
	}
}

// profileSeries is the (instrument, venue) the fixtures publish on.
func profileSeries() volprofilefeed.Series {
	return volprofilefeed.Series{InstrumentID: "BTC-USD", Venue: profileVenue}
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
