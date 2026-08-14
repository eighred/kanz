package costwatch

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// EXECUTION COST REACHES A DASHBOARD, BY VENUE (#436).
//
// internal/execution/tca proves the arithmetic. These prove the fold: that a
// fill FACT becomes a measurement, that an unmeasurable one is COUNTED rather
// than scored zero, and that the venue label — the whole point, since the
// question is which venue to use — actually arrives.

func d(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

func filledPayload(t *testing.T, venue string, arrival, price, qty int64, fee int64, ccy string) []byte {
	t.Helper()
	fill := &orderpb.Fill{
		FillId: "f-1", OrderId: "o-1", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, Quantity: d(qty, 0), Price: d(price, 0), Venue: venue,
	}
	if ccy != "" {
		fill.Fee = &commonpb.Money{Amount: d(fee, 0), CurrencyCode: ccy}
	}
	st := &orderpb.OrderState{
		OrderId: "o-1", InstrumentId: "BTC-USD", Venue: venue,
		Side: orderpb.Side_SIDE_BUY,
	}
	if arrival > 0 {
		st.ArrivalPrice = d(arrival, 0)
	}
	b, err := proto.Marshal(&orderpb.OrderFilled{OrderId: "o-1", Fill: fill, State: st})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newWatch(t *testing.T) (*Watch, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return New(reg, "__system__", slog.New(slog.DiscardHandler)), reg
}

func env() *envelopepb.Envelope {
	return &envelopepb.Envelope{EventId: "e-1", EventType: EventTypeFilled, TenantId: "__system__"}
}

// A FILL BECOMES A MEASUREMENT, LABELLED BY ITS VENUE.
func TestHandle_MeasuresAFillByVenue(t *testing.T) {
	w, reg := newWatch(t)

	// arrival 100, bought 10 @ 110, fee 0 ⇒ 1000 bps.
	if err := w.Handle(context.Background(), env(),
		filledPayload(t, "XBIN", 100, 110, 10, 0, "USD")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	n, sum := histogram(t, reg, "kanz_execution_shortfall_bps", "XBIN")
	if n != 1 {
		t.Fatalf("observations = %d, want 1 — the fill did not reach the measurement", n)
	}
	if sum != 1000 {
		t.Errorf("shortfall = %v bps, want 1000", sum)
	}
	// THE VENUE LABEL IS THE POINT. An unlabelled aggregate says trading cost
	// something; a per-venue distribution says WHICH venue, which is the
	// difference between a number and a decision.
	if _, ok := histogramFor(t, reg, "kanz_execution_shortfall_bps", "XOKX"); ok {
		t.Error("a series appeared for a venue that had no fills")
	}
}

// FEES ARE IN THE HEADLINE FIGURE AND SEPARABLE FROM IT. A venue can execute
// well and charge badly; one blended number hides which.
func TestHandle_ReportsCostWithAndWithoutFees(t *testing.T) {
	w, reg := newWatch(t)

	// arrival 100, filled 10 @ 100 (perfect), fee 10 on 1000 notional = 100 bps.
	if err := w.Handle(context.Background(), env(),
		filledPayload(t, "XBIN", 100, 100, 10, 10, "USD")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, sum := histogram(t, reg, "kanz_execution_price_shortfall_bps", "XBIN"); sum != 0 {
		t.Errorf("price shortfall = %v, want 0 — the fill was exactly at arrival", sum)
	}
	if _, sum := histogram(t, reg, "kanz_execution_shortfall_bps", "XBIN"); sum != 100 {
		t.Fatalf("total shortfall = %v bps, want 100. Dropping fees ranks a zero-fee venue "+
			"with poor fills above a maker-rebate venue with good ones", sum)
	}
}

// AN UNMEASURABLE FILL IS COUNTED BY REASON, NOT SCORED ZERO.
//
// This is the failure that matters most: scoring a fill with no arrival mark as
// zero-cost would drag every venue average toward whichever venue trades the
// instruments the price spine covers worst — the exact opposite of what a venue
// comparison is for. And counting it by REASON is what separates a price-spine
// coverage problem from a venue-adapter one; a single bucket makes both look
// like the same quiet system.
func TestHandle_NoArrivalMarkIsCountedNotScoredZero(t *testing.T) {
	w, reg := newWatch(t)

	if err := w.Handle(context.Background(), env(),
		filledPayload(t, "XBIN", 0, 110, 10, 0, "USD")); err != nil { // arrival absent
		t.Fatalf("Handle: %v", err)
	}
	if n, _ := histogramFor(t, reg, "kanz_execution_shortfall_bps", "XBIN"); n {
		t.Fatal("an unmeasurable fill was OBSERVED into the cost histogram — it would read as " +
			"a zero-cost execution and flatter whichever venue the price spine covers worst")
	}
	if got := counter(t, reg, "kanz_execution_unmeasurable_fills_total", "XBIN", "no_arrival_mark"); got != 1 {
		t.Fatalf("unmeasurable{no_arrival_mark} = %v, want 1 — the gap must be visible and "+
			"attributable, not silent", got)
	}
}

// GARBAGE IS ACKED AND COUNTED. A nack would redeliver bytes that fail the same
// way forever while occupying the consumer real fills arrive on; the fill FACT
// is durable regardless of whether this handler could read it.
func TestHandle_UndecodableIsAckedAndCounted(t *testing.T) {
	w, reg := newWatch(t)

	if err := w.Handle(context.Background(), env(), []byte("not a proto")); err != nil {
		t.Fatalf("Handle(garbage) = %v, want nil (ack)", err)
	}
	if got := counter(t, reg, "kanz_execution_unmeasurable_fills_total", "", "undecodable"); got != 1 {
		t.Errorf("undecodable count = %v, want 1", got)
	}
}

// A FILL FROM ANOTHER TENANT IS REFUSED. Inert while this OMS serves
// __system__, which is what makes adopting it safe today and correct when
// per-tenant compute lands (#97).
func TestHandle_CrossTenantIsRefused(t *testing.T) {
	reg := prometheus.NewRegistry()
	w := New(reg, "acme", slog.New(slog.DiscardHandler))

	e := env()
	e.TenantId = "someone-else"
	if err := w.Handle(context.Background(), e,
		filledPayload(t, "XBIN", 100, 110, 10, 0, "USD")); err == nil {
		t.Fatal("a fill from another tenant was measured into this tenant's cost series")
	}
}

// --- metric readers ---

func histogram(t *testing.T, reg *prometheus.Registry, name, venue string) (uint64, float64) {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelled(m.GetLabel(), venue) && m.GetHistogram() != nil {
				return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
			}
		}
	}
	t.Fatalf("no %s series for venue %q", name, venue)
	return 0, 0
}

func histogramFor(t *testing.T, reg *prometheus.Registry, name, venue string) (bool, bool) {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelled(m.GetLabel(), venue) {
				return true, true
			}
		}
	}
	return false, false
}

func counter(t *testing.T, reg *prometheus.Registry, name, venue, reason string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			var gotVenue, gotReason string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "venue":
					gotVenue = l.GetValue()
				case "reason":
					gotReason = l.GetValue()
				}
			}
			if gotVenue == venue && gotReason == reason {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelled(labels []*dto.LabelPair, venue string) bool {
	for _, l := range labels {
		if l.GetName() == "venue" && l.GetValue() == venue {
			return true
		}
	}
	return false
}

// AN OUT-OF-DOMAIN EXPONENT IS REFUSED BEFORE ANY NUMBER IS READ (#95).
//
// Decimal.exponent is an unvalidated wire field and dec.FromProto materialises
// 10^abs(exponent). A FACT carrying {1, 2000000000} does not produce a wrong
// cost — it NEVER RETURNS. The handler stops acking, the subscription stalls
// behind that one message, and the service still reports healthy. That is worse
// than a wrong number, because nothing about it looks like a failure.
func TestHandle_RefusesAnOutOfDomainExponent(t *testing.T) {
	w, reg := newWatch(t)

	payload, err := proto.Marshal(&orderpb.OrderFilled{
		OrderId: "o-1",
		Fill: &orderpb.Fill{
			FillId: "f-1", OrderId: "o-1", InstrumentId: "BTC-USD", Venue: "XBIN",
			Side: orderpb.Side_SIDE_BUY, Quantity: d(1, 0),
			Price: &commonpb.Decimal{Coefficient: 1, Exponent: 2_000_000_000},
		},
		State: &orderpb.OrderState{OrderId: "o-1", ArrivalPrice: d(100, 0)},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- w.Handle(context.Background(), env(), payload) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Handle = %v, want nil (ack)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Handle did not return — the exponent was materialised instead of refused, and " +
			"the subscription would stall behind this one message while the pod reports healthy")
	}
	if got := counter(t, reg, "kanz_execution_unmeasurable_fills_total", "", "undecodable"); got != 1 {
		t.Errorf("out-of-domain count = %v, want 1", got)
	}
}
