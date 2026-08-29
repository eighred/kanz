package costwatch

import (
	"context"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
)

// ONE EXECUTION IS MEASURED ONCE (#802).
//
// # What it cost
//
// VenueCosts.Observe increments sum, weight and n with no fill-id guard, and it
// is driven from a bus handler on order.order.filled — so every redelivery
// re-observed the same execution. VenueCosts.Preferred reads the mean that
// produces, and that is what decides where the next untargeted order routes.
//
// A venue whose fills happened to be redelivered therefore looked systematically
// cheaper or dearer than it was, and order flow followed. Silently: a biased
// mean is a plausible number. The durable cost FACT was republished per delivery
// too, so the post-trade report CONFIRMED the wrong answer rather than catching
// it.
//
// The decisions set inside venueStat bounds the EVIDENCE floor against
// duplicates. It does not bound the MEAN, which is the number that routes — and
// that distinction is the whole defect.
//
// The only defence was a per-pod 2-minute DedupWindow the OMS never wires (no
// WithDeduper in cmd/oms), and at replicas: 2 each pod would carry its own
// independently-biased mean regardless.

// costFill builds a fill FACT with a chosen fill id, venue and price, so a
// redelivery can be spelled as the SAME bytes and a distinct fill as different
// ones. The package helper fixes both ids, which is exactly what these cases
// have to vary.
func costFill(t *testing.T, orderID, fillID, venue string, arrival, price int64) []byte {
	t.Helper()
	fill := &orderpb.Fill{
		FillId: fillID, OrderId: orderID, InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, Quantity: d(1, 0), Price: d(price, 0), Venue: venue,
	}
	st := &orderpb.OrderState{
		OrderId: orderID, InstrumentId: "BTC-USD", Venue: venue,
		Side: orderpb.Side_SIDE_BUY,
	}
	// arrival <= 0 leaves the mark ABSENT, which is what tca refuses with
	// ErrNoArrivalMark — the package helper spells it the same way.
	if arrival > 0 {
		st.ArrivalPrice = d(arrival, 0)
	}
	b, err := proto.Marshal(&orderpb.OrderFilled{OrderId: orderID, Fill: fill, State: st})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// rankerFixture wires a watch over a REAL VenueCosts with an evidence floor of
// one, so a single decision is enough for Preferred to speak and the fold is
// observable through the thing that actually routes.
func rankerFixture(t *testing.T) (*Watch, *execution.VenueCosts, *fakeBus) {
	t.Helper()
	costs := execution.NewVenueCosts(execution.WithMinSamples(1))
	fb := &fakeBus{}
	return New(prometheus.NewRegistry(), "__system__", fb, costs, slog.New(slog.DiscardHandler)), costs, fb
}

func costEnv() *envelopepb.Envelope {
	return &envelopepb.Envelope{EventType: EventTypeFilled, TenantId: "__system__"}
}

// THE HEADLINE. The same delivery twice must not be measured twice.
func TestARedeliveredFillIsMeasuredOnce(t *testing.T) {
	w, _, fb := rankerFixture(t)
	ctx := context.Background()
	payload := costFill(t, "o-802", "f-802", "BINANCE", 100, 101)

	if err := w.Handle(ctx, costEnv(), payload); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if len(fb.events) != 1 {
		t.Fatalf("the first delivery published %d cost FACT(s), want 1 — the fixture never "+
			"measured anything and everything below would compare two absences", len(fb.events))
	}

	// THE REDELIVERY: the same bytes on the same subject, which is what the
	// broker sends after a nack, a rebalance, or an ack that did not land.
	if err := w.Handle(ctx, costEnv(), payload); err != nil {
		t.Fatalf("redelivery: %v", err)
	}

	if got := len(fb.events); got != 1 {
		t.Fatalf("%d cost FACTs published for ONE execution.\n\n"+
			"This is #802. The same delivery also re-observed the ranker's mean, and "+
			"VenueCosts.Preferred reads that number to decide where the next untargeted order "+
			"routes — so order flow follows bytes the broker happened to send twice. The durable "+
			"TCA report inherits the duplication, which is why post-trade analysis confirms the "+
			"biased routing instead of catching it.", got)
	}
}

// AND THE RANKING IS UNCHANGED. The mean is the mechanism; which venue wins is
// what the fix is protecting.
func TestARedeliveryDoesNotChangeWhichVenueIsPreferred(t *testing.T) {
	w, costs, _ := rankerFixture(t)
	ctx := context.Background()

	// BINANCE executes at the arrival mark; OKX 10% through it. BINANCE is the
	// cheaper venue by construction.
	if err := w.Handle(ctx, costEnv(), costFill(t, "o-a", "f-a", "BINANCE", 100, 100)); err != nil {
		t.Fatalf("binance fill: %v", err)
	}
	dear := costFill(t, "o-b", "f-b", "OKX", 100, 110)
	if err := w.Handle(ctx, costEnv(), dear); err != nil {
		t.Fatalf("okx fill: %v", err)
	}

	before, ok := costs.Preferred([]string{"BINANCE", "OKX"})
	if !ok {
		t.Fatal("the ranker abstained with a floor of one and two venues observed — no ranking " +
			"was produced, so the assertion below compares nothing")
	}

	// Redeliver the dearer venue's single execution. Every one of these used to
	// add weight to a venue that traded once.
	for i := range 5 {
		if err := w.Handle(ctx, costEnv(), dear); err != nil {
			t.Fatalf("redelivery %d: %v", i, err)
		}
	}

	after, okAfter := costs.Preferred([]string{"BINANCE", "OKX"})
	if !okAfter || after != before {
		t.Fatalf("the preferred venue went from %q to %q (ok=%v) on redeliveries of ONE "+
			"execution — routing is a capital decision and it moved on duplicate bytes",
			before, after, okAfter)
	}
}

// NON-VACUITY, AND IT IS THE ARM THAT MATTERS. A DISTINCT fill must still be
// measured — without it both cases above pass on a handler that measures nothing
// after the first fill it ever sees, which would starve the ranker rather than
// protect it.
func TestADistinctFillIsStillMeasured(t *testing.T) {
	w, costs, fb := rankerFixture(t)
	ctx := context.Background()

	if err := w.Handle(ctx, costEnv(), costFill(t, "o-1", "f-1", "BINANCE", 100, 101)); err != nil {
		t.Fatalf("first fill: %v", err)
	}
	// A SECOND VENUE, because Preferred deliberately abstains on fewer than two
	// candidates — "nothing to choose between" — so a one-venue assertion would
	// prove nothing about whether the fold reached the ranker.
	if err := w.Handle(ctx, costEnv(), costFill(t, "o-2", "f-2", "OKX", 100, 105)); err != nil {
		t.Fatalf("second fill: %v", err)
	}

	if got := len(fb.events); got != 2 {
		t.Fatalf("%d cost FACTs for two DISTINCT executions, want 2 — the claim is refusing real "+
			"measurements", got)
	}
	if _, ok := costs.Preferred([]string{"BINANCE", "OKX"}); !ok {
		t.Fatal("the ranker has no evidence after two distinct fills — the claim suppressed real " +
			"measurements, which starves routing rather than protecting it")
	}
}

// A FILL WITH NO ID IS REFUSED, as the books refuse it.
//
// fillfact.Validate — which the position store and the accounting ledger both
// run — rejects it under ErrNotIdentified. A fill neither book will fold must
// not rank venues; and with no id there is nothing to claim on, so every
// redelivery of it would count again.
func TestAnUnidentifiedFillIsNotMeasured(t *testing.T) {
	w, costs, fb := rankerFixture(t)
	ctx := context.Background()

	if err := w.Handle(ctx, costEnv(), costFill(t, "o-3", "", "BINANCE", 100, 101)); err != nil {
		t.Fatalf("unidentified fill: %v", err)
	}
	if _, ok := costs.Preferred([]string{"BINANCE", "OKX"}); ok {
		t.Fatal("a fill with no fill_id was measured into the ranker. The position book and the " +
			"ledger both refuse it, so this ranks venues on an execution the platform does not " +
			"believe happened — and with no id it cannot be deduped, so every redelivery counts " +
			"again")
	}
	if got := len(fb.events); got != 0 {
		t.Fatalf("%d cost FACT(s) published for an unidentified fill, want 0", got)
	}
}

// AN UNMEASURABLE FILL STAYS CLAIMABLE, and that is why the claim sits AFTER the
// measurement rather than before it.
//
// A fill the price spine cannot value yet produced no observation to protect. If
// the claim were taken first it would be spent on a delivery that measured
// nothing, and the later delivery — once the instrument has an arrival mark —
// would be refused as a duplicate. That is not a double count avoided; it is a
// measurement permanently lost, and the venue's mean would be short by every
// fill that ever arrived early.
func TestAFillThatCouldNotBeMeasuredIsStillClaimableLater(t *testing.T) {
	w, costs, fb := rankerFixture(t)
	ctx := context.Background()

	// No arrival mark: tca refuses it with ErrNoArrivalMark.
	if err := w.Handle(ctx, costEnv(), costFill(t, "o-4", "f-4", "BINANCE", 0, 101)); err != nil {
		t.Fatalf("unmeasurable delivery: %v", err)
	}
	if got := len(fb.events); got != 0 {
		t.Fatalf("%d cost FACT(s) published for a fill that could not be measured, want 0 — the "+
			"fixture did not reproduce an unmeasurable fill and the assertion below proves nothing",
			got)
	}

	// The same fill, redelivered once the mark exists.
	if err := w.Handle(ctx, costEnv(), costFill(t, "o-4", "f-4", "BINANCE", 100, 101)); err != nil {
		t.Fatalf("measurable redelivery: %v", err)
	}
	if got := len(fb.events); got != 1 {
		t.Fatalf("%d cost FACT(s) after the fill became measurable, want 1 — the claim was spent "+
			"on a delivery that measured nothing, so this execution is permanently absent from "+
			"the venue's mean", got)
	}
	if err := w.Handle(ctx, costEnv(), costFill(t, "o-5", "f-5", "OKX", 100, 105)); err != nil {
		t.Fatalf("second venue: %v", err)
	}
	if _, ok := costs.Preferred([]string{"BINANCE", "OKX"}); !ok {
		t.Fatal("the ranker still has no evidence — the recovered measurement never reached it")
	}
}
