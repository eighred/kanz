package order

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/execution/algo"
	"github.com/eighred/kanz/internal/marketedge/volprofile"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/volprofilefeed"
	"github.com/eighred/kanz/services/oms/internal/schedule"
)

// A VOLUME-DRIVEN ORDER IS ADMITTED, AND ITS CURVE IS PINNED (#897).
//
// # What this file used to say
//
// Until #897 every assertion here was a REFUSAL. #869 had landed VWAP and POV in
// internal/execution/algo and #867 the intraday volume profile, and nothing
// carried the profile from market-ingest — where the fold lives — to the OMS. So
// all three of this service's entries into the algo package passed
// algo.UnknownMarket and a VWAP order was refused at admission under
// NO_VOLUME_PROFILE. The file's own header said, in as many words, that it was
// what must change when a profile reached this path.
//
// # What replaced it, and what did NOT
//
// The refusal is still here and it is still the fail-closed direction: a
// volume-driven order this OMS cannot size is refused rather than worked against
// a flat curve, because a flat curve IS TWAP and an order labelled VWAP that TWAP
// worked is a mislabelled execution nothing downstream can detect. What changed
// is that it is no longer the ONLY outcome, and that the refusal now says WHICH of
// four different things is missing.
//
// # The property the rest of this file exists for
//
// A schedule is derived FOUR times over a parent's life — at admission, at each
// inbound child's admission, on every driver tick, and again after any pod
// restart — and authorizeChild compares a child's quantity as an EXACT RATIONAL.
// So all of those derivations must produce identical children, on any pod, at any
// time, while the market underneath them moves. That is what the pinned profile
// version buys, and TestScheduleE2E_ASecondPodDerivesTheIdenticalSchedule is the
// assertion that it is bought.

const profileVenue = "XSIM"

// profiledService is scheduledService plus a bound volume-profile registry.
//
// THE REGISTRY IS THE REAL ONE, NOT A DOUBLE. A stub answering for every key
// would let a curve measured on one series size an order on another — the exact
// failure Pinned.ExpectedVolume refuses and the exact shape a test double hides.
// Folding real encoded messages also runs Decode's version check, so a fixture
// that drifted from the producer's own arithmetic fails here rather than passing
// against itself.
func profiledService(t *testing.T, fb *fakeBus, at *time.Time, reg *volprofilefeed.Registry) (*Service, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	router := execution.NewRouter([]execution.Venue{execution.NewSimVenue(profileVenue)})
	opts := []ServiceOption{WithHaltGate(halt.OpenGate(nil))}
	if reg != nil {
		opts = append(opts, WithVolumeProfiles(reg))
	}
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, router, nil, nil, opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return *at }
	return svc, store
}

// publishedCurve folds one KNOWN profile into a registry the way the bus would,
// and returns its version.
//
// head/level are the quantities expected in the session's FIRST half-hour and in
// the whole session, so the noon bucket carries the remainder. A caller reading a
// child quantity below can evaluate it from these two numbers by hand.
func publishedCurve(t *testing.T, reg *volprofilefeed.Registry, asOf time.Time, head, level int64) string {
	t.Helper()
	n := volprofile.BucketsPerSession(volprofile.DefaultBucket)
	shares := make([]*big.Rat, n)
	for i := range shares {
		shares[i] = new(big.Rat)
	}
	shares[0] = new(big.Rat).SetFrac64(head, level)
	shares[24] = new(big.Rat).SetFrac64(level-head, level)

	pb, err := volprofilefeed.Encode(volprofile.Answer{
		Series:        volprofile.Series{InstrumentID: "BTC-USD", Venue: profileVenue},
		Bucket:        volprofile.DefaultBucket,
		AsOf:          asOf,
		Verdict:       volprofile.VerdictKnown,
		Sessions:      3,
		Oldest:        asOf.Add(-3 * volprofile.Session),
		Newest:        asOf.Add(-volprofile.Session),
		SessionVolume: new(big.Rat).SetInt64(level),
		Shares:        shares,
	}, volprofile.DefaultHorizon)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	prof, err := volprofilefeed.Decode(pb)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	reg.Fold(prof)
	return prof.Version
}

// publishedVerdict folds a profile with no curve — the "measured, and not enough
// of it" state.
func publishedVerdict(t *testing.T, reg *volprofilefeed.Registry, asOf time.Time, v volprofile.Verdict) {
	t.Helper()
	pb, err := volprofilefeed.Encode(volprofile.Answer{
		Series:   volprofile.Series{InstrumentID: "BTC-USD", Venue: profileVenue},
		Bucket:   volprofile.DefaultBucket,
		AsOf:     asOf,
		Verdict:  v,
		Sessions: 2,
	}, volprofile.DefaultHorizon)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	prof, err := volprofilefeed.Decode(pb)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	reg.Fold(prof)
}

// vwapOrder is a scheduled parent worked by VWAP over the whole fixture session
// in two half-day slices, on the venue the profile was measured on.
//
// TWO SLICES OVER A WHOLE SESSION IS WHAT MAKES THE SHAPE VISIBLE. The fixture
// curve puts `head` of `level` in the first half-hour and the rest at noon, so
// slice 0 gets exactly head/level of the parent and slice 1 the remainder — a
// split that is emphatically NOT two equal halves, which is the whole difference
// between VWAP and TWAP.
func vwapOrder(id string, a orderpb.ExecutionAlgo, venue string) *orderpb.SubmitOrder {
	cmd := scheduledOrder(id, 2, nil)
	cmd.Venue = venue
	cmd.ExecutionSchedule.Algo = a
	cmd.ExecutionSchedule.WindowStart = timestamppb.New(profileDay)
	cmd.ExecutionSchedule.WindowEnd = timestamppb.New(profileDay.Add(volprofile.Session))
	return cmd
}

var profileDay = time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

// ===== ADMITTED, AND PINNED =====

// A VWAP ORDER AGAINST A MEASURED PROFILE IS ADMITTED, RESTS, AND RECORDS THE
// CURVE IT WAS PLANNED AGAINST.
//
// This is #897's "verified when", and every clause of it is separately load-
// bearing: admitted (not refused), resting at WORKING_SCHEDULED (not sent to a
// venue whole), children proportional to the profile (not equal, which would be
// TWAP), and a version on the order (without which no later derivation could
// agree with this one).
func TestScheduleE2E_AVolumeDrivenOrderIsAdmittedAndPinsItsCurve(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	fb := &fakeBus{}
	reg := volprofilefeed.NewRegistry(0)
	version := publishedCurve(t, reg, profileDay, 100, 400) // 1/4 early, 3/4 at noon
	svc, store := profiledService(t, fb, &at, reg)

	if err := svc.Handle(testCtx(), submitEnv(),
		mustMarshal(t, vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_VWAP, profileVenue))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	st, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("a VWAP order against a measured profile was NOT admitted: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED {
		t.Fatalf("status = %v, want WORKING_SCHEDULED — a parent is a container for the decision "+
			"and must reach no venue itself", st.GetStatus())
	}
	if got := st.GetExecutionSchedule().GetVolumeProfileVersion(); got != version {
		t.Fatalf("pinned version = %q, want %q — without it every later derivation of this "+
			"parent's schedule would re-plan it against whatever curve was current then", got, version)
	}
	// AND THE CURVE ITSELF IS ON THE ORDER, NOT ONLY ITS NAME (#943). A version
	// alone is a handle into the registry, which a pod rebuilds from a stream that
	// retains a day — so a parent worked over longer than that stopped advancing on
	// the pod that replaced the one that admitted it.
	stored := st.GetExecutionSchedule().GetVolumeProfile()
	if stored == nil {
		t.Fatal("the admitted parent carries a pinned version and NO curve — the schedule's last " +
			"input is still only on the bus, so this order is derivable exactly as long as some " +
			"pod happens to hold the profile in memory")
	}
	if got := stored.GetVersion(); got != version {
		t.Fatalf("the stored curve announces version %q beside a pin of %q — the order names one "+
			"market and carries another, and every fill would be attributed to a schedule that "+
			"was never derived", got, version)
	}
	if _, err := volprofilefeed.Decode(stored); err != nil {
		t.Fatalf("the stored curve does not describe its own version: %v — Decode recomputes the "+
			"hash, so this is a curve no pin can ever resolve", err)
	}

	// THE CHILDREN ARE THE MEASURED SHAPE, NOT TWO EQUAL HALVES. The parent is 60
	// units; the curve puts 1/4 of a session in the first half-hour and 3/4 at
	// noon, so a two-slice VWAP over the whole session works 15 then 45.
	at = profileDay.Add(volprofile.Session)
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}
	children, err := store.ListByParent(context.Background(), "p1")
	if err != nil {
		t.Fatalf("ListByParent: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2", len(children))
	}
	want := map[string]*big.Rat{
		schedule.ChildID("p1", 0): new(big.Rat).SetInt64(15),
		schedule.ChildID("p1", 1): new(big.Rat).SetInt64(45),
	}
	for _, c := range children {
		w, ok := want[c.GetOrderId()]
		if !ok {
			t.Fatalf("unexpected child %s", c.GetOrderId())
		}
		if got := dec.FromProto(c.GetOrderedQuantity()); got.Cmp(w) != 0 {
			t.Errorf("child %s carries %s, want %s — the schedule is not proportional to the "+
				"measured curve, which means it is not VWAP", c.GetOrderId(), got.RatString(), w.RatString())
		}
	}
}

// A TWAP ORDER PINS NOTHING, EVEN WITH A PROFILE AVAILABLE.
//
// The pin is a durable claim that a schedule was DERIVED against a measured
// curve. TWAP reads no curve, so stamping one on it would put a false statement
// in the audit record — the same class of mislabelling #869 refuses in the other
// direction. It is observed at the seam rather than predicted from the
// algorithm's name, so this cannot drift from what the algorithms do.
func TestScheduleE2E_ATWAPOrderIsNotPinnedToACurveItNeverRead(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	fb := &fakeBus{}
	reg := volprofilefeed.NewRegistry(0)
	publishedCurve(t, reg, profileDay, 100, 400)
	svc, store := profiledService(t, fb, &at, reg)

	if err := svc.Handle(testCtx(), submitEnv(),
		mustMarshal(t, vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_TWAP, profileVenue))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	st, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.GetExecutionSchedule().GetVolumeProfileVersion(); got != "" {
		t.Fatalf("a TWAP parent recorded profile version %q — its schedule never consulted a "+
			"curve, so the order's own audit record now asserts something that did not happen", got)
	}
}

// THE PIN IS THE PLATFORM'S, NEVER THE CALLER'S — AND SO IS THE CURVE.
//
// A client that could name the version could choose which measured curve its
// order is sliced against — an older, thinner one gives smaller early children —
// and the order's audit record would then assert a schedule the platform never
// chose. It is discarded before anything reads it, which is also what lets the
// dual-control digest exempt the field.
//
// THE CURVE IS THE HALF WITH TEETH (#943). Naming a version only lets a caller
// pick among curves somebody measured; sending the SHAPE lets them supply one
// nobody did. And because the two are checked against EACH OTHER — the stored
// curve must hash to the stored version — a caller who sent both would send a
// self-consistent pair that verifies. The only thing standing between that and a
// schedule sized against a fabricated market is that both fields are cleared
// before anything reads either.
func TestValidateSchedule_DiscardsAClientSuppliedProfileVersion(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	fb := &fakeBus{}
	svc, store := profiledService(t, fb, &at, nil) // no feed at all, so nothing can re-stamp it

	// A SELF-CONSISTENT FORGERY: a real Encode, so its version genuinely hashes to
	// its own content. Nothing downstream could tell it from a published curve; the
	// only thing that can is that it arrived on a command.
	forged := volprofilefeed.NewRegistry(0)
	forgedVersion := publishedCurve(t, forged, profileDay, 399, 400)
	p, ok := forged.Current(volprofilefeed.Series{InstrumentID: "BTC-USD", Venue: profileVenue})
	if !ok {
		t.Fatal("the fixture did not fold its own curve")
	}

	cmd := vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_TWAP, profileVenue)
	cmd.ExecutionSchedule.VolumeProfileVersion = forgedVersion
	cmd.ExecutionSchedule.VolumeProfile = p.Wire
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	st, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.GetExecutionSchedule().GetVolumeProfileVersion(); got != "" {
		t.Fatalf("a caller-supplied profile version survived onto the order as %q — the caller "+
			"would be choosing which measured curve their order is sliced against", got)
	}
	if got := st.GetExecutionSchedule().GetVolumeProfile(); got != nil {
		t.Fatalf("a caller-supplied CURVE survived onto the order (version %q) — the client just "+
			"handed the platform the market its own order is sized against, and every later "+
			"derivation on every pod would resolve it", got.GetVersion())
	}
}

// ===== REFUSED, AND THE REFUSAL SAYS WHICH OF FOUR THINGS IS MISSING =====

// NO_VOLUME_PROFILE IS STILL THE ANSWER WHEN NOTHING CAN SIZE THE ORDER, and the
// reason distinguishes four different operator actions.
//
// One code with an undifferentiated message would send a desk to look at the
// market feed when the fault was a missing option at the composition root, or to
// wait for history that will never arrive because nobody subscribed. Each arm
// below is a different fix.
func TestScheduleE2E_TheRefusalSaysWhichThingIsMissing(t *testing.T) {
	for _, tt := range []struct {
		name  string
		feed  bool
		setup func(t *testing.T, reg *volprofilefeed.Registry)
		venue string
		says  string
	}{
		{
			name:  "no profile feed is bound to this OMS at all",
			feed:  false,
			venue: profileVenue,
			says:  "not bound to a volume-profile feed",
		},
		{
			name:  "the order names no venue",
			feed:  true,
			setup: func(t *testing.T, reg *volprofilefeed.Registry) { publishedCurve(t, reg, profileDay, 100, 400) },
			venue: "",
			says:  "names no venue",
		},
		{
			name:  "nothing has ever published this series",
			feed:  true,
			venue: profileVenue,
			says:  "nothing has ever published",
		},
		{
			name: "published, and there is not enough history yet",
			feed: true,
			setup: func(t *testing.T, reg *volprofilefeed.Registry) {
				publishedVerdict(t, reg, profileDay, volprofile.VerdictTooFewSessions)
			},
			venue: profileVenue,
			says:  "TOO_FEW_SESSIONS",
		},
		{
			name: "published, and the curve is older than its retention horizon",
			feed: true,
			setup: func(t *testing.T, reg *volprofilefeed.Registry) {
				publishedVerdict(t, reg, profileDay, volprofile.VerdictStale)
			},
			venue: profileVenue,
			says:  "STALE",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			at := profileDay.Add(-time.Hour)
			fb := &fakeBus{}
			var reg *volprofilefeed.Registry
			if tt.feed {
				reg = volprofilefeed.NewRegistry(0)
				if tt.setup != nil {
					tt.setup(t, reg)
				}
			}
			svc, store := profiledService(t, fb, &at, reg)

			cmd := vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_VWAP, tt.venue)
			if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			// A HANDLER RETURNING NIL MEANS ANSWERED, NOT ACCEPTED. The store is
			// what says whether an order exists.
			if _, _, err := store.Load(context.Background(), "p1"); err == nil {
				t.Fatal("a volume-driven order was ADMITTED by an OMS that cannot size it — it " +
					"would rest at WORKING_SCHEDULED forever, or be worked against a curve nobody " +
					"measured")
			}
			rej, ok := fb.last(EventTypeRejected).(*orderpb.OrderRejected)
			if !ok {
				t.Fatalf("no ORDER_REJECTED published: %v", fb.types())
			}
			if rej.GetErrorCode() != ReasonNoVolumeProfile {
				t.Fatalf("error code = %q, want %q", rej.GetErrorCode(), ReasonNoVolumeProfile)
			}
			if !strings.Contains(rej.GetReason(), tt.says) {
				t.Errorf("the refusal does not say %q, so an operator cannot tell this cause from "+
					"the other three:\n%s", tt.says, rej.GetReason())
			}
		})
	}
}

// POV WITHOUT A CAP IS INVALID_SCHEDULE, NOT NO_VOLUME_PROFILE, EVEN WITH A
// PROFILE PRESENT.
//
// The two refusals must not collapse into one. A missing cap is the operator's
// own error and they fix it from the command; a missing profile is not theirs at
// all. The ordering is the algorithm's — pov checks its cap before asking the
// market anything — and this asserts it survives to the client now that the
// market can actually answer.
func TestScheduleE2E_POVWithoutACapIsTheOperatorsOwnError(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	fb := &fakeBus{}
	reg := volprofilefeed.NewRegistry(0)
	publishedCurve(t, reg, profileDay, 100, 400)
	svc, _ := profiledService(t, fb, &at, reg)

	cmd := vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_POV, profileVenue)
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	rej, ok := fb.last(EventTypeRejected).(*orderpb.OrderRejected)
	if !ok {
		t.Fatalf("no ORDER_REJECTED published: %v", fb.types())
	}
	if rej.GetErrorCode() != "INVALID_SCHEDULE" {
		t.Errorf("error code = %q, want INVALID_SCHEDULE", rej.GetErrorCode())
	}
	if !strings.Contains(rej.GetReason(), string(algo.NameVWAP)) {
		t.Errorf("reason %q does not tell the operator which algorithm works this order without "+
			"a cap", rej.GetReason())
	}
}

// A POV ORDER WITH A CAP IS ADMITTED AND ITS PARTICIPATION IS BOUNDED BY THE
// MEASURED CURVE.
//
// The cap binds against the profile, so this is also the assertion that the level
// — not just the shape — survives the wire: a curve carrying the right proportions
// and the wrong magnitude would admit an order the cap should have refused.
func TestScheduleE2E_POVIsBoundedByTheMeasuredLevel(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	fb := &fakeBus{}
	reg := volprofilefeed.NewRegistry(0)
	publishedCurve(t, reg, profileDay, 100, 400) // 400 units expected across the session

	// The parent is 60 units. At a 10% cap the window can work at most 40, so this
	// must be refused; at 20% it can work 80, so it must be admitted.
	for _, tt := range []struct {
		name     string
		rate     *commonpb.Decimal
		admitted bool
	}{
		{"a cap the measured volume cannot honour", d(10, -2), false},
		{"a cap it can", d(20, -2), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fb2 := &fakeBus{}
			svc, store := profiledService(t, fb2, &at, reg)
			cmd := vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_POV, profileVenue)
			cmd.ExecutionSchedule.MaxParticipationRate = tt.rate
			if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			_, _, err := store.Load(context.Background(), "p1")
			if tt.admitted && err != nil {
				t.Fatalf("a POV order inside its cap was refused: %v",
					fb2.last(EventTypeRejected))
			}
			if !tt.admitted && err == nil {
				t.Fatal("a POV order that would breach its participation cap against the measured " +
					"volume was ADMITTED — the cap is the one control POV exists to provide")
			}
		})
	}
	_ = fb
}

// ===== THE INSTRUMENT AND THE CAP STILL REACH THE PLAN =====

// Both are inputs to a volume-driven schedule and both come off the ORDER, which
// is what keeps the schedule derivable. A field validated at admission and then
// dropped on the way to the plan is invisible here, because TWAP reads neither.
func TestPlanFromOrder_CarriesTheInstrumentAndTheParticipationCap(t *testing.T) {
	st := &orderpb.OrderState{
		OrderId:         "p1",
		InstrumentId:    "BTC-USD",
		OrderedQuantity: d(60, 0),
		ExecutionSchedule: &orderpb.ExecutionSchedule{
			Algo:                 orderpb.ExecutionAlgo_EXECUTION_ALGO_POV,
			WindowStart:          scheduledOrder("p1", 6, nil).ExecutionSchedule.WindowStart,
			WindowEnd:            scheduledOrder("p1", 6, nil).ExecutionSchedule.WindowEnd,
			SliceCount:           6,
			MaxParticipationRate: d(8, -2),
		},
	}

	plan, err := planFromOrder(st)
	if err != nil {
		t.Fatalf("planFromOrder: %v", err)
	}
	if plan.InstrumentID != "BTC-USD" {
		t.Errorf("plan instrument = %q, want BTC-USD — a volume-driven algorithm would ask the "+
			"market view about nothing", plan.InstrumentID)
	}
	if plan.MaxParticipation == nil {
		t.Fatal("the participation cap did not reach the plan — POV would refuse an order that " +
			"carries one, or worse, work one uncapped if the refusal were ever relaxed")
	}
	if want := new(big.Rat).SetFrac64(8, 100); plan.MaxParticipation.Cmp(want) != 0 {
		t.Errorf("cap = %s, want %s", plan.MaxParticipation.RatString(), want.RatString())
	}
}
