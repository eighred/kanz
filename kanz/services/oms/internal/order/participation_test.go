package order

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/marketedge/volprofile"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/volprofilefeed"
	"github.com/eighred/kanz/services/oms/internal/schedule"
	"github.com/eighred/kanz/services/oms/internal/tape"
)

// #1007'S "VERIFIED WHEN", END TO END.
//
//	An ExecutionAttributionRecorded for a POV parent carries a realised
//	participation rate and its quality, and a test drives a parent against a
//	realised tape THINNER than the forecast it was scheduled on and asserts that
//	the exceedance is reported — measured, published and counted.
//
// # Why the whole path is exercised rather than the arithmetic
//
// internal/execution/tca/participation_test.go proves the division. What it
// cannot prove is the half that was actually missing before this: that a finished
// parent's children are matched back to the SCHEDULE's own intervals, that the
// figure reaches the FACT the estate already publishes, and that a breach moves a
// series somebody can alert on. Every one of those is a join between two things
// that already existed, and a join is exactly what a unit test on either side
// cannot see.
//
// # The fixture, chosen so every number is checkable by hand
//
//	profile     400 units expected across the session, 1/4 of it in the first
//	            half-hour and 3/4 at noon (publishedCurve(100, 400)).
//	parent      60 units, POV, capped at 20%, worked in two half-session slices.
//	            The forecast says 100 will print before noon and 300 after, so
//	            the schedule is 15 then 45 — and 15/100 = 15% and 45/300 = 15%,
//	            comfortably inside the cap. That is the admission decision, and
//	            it is correct.
//	tape        the morning printed 50, not 100. The afternoon printed 450.
//
// So the first child was 15 of 50 = 30% of the tape, against a cap of 20%, on a
// schedule that was admitted honestly against a forecast. That is the entire
// failure mode POV's own doc names — "if the realised tape comes in thinner than
// the profile, the children were sized for volume that did not arrive" — and
// before this nothing anywhere could say it had happened.

// partCounters is the pair cmd/oms registers, built here so a test can read them.
func partCounters() (quality, exceeded *prometheus.CounterVec) {
	quality = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "kanz_oms_participation_measurements_total"}, []string{"quality"})
	exceeded = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "kanz_oms_participation_cap_exceeded_total"},
		[]string{"venue", "instrument"})
	for _, q := range ParticipationQualities {
		quality.WithLabelValues(q)
	}
	return quality, exceeded
}

// participationService is profiledService plus an arrival mark and a realised
// tape.
//
// THE ARRIVAL MARK IS NOT OPTIONAL HERE, and the reason is a real coupling worth
// stating: participation rides ExecutionAttributionRecorded, and that record is
// not published at all for a decision with no arrival mark. So an OMS whose price
// spine covers nothing measures no participation either — visible as
// no_arrival_mark on kanz_oms_execution_attributions_total rather than as a
// participation gap.
func participationService(t *testing.T, fb *fakeBus, at *time.Time, reg *volprofilefeed.Registry,
	realised *tape.Fold, quality, exceeded *prometheus.CounterVec) (*Service, *MemoryStore) {

	t.Helper()
	store := NewMemoryStore()
	router := execution.NewRouter([]execution.Venue{execution.NewSimVenue(profileVenue)})
	opts := []ServiceOption{
		WithHaltGate(halt.OpenGate(nil)),
		WithVolumeProfiles(reg),
		WithArrivalMarks(attrMarks()),
		WithParticipationCounter(quality),
		WithParticipationBreachCounter(exceeded),
	}
	if realised != nil {
		opts = append(opts, WithRealisedVolume(realised))
	}
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, router, nil, nil, opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return *at }
	return svc, store
}

// realisedTape folds one candle per (offset, volume) pair on the fixture series,
// through the real bus handler so a fixture that the producer's own shape would
// be refused fails here rather than passing against itself.
func realisedTape(t *testing.T, at ...any) *tape.Fold {
	t.Helper()
	f := tape.NewFold(0)
	for i := 0; i < len(at); i += 2 {
		open := profileDay.Add(at[i].(time.Duration))
		payload, err := proto.Marshal(&marketpb.MarketDataEvent{
			InstrumentId: "BTC-USD",
			Mic:          profileVenue,
			EventTime:    timestamppb.New(open.Add(time.Minute)),
			Data: &marketpb.MarketDataEvent_Bar{Bar: &marketpb.Bar{
				OpenTime:  timestamppb.New(open),
				CloseTime: timestamppb.New(open.Add(time.Minute)),
				Volume:    &commonpb.Decimal{Coefficient: at[i+1].(int64)},
			}},
		})
		if err != nil {
			t.Fatalf("marshal candle: %v", err)
		}
		if err := f.Handle(context.Background(), nil, payload); err != nil {
			t.Fatalf("fold candle: %v", err)
		}
	}
	if _, refused, _, series := f.Stats(); refused != 0 || series != 1 {
		t.Fatalf("fixture tape: refused=%d series=%d, want 0 and 1", refused, series)
	}
	return f
}

// povParent is the 60-unit capped parent, worked in two half-session slices.
func povParent(cap *commonpb.Decimal) *orderpb.SubmitOrder {
	cmd := vwapOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_POV, profileVenue)
	cmd.ExecutionSchedule.MaxParticipationRate = cap
	return cmd
}

// workAPovParentToTheEnd admits the parent, drives every slice, and retires it,
// returning the attribution FACT that was published.
func workAPovParentToTheEnd(t *testing.T, svc *Service, store *MemoryStore, fb *fakeBus,
	at *time.Time, cmd *orderpb.SubmitOrder) *orderpb.ExecutionAttributionRecorded {

	t.Helper()
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, _, err := store.Load(context.Background(), "p1"); err != nil {
		t.Fatalf("the POV parent was refused at admission (%v) — this fixture is supposed to be "+
			"workable against the FORECAST, which is the whole point: the breach is realised, not "+
			"predicted. Rejection: %v", err, fb.last(EventTypeRejected))
	}

	*at = profileDay.Add(volprofile.Session).Add(time.Minute)
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 2 {
		t.Fatalf("created %d children (err %v), want 2", n, err)
	}
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("retire pass: %v", err)
	}
	st, _, err := store.Load(context.Background(), "p1")
	if err != nil || !IsTerminal(st) {
		t.Fatalf("the parent is %v (err %v) — nothing terminal, so nothing is attributed",
			st.GetStatus(), err)
	}
	a, _ := fb.last(EventTypeAttributed).(*orderpb.ExecutionAttributionRecorded)
	if a == nil {
		t.Fatalf("a worked-out parent published no execution attribution: %v", fb.types())
	}
	return a
}

// THE BREACH IS MEASURED, PUBLISHED AND COUNTED.
func TestParticipationE2E_AThinnerTapeThanTheForecastIsReported(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	fb := &fakeBus{}
	reg := volprofilefeed.NewRegistry(0)
	publishedCurve(t, reg, profileDay, 100, 400)
	quality, exceeded := partCounters()
	// The morning printed 50 against a forecast of 100; the afternoon 450.
	tapeFold := realisedTape(t, time.Duration(0), int64(50), 12*time.Hour, int64(450))
	svc, store := participationService(t, fb, &at, reg, tapeFold, quality, exceeded)

	a := workAPovParentToTheEnd(t, svc, store, fb, &at, povParent(d(20, -2)))

	if got := a.GetParticipationQuality(); got != orderpb.ParticipationQuality_PARTICIPATION_QUALITY_MEASURED {
		t.Fatalf("participation_quality = %s, want MEASURED — both intervals had a candle behind "+
			"them, and a record that will not say it was measured is one nothing may act on", got)
	}
	// 60 filled over the 500 that printed across the window.
	if got := dec.FromProto(a.GetRealisedParticipationRate()).RatString(); got != "3/25" {
		t.Errorf("realised_participation_rate = %s, want 3/25 (60 over 500)", got)
	}
	// 15 into a morning that printed 50.
	if got := dec.FromProto(a.GetMaxSliceParticipationRate()).RatString(); got != "3/10" {
		t.Errorf("max_slice_participation_rate = %s, want 3/10 — this is the figure the cap "+
			"actually bounds, and the aggregate above sits comfortably under the cap it broke", got)
	}
	if got := a.GetMaxSliceParticipationIndex(); got != 0 {
		t.Errorf("max_slice_participation_index = %d, want 0 — an investigation must start at an "+
			"interval rather than at a window", got)
	}
	if a.GetParticipationIntervals() != 2 || a.GetMeasuredParticipationIntervals() != 2 {
		t.Errorf("coverage = %d of %d, want 2 of 2", a.GetMeasuredParticipationIntervals(),
			a.GetParticipationIntervals())
	}
	// THE CAP TRAVELS WITH THE RATES. Without it every consumer joins back to
	// OrderState to answer "was it honoured", and each of them does it differently.
	if got := dec.FromProto(a.GetParticipationCap()).RatString(); got != "1/5" {
		t.Errorf("participation_cap = %s, want 1/5 — the record cannot be read as a breach "+
			"without the number it breached", got)
	}
	// AND THE ALGORITHM THAT PRODUCED IT. Until this field a consumer had to join
	// back to OrderState to learn which algorithm ran, so the one comparison #866
	// exists for was the one it could not serve alone.
	if got := a.GetExecutionAlgo(); got != orderpb.ExecutionAlgo_EXECUTION_ALGO_POV {
		t.Errorf("execution_algo = %s, want EXECUTION_ALGO_POV", got)
	}

	if got := testutil.ToFloat64(quality.WithLabelValues("measured")); got != 1 {
		t.Errorf("measured count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(exceeded.WithLabelValues(profileVenue, "BTC-USD")); got != 1 {
		t.Errorf("kanz_oms_participation_cap_exceeded_total{venue=%q,instrument=\"BTC-USD\"} = %v, "+
			"want 1. This is the ONE series that says a control did not hold; without it the "+
			"breach is a number in a FACT somebody has to go and read", profileVenue, got)
	}
}

// A TAPE THAT ARRIVED AS FORECAST IS MEASURED AND COUNTS NO BREACH.
//
// The negative case is half the value: a counter that moves on a schedule that
// honoured its cap is a page a desk silences on the first morning, after which
// the real breach is invisible too.
func TestParticipationE2E_ATapeThatMatchedTheForecastCountsNoBreach(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	fb := &fakeBus{}
	reg := volprofilefeed.NewRegistry(0)
	publishedCurve(t, reg, profileDay, 100, 400)
	quality, exceeded := partCounters()
	tapeFold := realisedTape(t, time.Duration(0), int64(200), 12*time.Hour, int64(600))
	svc, store := participationService(t, fb, &at, reg, tapeFold, quality, exceeded)

	a := workAPovParentToTheEnd(t, svc, store, fb, &at, povParent(d(20, -2)))

	if got := a.GetParticipationQuality(); got != orderpb.ParticipationQuality_PARTICIPATION_QUALITY_MEASURED {
		t.Fatalf("participation_quality = %s, want MEASURED", got)
	}
	// 15 of 200 = 7.5%, 45 of 600 = 7.5%: the worst interval is well inside 20%.
	if got := dec.FromProto(a.GetMaxSliceParticipationRate()).RatString(); got != "3/40" {
		t.Errorf("max_slice_participation_rate = %s, want 3/40", got)
	}
	if got := testutil.ToFloat64(exceeded.WithLabelValues(profileVenue, "BTC-USD")); got != 0 {
		t.Errorf("a schedule that honoured its cap incremented the breach counter %v time(s)", got)
	}
}

// NO TAPE AT ALL IS UNOBSERVABLE — NOT A RATE OF ZERO, AND NOT A BREACH.
//
// This is the state every deployment is in until a candle spine reaches the OMS,
// and it is the state the whole quality vocabulary exists for: the cap this order
// carried has been neither confirmed nor contradicted, and a record saying so is
// different from one reporting a comfortable zero.
func TestParticipationE2E_NoRealisedTapeIsUnobservable(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	fb := &fakeBus{}
	reg := volprofilefeed.NewRegistry(0)
	publishedCurve(t, reg, profileDay, 100, 400)
	quality, exceeded := partCounters()
	svc, store := participationService(t, fb, &at, reg, nil, quality, exceeded)

	a := workAPovParentToTheEnd(t, svc, store, fb, &at, povParent(d(20, -2)))

	if got := a.GetParticipationQuality(); got != orderpb.ParticipationQuality_PARTICIPATION_QUALITY_UNOBSERVABLE {
		t.Fatalf("participation_quality = %s, want UNOBSERVABLE from an OMS bound to no tape", got)
	}
	if a.GetRealisedParticipationRate() != nil || a.GetMaxSliceParticipationRate() != nil {
		t.Errorf("rates are %v / %v, want both UNSET — a rate nobody could compute must not "+
			"arrive as a number, and a zero here would report the calmest possible participation "+
			"in exactly the state nothing was measured",
			a.GetRealisedParticipationRate(), a.GetMaxSliceParticipationRate())
	}
	if got := a.GetMaxSliceParticipationIndex(); got != -1 {
		t.Errorf("max_slice_participation_index = %d, want -1", got)
	}
	// THE CAP IS STILL CARRIED. It is the claim the missing measurement failed to
	// check, and a reader holding an unobservable record still needs to know what
	// was promised.
	if got := dec.FromProto(a.GetParticipationCap()).RatString(); got != "1/5" {
		t.Errorf("participation_cap = %s, want 1/5 even on an unobservable record", got)
	}
	// AND THE COVERAGE COUNTS ARE STILL THE COVERAGE COUNTS: two intervals traded,
	// none of them seen. "Nothing configured" and "checked, and nothing is known"
	// reach the record identically here on purpose; the counter is where the
	// deployment gap is legible.
	if a.GetParticipationIntervals() != 2 || a.GetMeasuredParticipationIntervals() != 0 {
		t.Errorf("coverage = %d of %d, want 0 of 2", a.GetMeasuredParticipationIntervals(),
			a.GetParticipationIntervals())
	}
	if got := testutil.ToFloat64(quality.WithLabelValues("unobservable")); got != 1 {
		t.Errorf("unobservable count = %v, want 1 — an OMS that measures nothing must export a "+
			"RISING series, because that is the finding", got)
	}
	if got := testutil.ToFloat64(exceeded.WithLabelValues(profileVenue, "BTC-USD")); got != 0 {
		t.Errorf("an unobservable measurement counted %v breach(es) — every one of these is "+
			"supposed to be an event a desk can point at", got)
	}
}

// AN UNSLICED ORDER IS NOT_WORKED, AND CARRIES NO PARTICIPATION FIGURE.
//
// "This order had no working window" and "this order's window could not be seen"
// are different operator problems and only the second is a market-data gap.
// Collapsing them would make an estate that trades most of its orders whole look
// like one whose candle spine is broken.
func TestParticipationE2E_AnUnslicedDecisionIsNotWorked(t *testing.T) {
	at := profileDay.Add(-time.Hour)
	fb := &fakeBus{}
	quality, exceeded := partCounters()
	svc, _ := participationService(t, fb, &at, volprofilefeed.NewRegistry(0),
		realisedTape(t, time.Duration(0), int64(50)), quality, exceeded)

	cmd := limitOrder(d(100, 0), d(1025, -2))
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	a, _ := fb.last(EventTypeAttributed).(*orderpb.ExecutionAttributionRecorded)
	if a == nil {
		t.Fatalf("no attribution for a filled ordinary order: %v", fb.types())
	}
	if got := a.GetParticipationQuality(); got != orderpb.ParticipationQuality_PARTICIPATION_QUALITY_NOT_WORKED {
		t.Errorf("participation_quality = %s, want NOT_WORKED", got)
	}
	if a.GetParticipationIntervals() != 0 || a.GetParticipationCap() != nil {
		t.Errorf("intervals = %d, cap = %v, want 0 and unset on an order that was never worked "+
			"as a schedule", a.GetParticipationIntervals(), a.GetParticipationCap())
	}
	if got := testutil.ToFloat64(quality.WithLabelValues("not_worked")); got != 1 {
		t.Errorf("not_worked count = %v, want 1", got)
	}
}

// THE INTERVALS COME FROM THE SCHEDULE, AND A CHILD IS MATCHED TO ITS OWN SLICE.
//
// A schedule can have HOLES — a crash between two creations leaves slice 2
// missing while 3 exists, which is why the driver uses a predicate over indices
// rather than a count. Matching positionally would attribute slice 3's fills to
// slice 2's minutes for every slice after the hole, and the resulting rate would
// be plausible, wrong, and impossible to spot.
func TestParticipationIntervals_AreMatchedByDerivedChildID(t *testing.T) {
	svc := &Service{}
	st := &orderpb.OrderState{
		OrderId:         "p1",
		InstrumentId:    "BTC-USD",
		Venue:           profileVenue,
		OrderedQuantity: d(60, 0),
		ExecutionSchedule: &orderpb.ExecutionSchedule{
			Algo:        orderpb.ExecutionAlgo_EXECUTION_ALGO_TWAP,
			WindowStart: timestamppb.New(schedStart),
			WindowEnd:   timestamppb.New(schedEnd),
			SliceCount:  6,
		},
	}
	// Only slices 1 and 4 exist, delivered in the wrong order — which is what a
	// store list after a hole and a retry looks like.
	children := []*orderpb.OrderState{
		{OrderId: schedule.ChildID("p1", 4), Venue: profileVenue, FilledQuantity: d(10, 0)},
		{OrderId: schedule.ChildID("p1", 1), Venue: profileVenue, FilledQuantity: d(10, 0)},
		{OrderId: "not-a-slice-of-this-parent", Venue: profileVenue, FilledQuantity: d(99, 0)},
	}

	got := svc.participationIntervals(st, children)
	if len(got) != 2 {
		t.Fatalf("%d intervals, want 2 — the order that is not a derived child of this parent "+
			"must be dropped rather than given an interval the schedule never claimed", len(got))
	}
	byIndex := map[int]time.Time{}
	for _, in := range got {
		byIndex[in.Index] = in.From
	}
	// The window is one hour in six slices, so slice i opens at schedStart+10i.
	for _, c := range []struct {
		index int
		want  time.Time
	}{{1, schedStart.Add(10 * time.Minute)}, {4, schedStart.Add(40 * time.Minute)}} {
		if got, ok := byIndex[c.index]; !ok || !got.Equal(c.want) {
			t.Errorf("slice %d opens at %v (present %v), want %v — a positional match would put "+
				"this child's fills in another slice's minutes", c.index, got, ok, c.want)
		}
	}
}
