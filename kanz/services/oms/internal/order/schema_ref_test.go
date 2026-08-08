package order

import (
	"strings"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/services/oms/internal/outbox"
)

// EVERY FACT'S payload_schema_ref MUST NAME A TYPE THAT EXISTS.
//
// payloadSchemaRef used to take the message's name as a STRING and prefix it
// with "order.v1.". Every FACT here is an order.v1 message except one — the
// universal CommandOutcome, which is command.v1 — so every command outcome the
// OMS published carried "order.v1.CommandOutcome:1", naming a type in no
// descriptor anywhere.
//
// NOTHING FAILED, and that is the part worth a test rather than a fix. An
// unresolvable ref is not an error on the publish path: LAKE-01a treats it as
// envelope-only, so the outcomes landed in the lake with their payloads never
// decoded — indistinguishable from a schema not yet registered, which is a
// legitimate transient state. The defect could only surface somewhere that has
// to RESOLVE the ref, and until #292 moved a command outcome onto the outbox,
// nothing did.
//
// So this asserts the property directly, for every FACT the emitter can build:
// the ref resolves, and the record round-trips back to the same payload. A new
// FACT whose payload comes from a different proto package is caught here rather
// than in a lake nobody is reading yet.
func TestEveryEmittedFactHasAResolvableSchemaRef(t *testing.T) {
	e := NewEmitter(&fakeBus{})
	now := time.Unix(1700000000, 0).UTC()
	st := &orderpb.OrderState{
		OrderId: "o-ref",
		AsOf:    timestamppb.New(now),
		Status:  orderpb.OrderStatus_ORDER_STATUS_FILLED,
	}
	fill := &orderpb.Fill{FillId: "f1", ExecutedAt: timestamppb.New(now)}

	built := map[string]struct {
		ref     string
		payload string
	}{}

	add := func(name, ref, wantType string) {
		built[name] = struct {
			ref     string
			payload string
		}{ref, wantType}
	}

	add("accepted", e.acceptedEvent(st).PayloadSchemaRef, "order.v1.OrderAccepted")
	add("routed", e.routedEvent("o-ref", "XSIM", "", now).PayloadSchemaRef, "order.v1.OrderRouted")
	add("filled", e.fillEvent(fill, st).PayloadSchemaRef, "order.v1.OrderFilled")
	add("outcome", e.outcomeEvent("o-ref",
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "r", "", "", now).PayloadSchemaRef,
		"command.v1.CommandOutcome")

	// NON-VACUITY: if the builders are renamed away, this map empties and every
	// assertion below becomes true of nothing.
	if len(built) < 4 {
		t.Fatalf("only %d FACT builders enumerated — the emitter changed shape and this guard did not "+
			"move with it", len(built))
	}

	for name, got := range built {
		if got.ref != got.payload+":1" {
			t.Errorf("%s: payload_schema_ref = %q, want %q.\n\n"+
				"The ref must name the payload's real proto full name. A ref that names a type in no "+
				"descriptor does not fail on publish — LAKE-01a lands it envelope-only — so the "+
				"payload is silently never decoded.", name, got.ref, got.payload+":1")
		}
		if !strings.HasSuffix(got.ref, ":1") {
			t.Errorf("%s: ref %q carries no schema version", name, got.ref)
		}
	}
}

// AND THE ROUND TRIP, which is what the relay actually does. A ref that parses
// but resolves to the wrong message would pass the assertions above and fail
// here.
func TestEveryEmittedFactRoundTripsThroughTheOutbox(t *testing.T) {
	e := NewEmitter(&fakeBus{})
	ctx := testCtx()
	now := time.Unix(1700000000, 0).UTC()
	st := &orderpb.OrderState{
		OrderId: "o-rt",
		AsOf:    timestamppb.New(now),
		Status:  orderpb.OrderStatus_ORDER_STATUS_FILLED,
	}
	fill := &orderpb.Fill{FillId: "f1", ExecutedAt: timestamppb.New(now)}

	facts := map[string]func() (outbox.Record, error){
		"accepted": func() (outbox.Record, error) { return e.AcceptedFact(ctx, st) },
		"routed":   func() (outbox.Record, error) { return e.RoutedFact(ctx, "o-rt", "XSIM", "", now) },
		"filled":   func() (outbox.Record, error) { return e.FillFact(ctx, fill, st) },
		"outcome": func() (outbox.Record, error) {
			return e.OutcomeFact(ctx, "o-rt",
				commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "amended", "", "", now)
		},
	}

	for name, make := range facts {
		t.Run(name, func(t *testing.T) {
			rec, err := make()
			if err != nil {
				t.Fatalf("build record: %v", err)
			}
			ev, err := rec.Event()
			if err != nil {
				t.Fatalf("Event() could not rebuild the record: %v\n\n"+
					"The relay calls this to publish. A record it cannot rebuild is a HEAD-OF-LINE "+
					"STOP for the order — every FACT behind it waits forever, because publishing past "+
					"it would hand consumers a history with a hole in it.", err)
			}
			if ev.Payload == nil {
				t.Fatal("rebuilt event carries no payload")
			}
			if got := string(ev.Payload.ProtoReflect().Descriptor().FullName()); !strings.HasPrefix(rec.PayloadSchemaRef, got+":") {
				t.Errorf("rebuilt payload is %s but the ref says %q — the ref resolves to the WRONG "+
					"message, so a consumer decoding by ref gets a different type than was sent",
					got, rec.PayloadSchemaRef)
			}
			if ev.TenantID == "" {
				t.Error("rebuilt event has no tenant — the relay has none of its own, so a record " +
					"that loses it publishes a FACT the broker refuses")
			}
		})
	}
}
