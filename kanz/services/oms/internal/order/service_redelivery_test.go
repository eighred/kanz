package order

import (
	"context"
	"errors"
	"sync"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"bytes"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"google.golang.org/protobuf/proto"
)

// unreachableVenue fails its FIRST execute and then recovers — a venue call
// that timed out, a 500, an adapter that had lost its connection and got it
// back. By the time the first failure fires the order has already been admitted,
// routed, and SAVED as ROUTED.
//
// The recovery is load-bearing in this test, not incidental. A venue that failed
// forever could never demonstrate a successful resume; it would only ever prove
// that the redelivery errored again. The behaviour under test is that delivery 2
// ASKS and then WORKS the order, so the venue has to be able to work it.
//
// QueryOrder is delegated to the embedded SimVenue, which is the honest model:
// the EXECUTE call failed and recorded nothing, so the venue truthfully answers
// that it has no such order.
type unreachableVenue struct {
	*execution.SimVenue
	mu sync.Mutex
	n  int
}

func (v *unreachableVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	first := v.n == 1
	v.mu.Unlock()
	if first {
		return nil, errors.New("venue unreachable")
	}
	return v.SimVenue.Execute(ctx, st)
}

func (v *unreachableVenue) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}

// THIS TEST REPLACES TestRedeliveryAfterVenueFailureIsAckedWithoutResuming.
//
// That test pinned the gap: delivery 1 created the order and failed at the
// venue; delivery 2 found the record, took it for a duplicate, returned nil, and
// left the order at ROUTED with nothing working it and nothing anywhere saying
// so. Its own doc said "WHEN THIS TEST FAILS, THAT IS THE SIGNAL, NOT A
// REGRESSION. It means handlers have learned to resume."
//
// This is that signal, inverted into an assertion. The redelivery must now ASK
// THE VENUE and act on the answer. The venue never received this order — its
// Execute failed before recording anything — so it answers UNKNOWN, our record
// carries no venue_ack_at, and the only safe action is to work it. It fills.
func TestRedeliveryAfterVenueFailureResumesAgainstVenueTruth(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	venue := &unreachableVenue{SimVenue: execution.NewSimVenue("XSIM")}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	// Delivery 1: admitted, routed, stored as ROUTED, then the venue call fails.
	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the venue failure to surface")
	}
	if got := venue.count(); got != 1 {
		t.Fatalf("venue.Execute called %d times on delivery 1, want exactly 1", got)
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 1: %v", err)
	}
	if st.GetVenueAckAt() != nil {
		t.Fatal("venue_ack_at is set after a FAILED Execute — the ack must be stamped " +
			"only when the venue actually confirmed it holds the order, or the " +
			"quarantine arm fires on healthy orders")
	}

	// Delivery 2: the redelivery that error asked for. The handler must now ask
	// the venue rather than ack on sight.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v, want nil after a successful resume", err)
	}

	st, _, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 2: %v", err)
	}
	if got := st.GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("order status is %v, want FILLED — the venue had no record of this "+
			"order and no ack was ever recorded, so the redelivery had to work it", got)
	}
	if st.GetQuarantine() != nil {
		t.Fatalf("order was quarantined (%q) — the venue affirmatively said UNKNOWN "+
			"and we held no ack, which is the one combination that is safe to re-drive",
			st.GetQuarantine().GetReason())
	}
}

// THE DOUBLE-TRADE ARM. This is the assertion the whole design exists to make.
//
// The venue confirmed it held this order, and now denies it. Exactly one of the
// two records is wrong and nothing here can tell which. Re-driving trades the
// fund twice if ours is right; abandoning strands a live exchange order if the
// venue is right. The order freezes, and it must NOT reach the venue again.
func TestVenueDenyingAnAcknowledgedOrderQuarantinesAndDoesNotRedrive(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	// amnesiacVenue acknowledges an execute (no error) but then has no record of
	// the order — a venue that lost its order book, or answered from a replica
	// that never saw the write.
	venue := &amnesiacVenue{SimVenue: execution.NewSimVenue("XSIM")}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetVenueAckAt() == nil {
		t.Fatal("venue_ack_at not stamped after a SUCCESSFUL Execute — without it the " +
			"quarantine arm below can never fire, and this venue's contradiction " +
			"would be re-driven as if it were a fresh order")
	}
	before := venue.executes()

	// The redelivery. The venue now denies the order it acknowledged.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v; a quarantine is terminal and must ack", err)
	}

	st, _, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 2: %v", err)
	}
	if st.GetQuarantine() == nil {
		t.Fatal("order was NOT quarantined. The venue acknowledged it and now reports " +
			"UNKNOWN — that is two authorities contradicting each other, and acting " +
			"on either one is a coin flip with the fund's money")
	}
	if st.GetQuarantine().GetReason() == "" {
		t.Error("quarantine carries no reason — an operator sees a frozen order and no account of why")
	}
	if got := venue.executes(); got != before {
		t.Fatalf("venue.Execute called %d more times — the quarantined order was "+
			"RE-DRIVEN, which is the double trade this whole design exists to prevent",
			got-before)
	}
}

// amnesiacVenue executes successfully but never remembers: every QueryOrder
// answers UNKNOWN. It models a venue whose order book was lost or whose read
// replica never saw the write.
type amnesiacVenue struct {
	*execution.SimVenue
	mu sync.Mutex
	n  int
}

func (v *amnesiacVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	v.mu.Unlock()
	// Deliberately does NOT delegate to SimVenue.Execute: this venue must
	// acknowledge without recording, so the query below can contradict it.
	return nil, nil
}

func (v *amnesiacVenue) QueryOrder(context.Context, *orderpb.OrderState) (execution.OrderView, error) {
	return execution.OrderView{State: execution.OrderViewUnknown}, nil
}

func (v *amnesiacVenue) executes() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}

// A venue that cannot be asked is not a venue we may guess about. Every real
// out-of-process adapter is in this state today, because venue.v1's wire
// contract has no query RPC.
func TestVenueWithoutQuerierQuarantinesRatherThanGuessing(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	venue := &muteVenue{}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the venue failure to surface")
	}
	before := venue.executes()

	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v; a quarantine is terminal and must ack", err)
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetQuarantine() == nil {
		t.Fatal("an order at a venue that implements no Querier was not quarantined — " +
			"nothing can establish what that venue did, and the alternative to " +
			"freezing is guessing")
	}
	if got := venue.executes(); got != before {
		t.Fatalf("venue.Execute called %d more times on a venue nobody can query", got-before)
	}
}

// muteVenue implements Venue and nothing else — no Querier. This is every
// out-of-process GRPCVenue today.
type muteVenue struct {
	mu sync.Mutex
	n  int
}

func (v *muteVenue) MIC() string     { return "XMUTE" }
func (v *muteVenue) Account() string { return "acct-mute" }
func (v *muteVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	v.mu.Unlock()
	return nil, errors.New("venue unreachable")
}
func (v *muteVenue) executes() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}

// twoFillVenue acknowledges an execute (no error, nothing recorded — same shape
// as amnesiacVenue) and then, when queried, reports the order FILLED by TWO
// fills instead of amnesiacVenue's UNKNOWN. It models the first multi-fill
// venue to implement execution.Querier — none does today; SimVenue always
// reports exactly one full-leaves fill.
type twoFillVenue struct {
	*execution.SimVenue
	fills []*orderpb.Fill
}

func (v *twoFillVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	// Deliberately does NOT delegate to SimVenue.Execute or record anything: the
	// order must reach ROUTED with venue_ack_at set so the redelivery below takes
	// the Load-found-it path into resume(), exactly as amnesiacVenue's test does.
	return nil, nil
}

func (v *twoFillVenue) QueryOrder(context.Context, *orderpb.OrderState) (execution.OrderView, error) {
	return execution.OrderView{State: execution.OrderViewFilled, Fills: v.fills}, nil
}

// A MULTI-FILL VENUE VIEW IS ADOPTED, AND ADOPTING IT TWICE CHANGES NOTHING (#782).
//
// This test used to assert the opposite. adopt() refused any view carrying more
// than one fill and quarantined the order, because ApplyFill folds a fill into
// the aggregate with an over-fill guard and no fill_id dedup — so folding a fill
// the order already contained would silently double-count filled_quantity and
// re-weight average_fill_price, with nothing downstream able to notice.
//
// THE REFUSAL WAS CORRECT AND IT WAS A CEILING. Both wired connectors report
// multiple fills for one order under ordinary partial execution, so any order
// that partially filled and then needed healing froze and waited for a human —
// more often as volume rose.
//
// order_fills is the guarantee that retires it: Store.Save claims the fill in
// the same transaction as the state and the FACT, and returns ErrFillApplied
// having written nothing if the aggregate already holds it.
//
// WHAT THIS TEST PROVES, AND WHAT IT DOES NOT. It proves the ceiling is gone:
// a view carrying two fills is adopted, both fold exactly once, and the weighted
// average is right. It does NOT prove the dedup — a redelivery for an order that
// is already terminal announces the trailing outcome and returns without
// reaching the fold at all, so this test passes with the claim deleted. That was
// measured, not assumed: it was written first, it passed with the dedup removed,
// and fill_claim_test.go exists because of it.
//
// The re-adoption arm below is still worth keeping. It says a second delivery
// changes nothing an operator or a ledger would see, which is the property the
// issue asked for — it is just not, on its own, evidence of the claim.
func TestAdoptFoldsEveryFillOnceAndReadoptionIsANoOp(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	fills := []*orderpb.Fill{
		{FillId: "e1", OrderId: "o1", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_BUY,
			Quantity: d(50, 0), Price: d(1020, -2), Venue: "XSIM"},
		{FillId: "e2", OrderId: "o1", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_BUY,
			Quantity: d(50, 0), Price: d(1030, -2), Venue: "XSIM"},
	}
	venue := &twoFillVenue{SimVenue: execution.NewSimVenue("XSIM"), fills: fills}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	// Delivery 1: admitted and routed. The venue acks without recording anything,
	// so the order is stored ROUTED with venue_ack_at set and NO fills folded.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetVenueAckAt() == nil {
		t.Fatal("venue_ack_at not stamped after delivery 1 — the adoption below needs the order " +
			"routed and acknowledged before the redelivery queries the venue")
	}
	if !dec.IsZero(st.GetFilledQuantity()) {
		t.Fatalf("filled_quantity = %v after delivery 1, want 0 — nothing was folded yet",
			st.GetFilledQuantity())
	}

	// Delivery 2: the redelivery. The venue reports BOTH fills at once.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2: %v", err)
	}
	st, _, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 2: %v", err)
	}
	if q := st.GetQuarantine(); q != nil {
		t.Fatalf("the order was QUARANTINED (%q). A multi-fill view is the ordinary shape of an "+
			"interrupted partial execution, and refusing it is the ceiling #782 removed",
			q.GetReason())
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED — both fills together complete the 100 ordered",
			st.GetStatus())
	}
	if got := dec.Str(dec.FromProto(st.GetFilledQuantity())); got != "100" {
		t.Fatalf("filled_quantity = %s, want 100 — both fills must fold, exactly once each", got)
	}
	// 50 @ 10.20 + 50 @ 10.30 = 10.25 weighted. A double-fold of either fill
	// would move this, which is the number the old refusal was protecting.
	if got := dec.Str(dec.FromProto(st.GetAverageFillPrice())); got != "10.25" {
		t.Fatalf("average_fill_price = %s, want 10.25", got)
	}

	before := financialBytes(t, st)

	// Delivery 3: the SAME view again. Every fill is already claimed, so every
	// fold is skipped and the aggregate must not move by one bit.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 3 (re-adoption): %v", err)
	}
	after, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 3: %v", err)
	}
	if !bytes.Equal(before, financialBytes(t, after)) {
		t.Fatalf("re-adopting the same venue view CHANGED the aggregate.\n  before: filled=%s avg=%s\n"+
			"  after:  filled=%s avg=%s\nA fill this order already contains was folded a second "+
			"time — the double-count #782 exists to make impossible",
			dec.Str(dec.FromProto(st.GetFilledQuantity())), dec.Str(dec.FromProto(st.GetAverageFillPrice())),
			dec.Str(dec.FromProto(after.GetFilledQuantity())), dec.Str(dec.FromProto(after.GetAverageFillPrice())))
	}
}

// growingFillVenue answers a CUMULATIVE view that GROWS between queries, which
// is the shape execution.Querier actually has: Fills is what the venue
// attributes to the order, so every answer repeats the fills the previous one
// carried. twoFillVenue above returns the same two fills every time and is
// therefore only ever adopted from scratch; this one models the state that
// really occurs — some of the venue's fills already folded, the rest not.
//
// Views past the last one repeat it, so a redelivery beyond the script is
// answered rather than panicking: a venue that has stopped changing its mind is
// the ordinary end state, not a test error.
type growingFillVenue struct {
	*execution.SimVenue
	mu    sync.Mutex
	n     int
	views []execution.OrderView
}

func (v *growingFillVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	// Deliberately does NOT delegate to SimVenue.Execute or record anything: the
	// order must reach ROUTED with venue_ack_at set so the redeliveries below take
	// the Load-found-it path into resume(), exactly as twoFillVenue's test does.
	return nil, nil
}

func (v *growingFillVenue) QueryOrder(context.Context, *orderpb.OrderState) (execution.OrderView, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	i := v.n
	if i >= len(v.views) {
		i = len(v.views) - 1
	}
	v.n++
	return v.views[i], nil
}

func (v *growingFillVenue) queries() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}

// A FILL THIS ORDER ALREADY HOLDS IS SKIPPED BEFORE THE FOLD, NOT AFTER IT (#798).
//
// #782 made the order aggregate dedup its own fills by claiming each fill_id in
// order_fills inside the transaction that folds it. That claim is the guarantee
// and it still is — but adopt() folded FIRST and claimed SECOND, so Store.Save
// only ever got to answer ErrFillApplied for a fill ApplyFill had already
// accepted. ApplyFill refuses any fill larger than the REMAINING leaves, so a
// re-presented fill was protected only while its quantity still fit.
//
// THE FAILURE, WHICH IS THE ORDINARY SHAPE AND NOT AN EDGE CASE. Order for 100.
// The venue fills it 60 then 40, and Querier.Fills is cumulative. The first
// adoption sees [A(60)] and folds it; the order is durably PARTIALLY_FILLED with
// 40 leaves. The next delivery is offered [A(60), B(40)] — and iteration one
// hands ApplyFill a 60 against 40 leaves. OVERFILL. The order QUARANTINES, and
// B is never reached: a 40-unit execution the platform does not record, on an
// order frozen for a human. That is worse than the double-count #782 prevented,
// because a double-count is visible in the numbers and a fill nobody recorded is
// not.
//
// WHY THIS TEST AND NOT TestAdoptFoldsEveryFillOnceAndReadoptionIsANoOp. That one
// uses 50/50 with nothing folded beforehand, so its first adoption takes both
// fills and terminates the order; its re-adoption arm never reaches the fold at
// all, because a redelivery for a terminal order announces the trailing outcome
// and returns. Equal fills and a clean start are exactly the two conditions that
// hide this. Here the fills are UNEQUAL and the first is already folded, which is
// the only combination that puts an over-fill in front of the claim.
func TestAdoptSkipsAFillAlreadyFoldedRatherThanOverfillingOnIt(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	a := &orderpb.Fill{
		FillId: "a60", OrderId: "o1", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_BUY,
		Quantity: d(60, 0), Price: d(1020, -2), Venue: "XSIM",
	}
	b := &orderpb.Fill{
		FillId: "b40", OrderId: "o1", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_BUY,
		Quantity: d(40, 0), Price: d(1030, -2), Venue: "XSIM",
	}
	venue := &growingFillVenue{
		SimVenue: execution.NewSimVenue("XSIM"),
		views: []execution.OrderView{
			// Query 1: the venue has filled 60 of the 100.
			{State: execution.OrderViewPartiallyFilled, Fills: []*orderpb.Fill{a}},
			// Query 2: the remaining 40 has since traded, and the view repeats the
			// 60 because it is cumulative. This is the answer that used to freeze.
			{State: execution.OrderViewFilled, Fills: []*orderpb.Fill{a, b}},
		},
	}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	// Delivery 1: admitted and routed. The venue acks without recording anything,
	// so the order is stored ROUTED with venue_ack_at set and NO fills folded.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 1: %v", err)
	}
	if st.GetVenueAckAt() == nil {
		t.Fatal("venue_ack_at not stamped after delivery 1 — the adoptions below need the order " +
			"routed and acknowledged before the redeliveries query the venue")
	}

	// Delivery 2: the first adoption. It folds A and leaves the order OPEN, which
	// is the state the defect needs.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2: %v", err)
	}
	st, _, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 2: %v", err)
	}
	// THE NON-VACUITY ARM. Without it every assertion after delivery 3 would also
	// pass on a handler that folded NOTHING at either delivery: no fold means no
	// over-fill, and an order still sitting at ROUTED is not quarantined either.
	// The defect is only in front of the loop once A is genuinely in the
	// aggregate.
	if got := dec.Str(dec.FromProto(st.GetFilledQuantity())); got != "60" {
		t.Fatalf("filled_quantity = %s after delivery 2, want 60 — the first adoption did not fold "+
			"A, so nothing below is testing what it claims to", got)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Fatalf("status = %v after delivery 2, want PARTIALLY_FILLED — the order must still be "+
			"OPEN with 40 leaves, or delivery 3 never reaches the fold at all", st.GetStatus())
	}
	if q := st.GetQuarantine(); q != nil {
		t.Fatalf("the order was QUARANTINED on the FIRST adoption (%q)", q.GetReason())
	}

	// Delivery 3: the venue's cumulative view now carries A and B. A is already
	// folded; B is not.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 3: %v", err)
	}
	if got := venue.queries(); got != 2 {
		t.Fatalf("the venue was queried %d times, want 2 — delivery 3 did not reach adoption, so "+
			"the assertions below prove nothing", got)
	}
	st, _, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 3: %v", err)
	}
	if q := st.GetQuarantine(); q != nil {
		t.Fatalf("the order was QUARANTINED (%q). The venue re-presented A(60) — which this order "+
			"already holds — against 40 remaining leaves, and the over-fill guard fired on it "+
			"BEFORE the claim could refuse the fold. B(40) was never reached: a real execution "+
			"the platform does not record, on an order frozen for a human", q.GetReason())
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED — A(60) is already folded and B(40) completes the 100 "+
			"ordered", st.GetStatus())
	}
	if got := dec.Str(dec.FromProto(st.GetFilledQuantity())); got != "100" {
		t.Fatalf("filled_quantity = %s, want 100 — B was not folded", got)
	}
	// 60 @ 10.20 + 40 @ 10.30 = 10.24 weighted. A re-fold of A would move this,
	// which is the number #782's claim protects and this change must not weaken.
	if got := dec.Str(dec.FromProto(st.GetAverageFillPrice())); got != "10.24" {
		t.Fatalf("average_fill_price = %s, want 10.24", got)
	}

	// EXACTLY ONE FACT PER FILL, over the whole run. This is the arm that says the
	// skip is a SKIP and not a second fold: A announced once (as a partial) and B
	// once (as the fill that completed the order). A re-folded A would announce
	// twice, and a B that never folded would announce not at all.
	if got := fb.count(EventTypePartiallyFilled); got != 1 {
		t.Errorf("%d ORDER_PARTIALLY_FILLED FACTs published, want exactly 1 — one for A(60)", got)
	}
	if got := fb.count(EventTypeFilled); got != 1 {
		t.Errorf("%d ORDER_FILLED FACTs published, want exactly 1 — one for B(40), the fill that "+
			"completed the order", got)
	}
}

// zeroFillVenue acknowledges an execute (no error, nothing recorded — same
// shape as amnesiacVenue and twoFillVenue) and then, when queried, reports the
// order FILLED but supplies NO fill to fold. It models a venue whose answer is
// truthful about the outcome but omits the fill record — an adapter bug, or a
// partial/degraded response — which is the failure direction opposite of
// twoFillVenue's too-many-fills.
type zeroFillVenue struct {
	*execution.SimVenue
}

func (v *zeroFillVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	// Deliberately does NOT delegate to SimVenue.Execute or record anything: the
	// order must reach ROUTED with venue_ack_at set so the redelivery below takes
	// the Load-found-it path into resume(), exactly as amnesiacVenue's test does.
	return nil, nil
}

func (v *zeroFillVenue) QueryOrder(context.Context, *orderpb.OrderState) (execution.OrderView, error) {
	return execution.OrderView{State: execution.OrderViewFilled}, nil // no Fills
}

// THE ZERO-FILL ADOPT ARM.
//
// adopt() guarded len(view.Fills) > 1 against a double-fold but had nothing
// guarding the empty case. A venue answering FILLED or PARTIALLY_FILLED with
// an empty Fills slice used to stamp venue_ack_at, run a for loop whose body
// never executes, and return nil: the command ACKED, the order stuck at
// ROUTED forever, no fill folded, no position and no ledger entry created —
// and every future startup sweep re-asks the same venue, gets the same answer,
// and repeats exactly nothing. That is a live position the platform has
// permanently forgotten, precisely the failure this whole design exists to
// end. The fix quarantines instead of silently acking.
func TestAdoptQuarantinesFilledViewWithZeroFills(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	venue := &zeroFillVenue{SimVenue: execution.NewSimVenue("XSIM")}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	// Delivery 1: admitted and routed. The venue acks without recording
	// anything, so the order is stored ROUTED with venue_ack_at set and NO
	// fills folded — same setup as the multi-fill test above.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetVenueAckAt() == nil {
		t.Fatal("venue_ack_at not stamped after delivery 1 — the zero-fill arm below needs the " +
			"order routed and acknowledged before the redelivery queries the venue")
	}
	if !dec.IsZero(st.GetFilledQuantity()) {
		t.Fatalf("filled_quantity = %v after delivery 1, want 0 — nothing was folded yet", st.GetFilledQuantity())
	}

	// Delivery 2: the redelivery. The venue now reports FILLED with zero fills.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v; a quarantine is terminal and must ack", err)
	}

	st, _, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 2: %v", err)
	}
	if st.GetQuarantine() == nil {
		t.Fatal("order was NOT quarantined. The venue reported FILLED with zero fills, and " +
			"silently accepting that would leave a traded order stuck at ROUTED with nothing " +
			"folded, forever")
	}
	if st.GetQuarantine().GetReason() == "" {
		t.Error("quarantine carries no reason — an operator sees a frozen order and no account of why")
	}
	if !dec.IsZero(st.GetFilledQuantity()) {
		t.Fatalf("filled_quantity = %v after the redelivery, want 0 — nothing was ever reported "+
			"to fold, so nothing should have been folded", st.GetFilledQuantity())
	}
}

// TestAdmissionPathHoldsTheClaimWhileWorkingAnOrder asserts FINDING 1's
// invariant directly rather than trying to win a race: a claim taken for an
// order_id BEFORE that order_id's SubmitOrder is admitted must block the
// admission path from ever reaching the venue. Before the fix, handleSubmit
// called s.work without taking the claim at all, so nothing here would have
// stopped it — this test would have passed for the wrong reason (the venue
// would run because nothing contended for it). It is written against the
// documented interleaving instead: hold the claim exactly as a concurrent
// resume() would, and prove the admission delivery backs off.
func TestAdmissionPathHoldsTheClaimWhileWorkingAnOrder(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	venue := &amnesiacVenue{SimVenue: execution.NewSimVenue("XSIM")}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	// Simulate a concurrent claim holder — the same thing resume() would do if it
	// raced this admission delivery for the order.
	release, ok := svc.claim(cmd.GetOrderId())
	if !ok {
		t.Fatal("claim: order_id was not free at test start")
	}
	defer release()

	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("Handle returned %v, want nil — losing the claim is safe to ack, not an error", err)
	}
	if got := venue.executes(); got != 0 {
		t.Fatalf("venue.Execute called %d times — the admission path worked the order without "+
			"taking the claim, racing whatever else holds it", got)
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		t.Fatalf("stored status = %v, want PENDING_NEW — the order is created and ACCEPTED but "+
			"never routed, because working it requires the claim this delivery could not get",
			st.GetStatus())
	}
}

// financialBytes is the order's state with the two fields a re-adoption is
// ENTITLED to move cleared, so the comparison is over everything else.
//
// A BYTE COMPARISON WITH TWO NAMED EXCLUSIONS, NOT A LIST OF FIELDS TO CHECK.
// Asserting filled_quantity and average_fill_price by hand would pass if a
// double-fold moved leaves_quantity, the status, or a field nobody has added
// yet; the whole point of #782's dedup is that NOTHING about the aggregate moves
// on a second fold. So the test states what may change and compares the rest.
//
//	as_of                 every write stamps it; a no-op write still writes.
//	venue_ack_at          resume() re-records that the venue has the order before
//	                      it folds anything, which is correct and is why a
//	                      re-adoption is not literally a no-op at the storage layer.
//	outcome_announced_at  the redelivery finds the order already terminal and
//	                      announces the trailing CommandOutcome, stamping its
//	                      marker. That is the #238 compensator shape working, not
//	                      a fold.
//
// EACH ONE WAS FOUND BY THE COMPARISON FAILING, not assumed in advance — which
// is the argument for comparing bytes rather than four fields by hand. If any of
// them ever carried financial meaning this helper would be hiding it; none does.
// as_of is a write clock, venue_ack_at a possession marker, outcome_announced_at
// an announcement marker, and the money lives in the quantities, the price and
// the status — all of which are compared.
func financialBytes(t *testing.T, st *orderpb.OrderState) []byte {
	t.Helper()
	c, ok := proto.Clone(st).(*orderpb.OrderState)
	if !ok {
		t.Fatal("clone returned a different type")
	}
	c.AsOf = nil
	c.VenueAckAt = nil
	c.OutcomeAnnouncedAt = nil
	b, err := proto.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
