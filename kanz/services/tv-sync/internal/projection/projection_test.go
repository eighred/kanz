package projection

import (
	"context"
	"sync"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func d(n int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: n} }

func env(tenant, eventType string) *envelopepb.Envelope {
	return &envelopepb.Envelope{TenantId: tenant, EventType: eventType}
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// filledState builds a fully-filled OrderState + its Fill.
func filledOrder(orderID, account, instrument string, side orderpb.Side, qty, price int64) *orderpb.OrderFilled {
	st := &orderpb.OrderState{
		OrderId: orderID, PortfolioId: account, InstrumentId: instrument, Side: side,
		OrderType: orderpb.OrderType_ORDER_TYPE_MARKET, OrderedQuantity: d(qty),
		FilledQuantity: d(qty), LeavesQuantity: d(0), AverageFillPrice: d(price),
		Status: orderpb.OrderStatus_ORDER_STATUS_FILLED, AsOf: timestamppb.Now(),
	}
	fill := &orderpb.Fill{
		FillId: "fill-" + orderID, OrderId: orderID, InstrumentId: instrument, Side: side,
		Quantity: d(qty), Price: d(price), Venue: "XNAS", ExecutedAt: timestamppb.Now(),
	}
	return &orderpb.OrderFilled{OrderId: orderID, Fill: fill, State: st}
}

func TestHandle_FoldsFillIntoPositionOrderExecutionState(t *testing.T) {
	p := New(time.Now, nil)
	ev := filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100)
	if err := p.Handle(context.Background(), env("acme", evtFilled), mustMarshal(t, ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	pos, ok := p.Positions("acme", "fund-alpha", time.Time{})
	if !ok || len(pos) != 1 || pos[0].Qty != "2" || pos[0].AvgPrice != "100" || pos[0].Side != "long" {
		t.Fatalf("positions = %+v", pos)
	}
	ords, _ := p.Orders("acme", "fund-alpha", time.Time{})
	if len(ords) != 1 || ords[0].Status != "filled" || ords[0].FilledQty != "2" {
		t.Fatalf("orders = %+v", ords)
	}
	execs, _ := p.Executions("acme", "fund-alpha", "", time.Time{})
	if len(execs) != 1 || execs[0].Price != "100" {
		t.Fatalf("executions = %+v", execs)
	}
	st, _ := p.State("acme", "fund-alpha", time.Time{})
	if st.OpenPositions != 1 {
		t.Fatalf("state = %+v", st)
	}
}

func TestHandle_RealizedPnLAcrossFills(t *testing.T) {
	p := New(time.Now, nil)
	// Buy 2 @ 100, then sell 1 @ 150 → realized (150-100)*1 = 50.
	_ = p.Handle(context.Background(), env("acme", evtFilled), mustMarshal(t, filledOrder("o1", "f", "BTC", orderpb.Side_SIDE_BUY, 2, 100)))
	_ = p.Handle(context.Background(), env("acme", evtFilled), mustMarshal(t, filledOrder("o2", "f", "BTC", orderpb.Side_SIDE_SELL, 1, 150)))

	st, _ := p.State("acme", "f", time.Time{})
	if st.RealizedPnl != "50" {
		t.Fatalf("realized = %q, want 50", st.RealizedPnl)
	}
	pos, _ := p.Positions("acme", "f", time.Time{})
	if len(pos) != 1 || pos[0].Qty != "1" {
		t.Fatalf("positions = %+v, want net 1", pos)
	}
}

func TestHandle_BitemporalAsOfKnowledge(t *testing.T) {
	c := &clock{t: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)}
	p := New(c.now, nil)

	_ = p.Handle(context.Background(), env("acme", evtFilled), mustMarshal(t, filledOrder("o1", "f", "BTC", orderpb.Side_SIDE_BUY, 1, 100)))
	t1 := c.now()
	c.advance(time.Hour)
	_ = p.Handle(context.Background(), env("acme", evtFilled), mustMarshal(t, filledOrder("o2", "f", "BTC", orderpb.Side_SIDE_BUY, 1, 100)))

	// As known at t1: only the first fill is visible → net 1.
	pos, _ := p.Positions("acme", "f", t1)
	if len(pos) != 1 || pos[0].Qty != "1" {
		t.Fatalf("as-of-t1 positions = %+v, want net 1", pos)
	}
	// Latest: both fills → net 2.
	pos, _ = p.Positions("acme", "f", time.Time{})
	if pos[0].Qty != "2" {
		t.Fatalf("latest positions = %+v, want net 2", pos)
	}
	execs, _ := p.Executions("acme", "f", "", t1)
	if len(execs) != 1 {
		t.Fatalf("as-of-t1 executions = %d, want 1", len(execs))
	}
}

func TestHandle_TenantIsolation(t *testing.T) {
	p := New(time.Now, nil)
	_ = p.Handle(context.Background(), env("acme", evtFilled), mustMarshal(t, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 1, 100)))

	// Tenant globex cannot see acme's account — not merely denied, not found.
	if _, ok := p.Positions("globex", "fund-alpha", time.Time{}); ok {
		t.Fatal("cross-tenant read returned acme's account")
	}
	if accts := p.Accounts("globex"); len(accts) != 0 {
		t.Fatalf("globex sees %d accounts, want 0", len(accts))
	}
	if accts := p.Accounts("acme"); len(accts) != 1 {
		t.Fatalf("acme sees %d accounts, want 1", len(accts))
	}
}

func TestHandle_StreamDelta(t *testing.T) {
	p := New(time.Now, nil)
	ch, cancel := p.Subscribe("acme", "fund-alpha")
	defer cancel()

	_ = p.Handle(context.Background(), env("acme", evtFilled), mustMarshal(t, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 1, 100)))

	// A fill fans into order + execution + position + state deltas; drain until
	// the three account-level updates have all arrived.
	kinds := map[string]bool{}
	timeout := time.After(2 * time.Second)
	for !(kinds["execution"] && kinds["position"] && kinds["state"]) {
		select {
		case dlt := <-ch:
			kinds[dlt.Kind] = true
		case <-timeout:
			t.Fatalf("only got deltas %v, want execution+position+state", kinds)
		}
	}
}

func TestHandle_FillDedupByFillID(t *testing.T) {
	p := New(time.Now, nil)
	ev := filledOrder("o1", "f", "BTC", orderpb.Side_SIDE_BUY, 2, 100) // fill_id = fill-o1
	// The same fill arrives twice (sync venue path + async ws echo).
	_ = p.Handle(context.Background(), env("acme", evtFilled), mustMarshal(t, ev))
	_ = p.Handle(context.Background(), env("acme", evtFilled), mustMarshal(t, ev))

	pos, _ := p.Positions("acme", "f", time.Time{})
	if len(pos) != 1 || pos[0].Qty != "2" {
		t.Fatalf("positions = %+v, want net 2 (fill folded once, not doubled)", pos)
	}
	execs, _ := p.Executions("acme", "f", "", time.Time{})
	if len(execs) != 1 {
		t.Fatalf("executions = %d, want 1 (deduped by fill_id)", len(execs))
	}
}

func TestHandle_UntenantedSkipped(t *testing.T) {
	p := New(time.Now, nil)
	if err := p.Handle(context.Background(), env("", evtFilled), mustMarshal(t, filledOrder("o1", "f", "BTC", orderpb.Side_SIDE_BUY, 1, 100))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if accts := p.Accounts(""); len(accts) != 0 {
		t.Fatal("an untenanted FACT was folded")
	}
}

// A SLICE OF A WORKED ORDER NAMES ITS PARENT ON THE BLOTTER (#435, #484).
//
// Without it a six-slice order is seven unrelated rows: the parent resting at
// "scheduled" and six children with derived ids that deliberately carry no
// readable relation. An operator looking at the blotter during an incident
// cannot tell which of them belong together, or that pulling the parent is what
// stops the rest.
//
// The grouping itself is the client's — how to present it is a design question.
// This asserts only that the projection stops throwing the answer away.
func TestOrderDTO_AChildNamesItsParent(t *testing.T) {
	child := orderDTO([]orderRev{{
		state: &orderpb.OrderState{
			OrderId: "9d3a45c14832d709db117779a8bfe3a2", PortfolioId: "fund-alpha",
			InstrumentId: "BTC-USD", ParentOrderId: "3f9a1c2e0b7d4e119a6f2c8d5e4b7a13",
			Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		},
		status: orderpb.OrderStatus_ORDER_STATUS_ROUTED,
	}}, "")
	if child.ParentOrderID != "3f9a1c2e0b7d4e119a6f2c8d5e4b7a13" {
		t.Fatalf("ParentOrderID = %q, want the parent — a slice is indistinguishable from an "+
			"unrelated order, so an operator cannot see which rows belong together",
			child.ParentOrderID)
	}
}

// AN ORDINARY ORDER LEAVES IT EMPTY, and `omitempty` keeps it off the wire — so
// a client that has never heard of parent orders sees exactly what it saw before.
func TestOrderDTO_AnOrdinaryOrderNamesNoParent(t *testing.T) {
	ord := orderDTO([]orderRev{{
		state: &orderpb.OrderState{
			OrderId: "o1", PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
			Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		},
		status: orderpb.OrderStatus_ORDER_STATUS_ROUTED,
	}}, "")
	if ord.ParentOrderID != "" {
		t.Errorf("ParentOrderID = %q on an order nobody sliced", ord.ParentOrderID)
	}
}
