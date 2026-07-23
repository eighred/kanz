package main

import (
	"strings"
	"testing"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/internal/dec"
)

// marshal is the test helper for building a real wire payload — mirrors the
// proto.Marshal idiom in services/webhook-ingest/internal/ingest/integration_test.go.
func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %T: %v", m, err)
	}
	return b
}

func TestDecodeLifecycle_Filled(t *testing.T) {
	fill := &orderpb.Fill{
		FillId:       "fill-1",
		OrderId:      "order-1",
		InstrumentId: "BTC-USD",
		Quantity:     dec.ToProto(dec.Rat("1")),
		Price:        dec.ToProto(dec.Rat("50000")),
		Venue:        "XNAS",
	}
	state := &orderpb.OrderState{
		OrderId:         "order-1",
		OrderedQuantity: dec.ToProto(dec.Rat("1")),
		FilledQuantity:  dec.ToProto(dec.Rat("1")),
		LeavesQuantity:  dec.ToProto(dec.Rat("0")),
		Status:          orderpb.OrderStatus_ORDER_STATUS_FILLED,
	}
	payload := marshal(t, &orderpb.OrderFilled{
		OrderId: "order-1",
		Fill:    fill,
		State:   state,
	})
	env := &envelopepb.Envelope{EventType: "order.order.filled"}

	got, ok := decodeLifecycle(env, payload)
	if !ok {
		t.Fatalf("decodeLifecycle: ok=false, want true")
	}
	if got.Type != "order.order.filled" {
		t.Errorf("Type = %q, want %q", got.Type, "order.order.filled")
	}
	if got.OrderID != "order-1" {
		t.Errorf("OrderID = %q, want %q", got.OrderID, "order-1")
	}
	if got.Detail == "" {
		t.Errorf("Detail is empty, want a rendered fill detail")
	}
	if !strings.Contains(got.Detail, "50000") || !strings.Contains(got.Detail, "1") {
		t.Errorf("Detail = %q, want it to carry the fill qty/price", got.Detail)
	}
}

func TestDecodeLifecycle_Rejected(t *testing.T) {
	payload := marshal(t, &orderpb.OrderRejected{
		OrderId:   "order-2",
		Reason:    "compliance breach",
		ErrorCode: "COMPLIANCE_BREACH",
	})
	env := &envelopepb.Envelope{EventType: "order.order.rejected"}

	got, ok := decodeLifecycle(env, payload)
	if !ok {
		t.Fatalf("decodeLifecycle: ok=false, want true")
	}
	if got.OrderID != "order-2" {
		t.Errorf("OrderID = %q, want %q", got.OrderID, "order-2")
	}
	if got.Detail == "" {
		t.Errorf("Detail is empty, want reason/error_code rendered")
	}
}

func TestDecodeLifecycle_Cancelled(t *testing.T) {
	payload := marshal(t, &orderpb.OrderCancelled{
		OrderId:           "order-3",
		CancelledQuantity: dec.ToProto(dec.Rat("2.5")),
	})
	env := &envelopepb.Envelope{EventType: "order.order.cancelled"}

	got, ok := decodeLifecycle(env, payload)
	if !ok {
		t.Fatalf("decodeLifecycle: ok=false, want true")
	}
	if got.OrderID != "order-3" {
		t.Errorf("OrderID = %q, want %q", got.OrderID, "order-3")
	}
	if !strings.Contains(got.Detail, "2.5") {
		t.Errorf("Detail = %q, want the exact wire quantity 2.5 (never a rounded float)", got.Detail)
	}
}

func TestDecodeLifecycle_UnknownEventType(t *testing.T) {
	// order.order.accepted carries no operator-facing detail at this row
	// density and is deliberately unrendered — this is not a guess, it is a
	// design choice recorded in decode.go's default case.
	payload := marshal(t, &orderpb.OrderAccepted{OrderId: "order-4"})
	env := &envelopepb.Envelope{EventType: "order.order.accepted"}

	_, ok := decodeLifecycle(env, payload)
	if ok {
		t.Errorf("decodeLifecycle: ok=true for an unrendered event_type, want false")
	}

	// A genuinely unknown event_type must also be dropped, not surfaced.
	_, ok = decodeLifecycle(&envelopepb.Envelope{EventType: "order.order.made_up"}, []byte{})
	if ok {
		t.Errorf("decodeLifecycle: ok=true for an unknown event_type, want false")
	}
}

func TestDecodePosition(t *testing.T) {
	payload := marshal(t, &domainpb.PositionState{
		PortfolioId:  "fund-alpha",
		InstrumentId: "BTC-USD",
		Quantity:     dec.ToProto(dec.Rat("1.5")),
		AveragePrice: dec.ToProto(dec.Rat("48000.25")),
	})
	env := &envelopepb.Envelope{EventType: "risk.position.changed"}

	got, ok := decodePosition(env, payload)
	if !ok {
		t.Fatalf("decodePosition: ok=false, want true")
	}
	if got.Portfolio != "fund-alpha" {
		t.Errorf("Portfolio = %q, want %q", got.Portfolio, "fund-alpha")
	}
	if got.Instrument != "BTC-USD" {
		t.Errorf("Instrument = %q, want %q", got.Instrument, "BTC-USD")
	}
	if got.Quantity != "1.5" {
		t.Errorf("Quantity = %q, want exact wire string %q", got.Quantity, "1.5")
	}
	if got.AvgPrice != "48000.25" {
		t.Errorf("AvgPrice = %q, want exact wire string %q", got.AvgPrice, "48000.25")
	}
}

func TestDecodePosition_InvalidPayload(t *testing.T) {
	_, ok := decodePosition(&envelopepb.Envelope{EventType: "risk.position.changed"}, []byte{0xff, 0xff, 0xff})
	if ok {
		t.Errorf("decodePosition: ok=true for an unparseable payload, want false")
	}
}
