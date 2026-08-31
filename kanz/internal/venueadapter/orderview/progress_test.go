package orderview

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// The write half of the order view (#904).
//
// Before Progress existed, the ONLY thing that ever advanced an order to a
// terminal status here was Server.recordStatus on a venue-confirmed cancel. A
// FILLED order stayed at whatever status the OMS handed Execute, forever — so
// Open kept returning it, the reconciler kept spending REST weight re-querying
// it, healedState kept finding permanent drift and re-emitting a StateHealed
// FACT about it every pass, and #891's terminal-order eviction could never reach
// it because eviction is keyed on GOING terminal and it never did.
//
// Two properties are load-bearing in OPPOSITE directions, and both are held
// here:
//
//   - A FILLED order must leave Open, or the leak above is unclosed.
//   - A PARTIALLY_FILLED order must NOT, because it is still live at the
//     exchange. Marking it terminal would hide a working order from the healing
//     watchdog, which is a far worse defect than the leak.

func qty(units int64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: units, Exponent: 0}
}

// venueReport is the shape both connectors' ingesters hand Progressed: the
// venue's own status plus the quantities it observed, and NOT an order's terms.
// That partiality is the reason Progress merges rather than replaces.
func venueReport(id string, status orderpb.OrderStatus, filled, leaves int64) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:        id,
		Status:         status,
		FilledQuantity: qty(filled),
		LeavesQuantity: qty(leaves),
	}
}

func TestProgressToFilledTakesTheOrderOutOfOpen(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Record(ctx, working("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	if open, _ := m.Open(ctx); len(open) != 1 {
		t.Fatalf("Open before the fill = %d orders, want 1", len(open))
	}

	if err := Progress(ctx, m, venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_FILLED, 1, 0)); err != nil {
		t.Fatalf("Progress: %v", err)
	}

	st, ok, err := m.Get(ctx, "o1")
	if err != nil || !ok {
		t.Fatalf("Get after the fill: ok=%v err=%v", ok, err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status after the fill = %v, want FILLED — the adapter's view disagrees with the "+
			"FACT it published (#904)", st.GetStatus())
	}
	open, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("Open still returns %d order(s) after the fill — the reconciler will re-query it "+
			"at the exchange and re-emit StateHealed on every pass, forever (#904)", len(open))
	}
}

// THE OPPOSITE DIRECTION, AND THE ONE THAT MATTERS MORE. A partial fill is a
// LIVE order. If this ever starts passing with the order absent from Open, the
// healing watchdog has gone blind to an order still working at the exchange.
func TestProgressToPartiallyFilledKeepsTheOrderOpen(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Record(ctx, working("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := Progress(ctx, m, venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 1, 1)); err != nil {
		t.Fatalf("Progress: %v", err)
	}

	st, ok, err := m.Get(ctx, "o1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Fatalf("status = %v, want PARTIALLY_FILLED", st.GetStatus())
	}
	if Terminal(st.GetStatus()) {
		t.Fatal("PARTIALLY_FILLED is being treated as terminal — a partially filled order is STILL " +
			"LIVE at the exchange, and calling it finished hides it from the healing watchdog")
	}
	open, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(open) != 1 || open[0].GetOrderId() != "o1" {
		t.Fatalf("Open returns %d order(s) after a PARTIAL fill, want the order itself — a live "+
			"order has disappeared from the healing watchdog's view, which is worse than the "+
			"leak #904 closes", len(open))
	}
	if got := open[0].GetFilledQuantity().GetCoefficient(); got != 1 {
		t.Fatalf("the still-open order carries filled=%d, want 1 — the reconciler compares this "+
			"against venue truth and would report permanent drift", got)
	}
}

// THE BOUND, MEASURED. The shape of #891's TestTerminalOrdersDoNotAccumulate,
// on the path that issue could not reach: orders that finish by FILLING.
func TestFilledOrdersDoNotAccumulate(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	m := memoryAt(clk)
	ctx := context.Background()

	const orders = 10_000
	step := DefaultTerminalRetention / (orders / 100) // 100 retention windows
	for i := range orders {
		id := fmt.Sprintf("ord-%d", i)
		if err := m.Record(ctx, working(id)); err != nil {
			t.Fatalf("record working %s: %v", id, err)
		}
		// The fill path, exactly as the user-data ingester drives it.
		if err := Progress(ctx, m, venueReport(id, orderpb.OrderStatus_ORDER_STATUS_FILLED, 1, 0)); err != nil {
			t.Fatalf("progress %s: %v", id, err)
		}
		clk.add(step)
	}

	const bound = 100 * 2 * 2 // rate/window * amortization * slack
	m.mu.RLock()
	got := len(m.orders)
	m.mu.RUnlock()
	if got > bound {
		t.Fatalf("Memory.orders holds %d entries after %d FILLED orders, want <= %d — the fill "+
			"path is still unbounded, which is the half #891's evictor could not reach (#904)",
			got, orders, bound)
	}
	open, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("the reconciler would re-query %d filled orders at the exchange on the next "+
			"pass, against a rate-limited weight budget (#904)", len(open))
	}
	t.Logf("%d orders filled, %d entries retained, %d believed open", orders, got, len(open))
}

// A REPORT CARRIES A STATUS AND A SIZE, NOT AN ORDER'S TERMS. Recording it
// wholesale would drop the twenty fields the ingester does not reconstruct —
// the #405/#240 shape, a field the system had and then quietly did not.
func TestProgressKeepsTheOrdersTerms(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	full := &orderpb.OrderState{
		OrderId:         "o1",
		PortfolioId:     "fund-alpha",
		InstrumentId:    "BTC-USD",
		Status:          orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		OrderedQuantity: qty(2),
		StopPrice:       qty(49000),
		ParentOrderId:   "parent-1",
		Leverage:        qty(3),
		MarginMode:      orderpb.MarginMode_MARGIN_MODE_CROSS,
		VenueAccountId:  "acct-7",
		ArrivalPrice:    qty(50000),
	}
	if err := m.Record(ctx, full); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := Progress(ctx, m, venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_FILLED, 2, 0)); err != nil {
		t.Fatalf("Progress: %v", err)
	}

	st, _, err := m.Get(ctx, "o1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, c := range []struct {
		field string
		got   any
		want  any
	}{
		{"portfolio_id", st.GetPortfolioId(), "fund-alpha"},
		{"instrument_id", st.GetInstrumentId(), "BTC-USD"},
		{"ordered_quantity", st.GetOrderedQuantity().GetCoefficient(), int64(2)},
		{"stop_price", st.GetStopPrice().GetCoefficient(), int64(49000)},
		{"parent_order_id", st.GetParentOrderId(), "parent-1"},
		{"leverage", st.GetLeverage().GetCoefficient(), int64(3)},
		{"margin_mode", st.GetMarginMode(), orderpb.MarginMode_MARGIN_MODE_CROSS},
		{"venue_account_id", st.GetVenueAccountId(), "acct-7"},
		{"arrival_price", st.GetArrivalPrice().GetCoefficient(), int64(50000)},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v after the fill, want %v — Progress replaced the order instead of "+
				"merging the venue's report onto it, dropping a field the adapter was given",
				c.field, c.got, c.want)
		}
	}
	// And it did take what the exchange actually observed.
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED || st.GetFilledQuantity().GetCoefficient() != 2 {
		t.Fatalf("status/filled = %v/%d, want FILLED/2", st.GetStatus(), st.GetFilledQuantity().GetCoefficient())
	}
}

// A quantity the report did not set must not blank one the view holds: a
// nulled filled_quantity reads downstream as an order that traded nothing.
func TestProgressDoesNotBlankAQuantityTheReportOmitted(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	st := working("o1")
	st.FilledQuantity = qty(1)
	if err := m.Record(ctx, st); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := Progress(ctx, m, &orderpb.OrderState{
		OrderId: "o1", Status: orderpb.OrderStatus_ORDER_STATUS_FILLED,
	}); err != nil {
		t.Fatalf("Progress: %v", err)
	}
	got, _, err := m.Get(ctx, "o1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetFilledQuantity().GetCoefficient() != 1 {
		t.Fatalf("filled_quantity = %v, want the 1 already recorded — an omitted field in the "+
			"report erased a quantity the view knew", got.GetFilledQuantity())
	}
}

// UNSPECIFIED is what both connectors' status tables answer for a venue string
// they do not know. It is "the venue said something we cannot read", never "no
// status", and it must not overwrite what this adapter does know.
func TestProgressRefusesAnUnmappedStatus(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Record(ctx, working("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	err := Progress(ctx, m, &orderpb.OrderState{
		OrderId: "o1", Status: orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED, FilledQuantity: qty(1),
	})
	if !errors.Is(err, ErrUnmappedStatus) {
		t.Fatalf("Progress with an unmapped status returned %v, want ErrUnmappedStatus", err)
	}
	st, _, gErr := m.Get(ctx, "o1")
	if gErr != nil {
		t.Fatalf("Get: %v", gErr)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_ROUTED {
		t.Fatalf("status = %v, want the ROUTED already recorded — an unreadable venue status "+
			"replaced what the adapter knew with what it does not", st.GetStatus())
	}
}

// A late trade report — one executed before a cancel the venue then confirmed,
// arriving on the websocket after it — must not put the order back into Open.
func TestProgressDoesNotReopenATerminalOrder(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Record(ctx, cancelled("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	err := Progress(ctx, m, venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 1, 1))
	if !errors.Is(err, ErrTerminalNotReopened) {
		t.Fatalf("Progress on a cancelled order returned %v, want ErrTerminalNotReopened", err)
	}
	open, oErr := m.Open(ctx)
	if oErr != nil {
		t.Fatalf("Open: %v", oErr)
	}
	if len(open) != 0 {
		t.Fatal("a cancelled order was put back into Open by a late fill report — the re-query, " +
			"the duplicate StateHealed and the deferred eviction all restart")
	}
}

// Terminal to terminal IS allowed: the venue saying FILLED about an order we
// recorded CANCELLED is venue truth about a trade that happened, and the
// retention clock keeps its FIRST terminal sighting (#891) so this cannot
// refresh the entry forever.
func TestProgressAcceptsVenueTruthOnATerminalOrder(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	m := memoryAt(clk)
	if err := m.Record(ctx, cancelled("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	firstTerminal := clk.t
	clk.add(DefaultTerminalRetention / 2)
	if err := Progress(ctx, m, venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_FILLED, 1, 0)); err != nil {
		t.Fatalf("Progress: %v", err)
	}
	st, _, err := m.Get(ctx, "o1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED (venue truth)", st.GetStatus())
	}
	m.mu.RLock()
	at := m.orders["o1"].terminalAt
	m.mu.RUnlock()
	if !at.Equal(firstTerminal) {
		t.Fatalf("terminalAt = %v, want the first sighting %v — a re-record refreshed the "+
			"retention clock and the entry would never be evicted", at, firstTerminal)
	}
}

// An order this adapter has no record of is REFUSED rather than created: the
// report is a partial reconstruction, and writing it would put an order into the
// view with its terms absent.
func TestProgressRefusesAnOrderNotInTheView(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	err := Progress(ctx, m, venueReport("ghost", orderpb.OrderStatus_ORDER_STATUS_FILLED, 1, 0))
	if !errors.Is(err, ErrNotInView) {
		t.Fatalf("Progress for an unknown order returned %v, want ErrNotInView", err)
	}
	if _, ok, gErr := m.Get(ctx, "ghost"); ok || gErr != nil {
		t.Fatalf("the unknown order was written into the view anyway (ok=%v err=%v)", ok, gErr)
	}
}

// The Seam is the connector's face on all of this, and it must report rather
// than swallow: a view that stops advancing is a reconciler that never stops
// re-querying.
func TestSeamProgressedReportsARefusal(t *testing.T) {
	var got []error
	seam := NewSeam(NewMemory(), func(err error) { got = append(got, err) })
	seam.Progressed(venueReport("ghost", orderpb.OrderStatus_ORDER_STATUS_FILLED, 1, 0))
	if len(got) != 1 || !errors.Is(got[0], ErrNotInView) {
		t.Fatalf("Seam.Progressed reported %v, want one ErrNotInView — a silent refusal here is "+
			"an order that never goes terminal and nothing that says so", got)
	}
}

// And it advances the view for real, through the same Store the watchdog reads.
func TestSeamProgressedAdvancesTheView(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Record(ctx, working("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	seam := NewSeam(m, func(err error) { t.Fatalf("unexpected report: %v", err) })
	seam.Progressed(venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_FILLED, 1, 0))
	if open := seam.OpenOrders(); len(open) != 0 {
		t.Fatalf("OpenOrders returns %d after the fill, want 0", len(open))
	}
	if st, ok := seam.Lookup("o1"); !ok || st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("Lookup = %v (ok=%v), want a FILLED order still readable for a late report", st, ok)
	}
}
