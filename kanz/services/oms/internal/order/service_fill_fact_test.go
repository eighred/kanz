package order

// #292'S "VERIFIED WHEN" FOR THE FILL PAIR, AT THE SERVICE LEVEL AND AGAINST
// THE ENGINE.
//
//	TEST_POSTGRES_URL=… go test -p 1 -run TestFillCommitsItsFactWithTheOrder ./services/oms/...
//
// The fill is the pair #292's own table omitted and the one that most needed
// converting: a fill is money that moved, it has no `*_announced_at` marker, and
// completeTerminalOutcome states in its own comment that it CANNOT rebuild the
// ORDER_FILLED FACT from the stored aggregate — the individual Fill (fill_id,
// price, venue_execution_id) is not persisted anywhere else. So before this,
// "the Save committed and the publish failed" meant the trade was gone: not
// late, gone, with tv-sync, accounting and audit never learning it happened.
//
// The in-memory twin of this
// (TestSubmit_ResumesInterruptedFillAnnouncement_FillFactFails) proves the
// control flow. THIS one proves it over a real transaction, which is the only
// place "committed together" means anything, and over a real marshal/unmarshal
// of the payload bytes — a fill FACT that decoded wrong would publish a
// different execution from the one that happened.

import (
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

func TestFillCommitsItsFactWithTheOrder(t *testing.T) {
	pool := newPool(t) // skips unless TEST_POSTGRES_URL is set
	ctx := testCtx()

	// The broker refuses the fill FACT — the blip that used to lose the trade.
	fb := &fakeBus{failOn: EventTypeFilled}
	store := NewPostgres(pool)
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")}), nil, nil,
		WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2)) // marketable ⇒ fills in full, one fill
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("Handle returned nil after the fill publish failed; a FACT that did not reach the " +
			"broker must nack, outbox or no outbox")
	}

	// 1. THE FOLD IS COMMITTED. Same as before the change — this is the half that
	//    was never the problem.
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after the failed publish: %v", err)
	}
	if got := st.GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("committed status = %v, want FILLED — the fold Saved before the publish was attempted", got)
	}

	// 2. AND SO IS ITS FACT. This is the property. Asserting the row without the
	//    record would pass against the old code too, which is exactly how the
	//    fill pair went unnoticed until #293.
	pending, err := store.Outbox().Pending(ctx, cmd.GetOrderId(), 10)
	if err != nil {
		t.Fatalf("outbox Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("outbox holds %d records for a committed fill whose publish failed, want exactly 1 "+
			"(the ORDER_FILLED FACT). The fill FACT carries the only copy of fill_id, price and "+
			"venue_execution_id this platform has — if it is not here it does not exist (#292)",
			len(pending))
	}
	if got := pending[0].Record.EventType; got != EventTypeFilled {
		t.Fatalf("queued record is %s, want %s", got, EventTypeFilled)
	}
	// It survives the BYTEA round trip as the event it was, with the venue's own
	// fill id. A record that decodes to something else would publish a different
	// execution from the one that happened, and the position book dedups on
	// fill_id.
	event, err := pending[0].Record.Event()
	if err != nil {
		t.Fatalf("the queued fill FACT does not decode: %v — the relay cannot publish it, so it will "+
			"sit at the head of this order's key blocking every FACT behind it", err)
	}
	filled, ok := event.Payload.(*orderpb.OrderFilled)
	if !ok {
		t.Fatalf("the queued fill FACT decoded to %T, want *orderpb.OrderFilled", event.Payload)
	}
	if filled.GetFill().GetFillId() == "" {
		t.Fatal("the queued ORDER_FILLED carries no fill_id — the position book claims fill_id before " +
			"folding, so a FACT without one either double-counts the fund's position or is refused")
	}
	if filled.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("the queued FACT carries state %v, want FILLED — it must describe the state the "+
			"same transaction committed", filled.GetState().GetStatus())
	}

	// 3. THE BROKER RECOVERS AND THE RELAY DELIVERS IT — no restart, no sweep, no
	//    second command, and no compensator inventing a fill it cannot know.
	fb.mu.Lock()
	fb.failOn = ""
	fb.mu.Unlock()
	sent, err := svc.Outbox().DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if sent != 1 {
		t.Fatalf("the relay published %d records, want 1", sent)
	}
	types := fb.types()
	filledAt := indexOf(types, EventTypeFilled)
	if filledAt == -1 {
		t.Fatalf("no ORDER_FILLED FACT after the relay ran: %v", types)
	}
	if got := countOf(types, EventTypeFilled); got != 1 {
		t.Fatalf("%d ORDER_FILLED FACTs on the bus for one fill, want 1 — a duplicated execution is "+
			"folded by the position book only because it dedups on fill_id; nothing else would", got)
	}
	// AND IT IS STILL BEHIND THE FACTS THAT PRECEDE IT. tv-sync's transition()
	// drops a FACT for an order its projection never admitted, so a fill arriving
	// before the accepted/routed pair is not a delay — it is a fold against a
	// history that never happened.
	acceptedAt := indexOf(types, EventTypeAccepted)
	if acceptedAt == -1 || acceptedAt > filledAt {
		t.Fatalf("ORDER_ACCEPTED is missing or follows the fill: %v", types)
	}
	if routedAt := indexOf(types, EventTypeRouted); routedAt == -1 || routedAt > filledAt {
		t.Fatalf("ORDER_ROUTED is missing or follows the fill: %v", types)
	}
}
