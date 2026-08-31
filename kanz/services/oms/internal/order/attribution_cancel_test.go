package order

// THE TERMINAL PATHS A DECISION CAN END ON, OTHER THAN A LIVE FILL (#866).
//
// A decision is attributed at whichever transition made it terminal, and there
// are four of them. attribution_postgres_test.go proves the live fill and the
// scheduled parent's retirement against the engine, where "committed together"
// means something. These are the other two — a WITHDRAWAL and an ADOPTION — plus
// the coverage case a deployment hits every day: an instrument the price spine
// prints but never quotes.
//
// They need no database: what they exercise is which Save carries the
// measurement and what the measurement says, which the MemoryStore shows as well
// as Postgres does.

import (
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

// TestAWithdrawnOrderIsMeasuredOnWhatItActuallyTraded.
//
// A partially-filled order that an operator pulls is a real, common and
// EXPENSIVE decision, and it is the one they most often want explained: half the
// order went at a price they can see, and the rest never went at all. It is
// measured on what traded — the quantity that never traded has no cost — and the
// terminal status on the record is what tells a reader the remainder was a
// withdrawal rather than a miss.
//
// The state is seeded rather than traded into place because SimVenue fills in
// full or not at all, so there is no way to reach a partially-filled live order
// through it. What is under test is the cancel path's Save, not the fold that
// produced the fill.
func TestAWithdrawnOrderIsMeasuredOnWhatItActuallyTraded(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	svc, store := newServiceNoVenue(t, fb)

	seeded := &orderpb.OrderState{
		OrderId:          "withdrawn1",
		PortfolioId:      "fund-alpha",
		InstrumentId:     "BTC-USD",
		Side:             orderpb.Side_SIDE_BUY,
		Status:           orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		OrderedQuantity:  d(100, 0),
		FilledQuantity:   d(40, 0),
		LeavesQuantity:   d(60, 0),
		AverageFillPrice: d(1025, -2),
		ArrivalPrice:     d(10, 0),
		ReleasePrice:     d(10, 0),
		ReleaseBid:       d(999, -2),
		ReleaseAsk:       d(1001, -2),
	}
	if err := store.Create(ctx, seeded, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cancel := &orderpb.CancelOrder{
		Metadata: &commandpb.CommandMetadata{
			TargetId:            seeded.GetOrderId(),
			Issuer:              ScheduleIssuer,
			PrincipalPortfolios: []string{seeded.GetPortfolioId()},
		},
		OrderId: seeded.GetOrderId(),
	}
	if err := svc.handleCancel(ctx, mustMarshal(t, cancel)); err != nil {
		t.Fatalf("handleCancel: %v", err)
	}

	after, _, err := store.Load(ctx, seeded.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if after.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status = %v, want CANCELLED — the cancel did not land and this test measures "+
			"nothing", after.GetStatus())
	}

	a, _ := fb.last(EventTypeAttributed).(*orderpb.ExecutionAttributionRecorded)
	if a == nil {
		t.Fatal("a withdrawn order that had already traded 40 units published no attribution. " +
			"Its cost is real and it is exactly the decision an operator asks about afterwards")
	}
	if a.GetTerminalStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Errorf("terminal_status = %s, want CANCELLED — without it a reader cannot tell the "+
			"untraded 60 from a miss", a.GetTerminalStatus())
	}
	if got := dec.FromProto(a.GetFilledQuantity()).RatString(); got != "40" {
		t.Errorf("filled_quantity = %s, want 40 — the measurement must cover what traded, not what "+
			"was ordered", got)
	}
	if got := dec.FromProto(a.GetOrderedQuantity()).RatString(); got != "100" {
		t.Errorf("ordered_quantity = %s, want 100", got)
	}
	// Same market as the other scenarios: 10.25 achieved against a 10.00 decision
	// mid, quoted 9.99 / 10.01, released at the decision instant.
	for _, c := range []struct {
		name string
		got  *commonpb.Decimal
		want string
	}{
		{"shortfall_bps", a.GetShortfallBps(), attrShortall},
		{"spread_bps", a.GetSpreadBps(), attrSpread},
		{"timing_bps", a.GetTimingBps(), attrTiming},
		{"impact_bps", a.GetImpactBps(), attrImpact},
	} {
		if c.got == nil {
			t.Errorf("%s is UNSET", c.name)
			continue
		}
		if g := dec.FromProto(c.got).RatString(); g != c.want {
			t.Errorf("%s = %s, want %s", c.name, g, c.want)
		}
	}
}

// TestAWithdrawnOrderThatNeverTradedPublishesNothing. Nothing was bought, so
// nothing was paid. A zero here would be a real datapoint claiming a perfect
// execution that never happened.
func TestAWithdrawnOrderThatNeverTradedPublishesNothing(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	svc, store := newServiceNoVenue(t, fb)

	seeded := &orderpb.OrderState{
		OrderId:         "withdrawn2",
		PortfolioId:     "fund-alpha",
		InstrumentId:    "BTC-USD",
		Side:            orderpb.Side_SIDE_BUY,
		Status:          orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		OrderedQuantity: d(100, 0),
		LeavesQuantity:  d(100, 0),
		ArrivalPrice:    d(10, 0),
		ReleasePrice:    d(10, 0),
		ReleaseBid:      d(999, -2),
		ReleaseAsk:      d(1001, -2),
	}
	if err := store.Create(ctx, seeded, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cancel := &orderpb.CancelOrder{
		Metadata: &commandpb.CommandMetadata{
			TargetId:            seeded.GetOrderId(),
			Issuer:              ScheduleIssuer,
			PrincipalPortfolios: []string{seeded.GetPortfolioId()},
		},
		OrderId: seeded.GetOrderId(),
	}
	if err := svc.handleCancel(ctx, mustMarshal(t, cancel)); err != nil {
		t.Fatalf("handleCancel: %v", err)
	}
	if fb.last(EventTypeAttributed) != nil {
		t.Fatal("an attribution was published for an order that never traded — a zero-cost record " +
			"for an execution that did not happen is a datapoint claiming a perfect fill")
	}
}

// TestAnAdoptedFillAttributesTheDecisionToo.
//
// adopt() is the RECOVERY path: the process that reaches it already crashed
// once, or the venue answered a reconciliation query with fills the OMS had
// never folded. A decision finished there is exactly as measurable as one
// finished on the live path, and leaving it out would make the
// execution-quality report silently thinner for the orders that had trouble —
// which is the population most worth measuring.
//
// The venue reports 60 @ 10.20 and 40 @ 10.30 against a 10.00 decision mid
// quoted 9.99 / 10.01: an achieved 10.24 on 100 units, so 240 bps of shortfall,
// 10 of quoted half-width, no drift, and 230 of impact.
func TestAnAdoptedFillAttributesTheDecisionToo(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	a := &orderpb.Fill{
		FillId: "adopt60", OrderId: "o1", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_BUY,
		Quantity: d(60, 0), Price: d(1020, -2), Venue: "XSIM",
	}
	b := &orderpb.Fill{
		FillId: "adopt40", OrderId: "o1", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_BUY,
		Quantity: d(40, 0), Price: d(1030, -2), Venue: "XSIM",
	}
	venue := &twoFillVenue{SimVenue: execution.NewSimVenue("XSIM"), fills: []*orderpb.Fill{a, b}}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{venue}), nil, nil,
		WithHaltGate(halt.OpenGate(nil)), WithArrivalMarks(attrMarks()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)
	// Delivery 1 admits and routes; the venue acks and records nothing.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	// Delivery 2 finds the order, queries the venue and adopts its two fills.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2: %v", err)
	}

	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED — nothing was adopted and this test measures nothing",
			st.GetStatus())
	}

	rec, _ := fb.last(EventTypeAttributed).(*orderpb.ExecutionAttributionRecorded)
	if rec == nil {
		t.Fatal("a decision finished by ADOPTION published no attribution. The recovery path is " +
			"the population most worth measuring, and a report that quietly omits it is one a " +
			"desk cannot use to compare algorithms")
	}
	for _, c := range []struct {
		name string
		got  *commonpb.Decimal
		want string
	}{
		{"shortfall_bps", rec.GetShortfallBps(), "240"},
		{"spread_bps", rec.GetSpreadBps(), "10"},
		{"timing_bps", rec.GetTimingBps(), "0"},
		{"impact_bps", rec.GetImpactBps(), "230"},
	} {
		if c.got == nil {
			t.Errorf("%s is UNSET", c.name)
			continue
		}
		if g := dec.FromProto(c.got).RatString(); g != c.want {
			t.Errorf("%s = %s, want %s", c.name, g, c.want)
		}
	}
}

// TestAnUnquotedMarketReportsTheTotalAndRefusesTheLegs.
//
// A last-trade mark carries a price and no width. The headline shortfall is
// still exact — it needs only arrival and the fills — but the SPREAD cannot be
// measured, so the decomposition refuses rather than guessing.
//
// THE ALTERNATIVE IS THE FAILURE #866 IS EXPLICIT ABOUT. Scoring the spread at
// zero would claim the fund crossed for free, and because the legs must sum to
// the shortfall the whole unexplained cost would land in impact — the one leg
// nobody can check independently. A desk reading that would go looking for an
// algorithm problem that is really a market-data problem.
func TestAnUnquotedMarketReportsTheTotalAndRefusesTheLegs(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	// A mark with no touch: this instrument was last seen as a trade print.
	marks := fakeMarks{price: dec.Rat(attrArrival), asOf: markObserved, seen: true}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")}), nil, nil,
		WithHaltGate(halt.OpenGate(nil)), WithArrivalMarks(marks))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	rec, _ := fb.last(EventTypeAttributed).(*orderpb.ExecutionAttributionRecorded)
	if rec == nil {
		t.Fatal("no attribution for a measurable decision — the headline shortfall needs only the " +
			"arrival mark and the fills, and both were present")
	}
	if rec.GetQuality() != orderpb.AttributionQuality_ATTRIBUTION_QUALITY_TOTAL_ONLY {
		t.Fatalf("quality = %s, want TOTAL_ONLY — the market's width was never observed",
			rec.GetQuality())
	}
	if rec.GetSpreadBps() != nil || rec.GetImpactBps() != nil || rec.GetTimingBps() != nil {
		t.Fatalf("legs = spread %v / impact %v / timing %v on a TOTAL_ONLY record, want all UNSET",
			rec.GetSpreadBps(), rec.GetImpactBps(), rec.GetTimingBps())
	}
	if got := dec.FromProto(rec.GetShortfallBps()).RatString(); got != attrShortall {
		t.Errorf("shortfall_bps = %s, want %s — the headline survives a missing touch",
			got, attrShortall)
	}
	if rec.GetSlices() != 1 || rec.GetMeasuredSlices() != 0 {
		t.Errorf("slices/measured = %d/%d, want 1/0 — the coverage pair is what separates a "+
			"price-spine gap from a total absence", rec.GetSlices(), rec.GetMeasuredSlices())
	}
}
