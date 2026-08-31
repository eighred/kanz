package order

// #866'S "VERIFIED WHEN", AT THE SERVICE LEVEL AND AGAINST THE ENGINE.
//
//	TEST_POSTGRES_URL=… go test -p 1 -run 'TestArrival|TestATerminal' ./services/oms/...
//
// Two properties, and neither is provable without a real transaction:
//
//  1. AN ORDER CANNOT EXIST WITHOUT THE BENCHMARK IT WILL BE JUDGED AGAINST.
//     The arrival mark and the release observation are stamped into the state
//     that the admission INSERT writes, so there is no instant at which the row
//     exists and the benchmark does not. A MemoryStore cannot show this: it has
//     no versions and no second write to distinguish "written by admission" from
//     "written by something afterwards".
//
//  2. THE ATTRIBUTION COMMITS WITH THE STATE CHANGE THAT ENDED THE DECISION.
//     It rides the same outbox transaction as the terminal FACT, so a crash or a
//     broker refusal between the two is not reachable. The proof is a broker that
//     REFUSES the fill FACT: the fold still commits, and the queue must then hold
//     BOTH records — the fill and the attribution — because they were one write.

import (
	"math/big"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

// The scenario, chosen so every expected figure is checkable by hand:
//
//	arrival mid   10.00, quoted 9.99 / 10.01
//	filled        100 @ 10.25 (SimVenue fills a LIMIT order at its limit price)
//
// so the realised shortfall is 250 bps of the 1,000 arrival notional, of which
// 10 bps is the half-width that was quoted and 0 bps is drift — an unsliced
// order is decided and released in the same instant — leaving 240 bps of impact.
const (
	attrArrival  = "10"
	attrBid      = "9.99"
	attrAsk      = "10.01"
	attrShortall = "250"
	attrSpread   = "10"
	attrTiming   = "0"
	attrImpact   = "240"
)

func attrMarks() fakeMarks {
	return fakeMarks{
		price: dec.Rat(attrArrival), asOf: markObserved, seen: true,
		bid: dec.Rat(attrBid), ask: dec.Rat(attrAsk), touchAt: markObserved,
	}
}

// TestArrivalAndReleaseAreCommittedByTheAdmissionWriteItself is property (1).
//
// THE VERSION IS THE PROOF, not the presence of the fields. Store.Save advances
// `version` on every post-admission transition, so a row read back at version 0
// carrying a benchmark is a row whose benchmark was in the INSERT. Asserting only
// that the fields are populated would pass just as well against a design that
// stamped them in a second write moments later — which is the design #866 exists
// to refuse, because the window between the two is exactly where an order comes
// to exist with nothing to judge it by.
func TestArrivalAndReleaseAreCommittedByTheAdmissionWriteItself(t *testing.T) {
	pool := newPool(t) // skips unless TEST_POSTGRES_URL is set
	ctx := testCtx()

	// The broker refuses the ACCEPTED FACT, so admission commits and then fails.
	// Nothing downstream of the INSERT runs, which is what makes the version
	// assertion below mean something.
	fb := &fakeBus{failOn: EventTypeAccepted}
	store := NewPostgres(pool)
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")}), nil, nil,
		WithHaltGate(halt.OpenGate(nil)), WithArrivalMarks(attrMarks()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("Handle returned nil after the ACCEPTED publish failed; this test needs admission " +
			"to stop there so nothing else can write to the row")
	}

	st, ver, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver != 0 {
		t.Fatalf("the order is at version %d, so something wrote to it after admission and this "+
			"test can no longer tell which write carried the benchmark", ver)
	}
	if st.GetArrivalPrice() == nil || st.GetArrivalAt() == nil {
		t.Fatalf("the committed order carries no arrival benchmark (price=%v at=%v). An order that "+
			"exists without one is permanently unmeasurable, and no later pass can recover it — a "+
			"mark read afterwards is a mark from after this order moved the market",
			st.GetArrivalPrice(), st.GetArrivalAt())
	}
	if st.GetReleasePrice() == nil || st.GetReleaseBid() == nil || st.GetReleaseAsk() == nil {
		t.Fatalf("the committed order carries no release observation (mark=%v bid=%v ask=%v), so "+
			"its shortfall can never be decomposed",
			st.GetReleasePrice(), st.GetReleaseBid(), st.GetReleaseAsk())
	}
	if got := dec.FromProto(st.GetArrivalPrice()).RatString(); got != attrArrival {
		t.Errorf("arrival_price = %s, want %s — it survived the BYTEA round trip as a different "+
			"benchmark", got, attrArrival)
	}
}

// TestATerminalDecisionCommitsItsAttributionWithTheStateChange is property (2),
// and it is the decisive test #866 names: a book with known fills against a
// known arrival produces a shortfall whose legs sum to the realised difference —
// end to end, through admission, the venue, the fold, protobuf and Postgres.
func TestATerminalDecisionCommitsItsAttributionWithTheStateChange(t *testing.T) {
	pool := newPool(t)
	ctx := testCtx()

	// The broker refuses the fill FACT. The fold commits anyway (#292), so
	// whatever is in the queue afterwards is what that ONE transaction wrote.
	fb := &fakeBus{failOn: EventTypeFilled}
	store := NewPostgres(pool)
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")}), nil, nil,
		WithHaltGate(halt.OpenGate(nil)), WithArrivalMarks(attrMarks()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("Handle returned nil after the fill publish failed")
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("committed status = %v, want FILLED", got)
	}

	pending, err := store.Outbox().Pending(ctx, cmd.GetOrderId(), 10)
	if err != nil {
		t.Fatalf("outbox Pending: %v", err)
	}
	// TWO RECORDS, FROM ONE TRANSACTION. Before this change there was one; a
	// third would mean something is announcing twice.
	if len(pending) != 2 {
		var got []string
		for _, p := range pending {
			got = append(got, p.Record.EventType)
		}
		t.Fatalf("the outbox holds %v for a terminal decision, want exactly the ORDER_FILLED FACT "+
			"and its execution attribution. The attribution rides the fill's own transaction, so "+
			"a missing one is a decision the execution-quality report will never see", got)
	}
	var attrRec *orderpb.ExecutionAttributionRecorded
	for _, p := range pending {
		if p.Record.EventType != EventTypeAttributed {
			continue
		}
		event, derr := p.Record.Event()
		if derr != nil {
			t.Fatalf("the queued attribution does not decode: %v — the relay cannot publish it, so "+
				"it sits at the head of this order's key blocking every FACT behind it", derr)
		}
		a, ok := event.Payload.(*orderpb.ExecutionAttributionRecorded)
		if !ok {
			t.Fatalf("the queued attribution decoded to %T", event.Payload)
		}
		attrRec = a
	}
	if attrRec == nil {
		t.Fatalf("no %s record in the outbox — the terminal transition committed without its "+
			"measurement", EventTypeAttributed)
	}

	if attrRec.GetQuality() != orderpb.AttributionQuality_ATTRIBUTION_QUALITY_DECOMPOSED {
		t.Fatalf("quality = %s, want DECOMPOSED — the order was admitted against a QUOTED market, "+
			"so every leg was observable", attrRec.GetQuality())
	}
	if attrRec.GetBenchmark() != orderpb.Benchmark_BENCHMARK_ARRIVAL {
		t.Errorf("benchmark = %s, want ARRIVAL — a record that does not say what it was measured "+
			"against is a record nothing may aggregate", attrRec.GetBenchmark())
	}
	if attrRec.GetOrderId() != cmd.GetOrderId() {
		t.Errorf("order_id = %q, want %q", attrRec.GetOrderId(), cmd.GetOrderId())
	}
	if attrRec.GetSlices() != 1 || attrRec.GetMeasuredSlices() != 1 {
		t.Errorf("slices/measured = %d/%d, want 1/1", attrRec.GetSlices(), attrRec.GetMeasuredSlices())
	}
	if attrRec.GetTerminalStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Errorf("terminal_status = %s, want FILLED", attrRec.GetTerminalStatus())
	}
	if attrRec.GetMeasuredAt() == nil {
		t.Error("no measured_at — a replayed terminal transition produces a late measurement of an " +
			"old order, and a reader comparing costs over a window cannot tell which clock it holds")
	}

	// THE FOUR FIGURES, EACH AGAINST A HAND-COMPUTED EXPECTATION. Checking only
	// the identity would pass for the degenerate split where spread and timing
	// are zero and impact is the whole shortfall — the "arithmetic dressing"
	// failure #866 is explicit about refusing.
	for _, c := range []struct {
		name string
		got  *commonpb.Decimal
		want string
	}{
		{"shortfall_bps", attrRec.GetShortfallBps(), attrShortall},
		{"spread_bps", attrRec.GetSpreadBps(), attrSpread},
		{"timing_bps", attrRec.GetTimingBps(), attrTiming},
		{"impact_bps", attrRec.GetImpactBps(), attrImpact},
	} {
		if c.got == nil {
			t.Errorf("%s is UNSET on a DECOMPOSED record", c.name)
			continue
		}
		if got := dec.FromProto(c.got).RatString(); got != c.want {
			t.Errorf("%s = %s, want %s", c.name, got, c.want)
		}
	}

	// AND THE IDENTITY, over the wire values rather than the in-process ones —
	// this is what a consumer will actually read, after two protobuf round trips
	// and a rescale through dec.ToProtoScaled.
	sum := new(big.Rat).Add(dec.FromProto(attrRec.GetSpreadBps()), dec.FromProto(attrRec.GetTimingBps()))
	sum.Add(sum, dec.FromProto(attrRec.GetImpactBps()))
	if sum.Cmp(dec.FromProto(attrRec.GetShortfallBps())) != 0 {
		t.Fatalf("the legs on the wire do not sum to the shortfall: %s != %s — a decomposition "+
			"whose parts do not reconstitute the whole is three unrelated numbers",
			sum.RatString(), dec.FromProto(attrRec.GetShortfallBps()).RatString())
	}
}

// TestAnUnmeasurableDecisionPublishesNothingRatherThanZero.
//
// An order admitted with no usable mark is UNMEASURABLE, not free. Publishing a
// zero-cost record for it would drag every algorithm comparison toward whichever
// instruments the price spine covers worst — and it would do it silently,
// because a zero is a plausible number.
func TestAnUnmeasurableDecisionPublishesNothingRatherThanZero(t *testing.T) {
	pool := newPool(t)
	ctx := testCtx()

	fb := &fakeBus{failOn: EventTypeFilled}
	store := NewPostgres(pool)
	// No mark source at all: the deployment has no price spine.
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")}), nil, nil,
		WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("Handle returned nil after the fill publish failed")
	}
	pending, err := store.Outbox().Pending(ctx, cmd.GetOrderId(), 10)
	if err != nil {
		t.Fatalf("outbox Pending: %v", err)
	}
	for _, p := range pending {
		if p.Record.EventType == EventTypeAttributed {
			t.Fatalf("an attribution was published for an order with no arrival benchmark. It " +
				"would be read as a zero-cost execution, and zero-cost executions are what pull " +
				"a venue or algorithm ranking toward the instruments nobody can price")
		}
	}
}

// TestAChildDoesNotAttributeItsOwnExecution.
//
// A slice is not a decision. Attributing to one would report the platform making
// fifty decisions where a desk made one — each measured against a benchmark it
// did not choose — and it would double-count the parent's cost into the bargain.
func TestAChildDoesNotAttributeItsOwnExecution(t *testing.T) {
	pool := newPool(t)
	ctx := testCtx()

	at := schedStart
	fb := &fakeBus{}
	store := NewPostgres(pool)
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")}), nil, nil,
		WithHaltGate(halt.OpenGate(nil)), WithArrivalMarks(attrMarks()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return at }

	parent := scheduledOrder("attrparent", 2, nil)
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, parent)); err != nil {
		t.Fatalf("Handle parent: %v", err)
	}
	// Well into the window, so both slices are due and both fill at the sim venue.
	at = schedEnd
	n, derr := svc.DriveSchedules(driveCtx())
	if derr != nil {
		t.Fatalf("DriveSchedules: %v", derr)
	}
	if n == 0 {
		t.Fatalf("the driver created no children (parent rejected: %v), so this test would pass "+
			"vacuously", fb.last(EventTypeRejected))
	}

	// Whatever the driver produced, no CHILD may carry an attribution.
	children, err := store.ListByParent(ctx, parent.GetOrderId())
	if err != nil {
		t.Fatalf("ListByParent: %v", err)
	}
	if len(children) == 0 {
		t.Fatal("no children were created, so this test would pass vacuously")
	}
	for _, c := range children {
		if !IsTerminal(c) {
			continue
		}
		pendingC, perr := store.Outbox().Pending(ctx, c.GetOrderId(), 10)
		if perr != nil {
			t.Fatalf("outbox Pending for child %s: %v", c.GetOrderId(), perr)
		}
		for _, p := range pendingC {
			if p.Record.EventType == EventTypeAttributed {
				t.Fatalf("child %s attributed its own execution — a parent worked in N slices would "+
					"produce N+1 decisions in the report, N of them benchmarked against a mark the "+
					"slice did not choose", c.GetOrderId())
			}
		}
		if fbHasAttributionFor(fb, c.GetOrderId()) {
			t.Fatalf("child %s published an attribution on the bus", c.GetOrderId())
		}
	}
}

// fbHasAttributionFor reports whether the fake bus saw an attribution for one
// order id. The queue check above cannot see records the relay has already
// drained, and a child's fills DO drain on the happy path.
func fbHasAttributionFor(fb *fakeBus, orderID string) bool {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for _, e := range fb.events {
		if e.EventType != EventTypeAttributed {
			continue
		}
		if a, ok := e.Payload.(*orderpb.ExecutionAttributionRecorded); ok && a.GetOrderId() == orderID {
			return true
		}
	}
	return false
}

// steppedMarks is a mark source that moves ONCE, at a named instant, so a test
// can put a known amount of market drift between a parent's admission and its
// children's release without doing arithmetic on a clock.
//
// A CONSTANT MARK WOULD MAKE THE TIMING LEG PASS VACUOUSLY. With one price for
// the whole window every child's release mark equals the parent's arrival mark,
// timing is zero for the right answer and for the wrong one alike, and the
// assertion survives deleting the release stamp entirely.
type steppedMarks struct {
	at            *time.Time
	step          time.Time
	before, after *big.Rat
	halfSpread    *big.Rat
	seenAt        time.Time
}

func (m *steppedMarks) mid() *big.Rat {
	if m.at.Before(m.step) {
		return new(big.Rat).Set(m.before)
	}
	return new(big.Rat).Set(m.after)
}

func (m *steppedMarks) Mark(string) *big.Rat { return m.mid() }

func (m *steppedMarks) Lookup(string) (*big.Rat, time.Time, bool) {
	return m.mid(), m.seenAt, true
}

func (m *steppedMarks) Touch(string) (*big.Rat, *big.Rat, time.Time, bool) {
	mid := m.mid()
	return new(big.Rat).Sub(mid, m.halfSpread), new(big.Rat).Add(mid, m.halfSpread), m.seenAt, true
}

// TestAWorkedParentAttributesItsChildrensExecutionOnce is the case the whole
// decomposition exists for, and the one #864 will be judged on.
//
// A 60-unit parent is decided when the mid is 10.00. The market drifts to 10.20
// before either slice is released, and both slices then fill at 10.25 into a
// 10.19 / 10.21 book:
//
//	shortfall  250 bps — the achieved 10.25 against the 10.00 decision mid
//	spread      10 bps — the half-width quoted when each slice went out
//	timing     200 bps — the market's own drift, which no algorithm caused
//	impact      40 bps — what was paid beyond both
//
// FOUR FIFTHS OF THAT COST WAS THE MARKET, NOT THE ALGORITHM. A single 250 bps
// slippage number says the opposite by omission, and it is the number the
// platform had before this. That difference is the entire argument for the
// decomposition, and this test is where it is demonstrated rather than asserted.
func TestAWorkedParentAttributesItsChildrensExecutionOnce(t *testing.T) {
	pool := newPool(t)
	ctx := testCtx()

	at := schedStart
	fb := &fakeBus{}
	store := NewPostgres(pool)
	marks := &steppedMarks{
		at: &at, step: schedStart.Add(time.Minute),
		before: dec.Rat("10"), after: dec.Rat("10.20"),
		halfSpread: dec.Rat("0.01"), seenAt: markObserved,
	}
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")}), nil, nil,
		WithHaltGate(halt.OpenGate(nil)), WithArrivalMarks(marks))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return at }

	parent := scheduledOrder("attrworked", 2, nil)
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, parent)); err != nil {
		t.Fatalf("Handle parent: %v", err)
	}

	// The market moves, then both slices go out and fill.
	at = schedEnd
	n, err := svc.DriveSchedules(driveCtx())
	if err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}
	if n != 2 {
		t.Fatalf("the driver created %d children (parent rejected: %v), want 2 — with none this "+
			"test would pass vacuously", n, fb.last(EventTypeRejected))
	}
	// The next tick finds every slice sent and finished, and retires the parent.
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("DriveSchedules (retire): %v", err)
	}

	ps, _, err := store.Load(ctx, parent.GetOrderId())
	if err != nil {
		t.Fatalf("Load parent: %v", err)
	}
	if !IsTerminal(ps) {
		t.Fatalf("the parent is %v, not terminal — nothing has attributed it", ps.GetStatus())
	}

	// EXACTLY ONE attribution, and it names the PARENT. Two would mean a slice
	// attributed itself; none would mean the decision finished unmeasured.
	var got []*orderpb.ExecutionAttributionRecorded
	fb.mu.Lock()
	for _, e := range fb.events {
		if e.EventType != EventTypeAttributed {
			continue
		}
		if a, ok := e.Payload.(*orderpb.ExecutionAttributionRecorded); ok {
			got = append(got, a)
		}
	}
	fb.mu.Unlock()
	if len(got) != 1 {
		var ids []string
		for _, a := range got {
			ids = append(ids, a.GetOrderId())
		}
		t.Fatalf("%d attributions published for one decision (%v), want exactly 1 naming the "+
			"parent. A parent worked in N slices must produce ONE answer to \"what did this order "+
			"cost\", not N+1", len(got), ids)
	}
	a := got[0]
	if a.GetOrderId() != parent.GetOrderId() {
		t.Fatalf("the attribution names %q, want the parent %q", a.GetOrderId(), parent.GetOrderId())
	}
	if a.GetSlices() != 2 || a.GetMeasuredSlices() != 2 {
		t.Errorf("slices/measured = %d/%d, want 2/2 — the parent is measured from the children "+
			"that traded it, and its own aggregate is empty by design",
			a.GetSlices(), a.GetMeasuredSlices())
	}
	if a.GetQuality() != orderpb.AttributionQuality_ATTRIBUTION_QUALITY_DECOMPOSED {
		t.Fatalf("quality = %s, want DECOMPOSED", a.GetQuality())
	}
	if got := dec.FromProto(a.GetFilledQuantity()).RatString(); got != "60" {
		t.Errorf("filled_quantity = %s, want 60 — the parent's own filled_quantity is zero by "+
			"design, so this figure can only come from folding the children", got)
	}

	for _, c := range []struct {
		name string
		got  *commonpb.Decimal
		want string
	}{
		{"shortfall_bps", a.GetShortfallBps(), "250"},
		{"spread_bps", a.GetSpreadBps(), "10"},
		{"timing_bps", a.GetTimingBps(), "200"},
		{"impact_bps", a.GetImpactBps(), "40"},
	} {
		if c.got == nil {
			t.Errorf("%s is UNSET on a DECOMPOSED record", c.name)
			continue
		}
		if g := dec.FromProto(c.got).RatString(); g != c.want {
			t.Errorf("%s = %s, want %s", c.name, g, c.want)
		}
	}
	// THE LEG THAT MATTERS MOST HERE, asserted on its own so a change that moved
	// all four consistently still has to face it: most of this parent's cost was
	// the market moving, and the report has to say so.
	if dec.Cmp(a.GetTimingBps(), a.GetImpactBps()) <= 0 {
		t.Fatalf("timing %s is not larger than impact %s, on a scenario constructed so that four "+
			"fifths of the cost was drift the algorithm did not cause. A decomposition that cannot "+
			"show this is not telling a desk anything a single slippage number did not",
			dec.FromProto(a.GetTimingBps()).RatString(), dec.FromProto(a.GetImpactBps()).RatString())
	}

	sum := new(big.Rat).Add(dec.FromProto(a.GetSpreadBps()), dec.FromProto(a.GetTimingBps()))
	sum.Add(sum, dec.FromProto(a.GetImpactBps()))
	if sum.Cmp(dec.FromProto(a.GetShortfallBps())) != 0 {
		t.Fatalf("the legs do not sum to the shortfall: %s != %s",
			sum.RatString(), dec.FromProto(a.GetShortfallBps()).RatString())
	}
}
