package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

type remainderProjector func(*orderpb.OrderState, *orderpb.Fill, *venuepb.QueryOrderResponse, []string) (syntheticvenue.SettlementProjection, error)

type remainderEvidence struct {
	first, second *orderpb.Fill
	projection    syntheticvenue.SettlementProjection
}

func prepareFundedRemainder(t *testing.T, ctx context.Context, control fundedVenueControl, cfg syntheticvenue.Config, partial syntheticvenue.Receipt, generation int, project remainderProjector) remainderEvidence {
	t.Helper()
	var st orderpb.OrderState
	if generation != 3 || proto.Unmarshal(partial.State, &st) != nil || st.Status != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Fatal("expected the retained G2 partial after actual handback to G3")
	}
	var raw []byte
	var currencies []string
	if err := control.proof.QueryRow(ctx, `SELECT fill, ARRAY(SELECT currency FROM capital_members WHERE order_id=$1 ORDER BY currency) FROM order_executions WHERE order_id=$1`, st.OrderId).Scan(&raw, &currencies); err != nil {
		t.Fatal(err)
	}
	var retained orderpb.Fill
	if proto.Unmarshal(raw, &retained) != nil || !retained.GetExecutedAt().AsTime().Equal(cfg.FillTime) {
		t.Fatal("XSIM clock differs from retained first execution")
	}
	// Advance the remote independently after ownership has changed. Only the
	// ordinary mTLS QueryOrder response can supply evidence for the new owner.
	venue, err := syntheticvenue.New(ctx, cfg, syntheticPool(t, ctx, "VENUE", cfg.Tenant))
	if err != nil {
		t.Fatal(err)
	}
	at := cfg.FillTime.Truncate(time.Microsecond).Add(789 * time.Nanosecond)
	if err := venue.RecordRemainder(ctx, st.OrderId, at); err != nil {
		t.Fatal(err)
	}
	view, err := control.venue.QueryOrder(ctx, &venuepb.QueryOrderRequest{TenantId: cfg.Tenant, State: &st})
	if err != nil || view.GetState() != venuepb.OrderViewState_ORDER_VIEW_STATE_FILLED || len(view.GetFills()) != 2 || !proto.Equal(&retained, view.Fills[0]) || dec.FromProto(view.ExecutedQuantity).RatString() != "5/4" {
		t.Fatalf("incomplete terminal XSIM history: %v", err)
	}
	second := view.Fills[1]
	if second.GetFillId() == retained.GetFillId() || second.GetVenueExecutionId() != second.GetFillId() || !second.GetExecutedAt().AsTime().Equal(at) ||
		second.GetOrderId() != st.OrderId || second.GetInstrumentId() != st.InstrumentId || second.GetVenueAccountId() != st.VenueAccountId ||
		dec.FromProto(second.GetQuantity()).RatString() != "5/8" || dec.FromProto(second.GetPrice()).RatString() != "200" ||
		second.GetFee().GetCurrencyCode() != "USD" || dec.FromProto(second.GetFee().GetAmount()).RatString() != "1" {
		t.Fatal("unexpected economics, identity or time of the second execution")
	}
	p, err := project(&st, &retained, view, currencies)
	if err != nil || !slices.Equal(p.Currencies, []string{"BNB", "USD"}) || !slices.Equal(p.ExecutedDebits, []string{"0", "126"}) {
		t.Fatalf("independent incremental settlement approval: %v", err)
	}
	claim := syntheticvenue.Command{Op: "fill", Tenant: cfg.Tenant, Portfolio: st.PortfolioId, OrderID: st.OrderId, Principal: syntheticvenue.OMSIdentity, EventID: second.FillId, EffectiveSeconds: at.Unix(), EffectiveNanos: int32(at.Nanosecond())}
	if err := control.recordProjection(ctx, claim, int64(generation), int64(partial.OrderVersion), partial.State, p, settlementProtoHash(t, second)); err != nil {
		t.Fatal(err)
	}
	return remainderEvidence{&retained, second, p}
}

func verifyFundedRemainderStored(t *testing.T, ctx context.Context, control fundedVenueControl, partial syntheticvenue.Receipt, want remainderEvidence) syntheticvenue.Receipt {
	t.Helper()
	id := control.order.command.OrderId
	var state []byte
	var version uint64
	if err := control.proof.QueryRow(ctx, "SELECT state,version FROM orders WHERE order_id=$1", id).Scan(&state, &version); err != nil {
		t.Fatal(err)
	}
	var st orderpb.OrderState
	if proto.Unmarshal(state, &st) != nil || version != partial.OrderVersion+1 || !proto.Equal(&st, want.projection.Next) || st.Status != orderpb.OrderStatus_ORDER_STATUS_FILLED ||
		dec.FromProto(st.FilledQuantity).RatString() != "5/4" || st.GetLeavesQuantity() == nil || dec.FromProto(st.LeavesQuantity).Sign() != 0 {
		t.Fatal("Go remainder or its replay changed the authorized terminal state")
	}
	rows, err := control.proof.Query(ctx, "SELECT fill FROM order_executions WHERE order_id=$1", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var wire []byte
		var f orderpb.Fill
		if err := rows.Scan(&wire); err != nil {
			t.Fatal(err)
		}
		if proto.Unmarshal(wire, &f) != nil || seen[f.FillId] || (!proto.Equal(&f, want.first) && !proto.Equal(&f, want.second)) {
			t.Fatal("unexpected retained execution")
		}
		seen[f.FillId] = true
	}
	if err := rows.Err(); err != nil || len(seen) != 2 {
		t.Fatalf("immutable execution count=%d: %v", len(seen), err)
	}
	return syntheticvenue.Receipt{OK: true, State: state, OrderVersion: version}
}

func verifyFundedRemainder(t *testing.T, ctx context.Context, s syntheticSession, control fundedVenueControl, partial syntheticvenue.Receipt, want remainderEvidence, firstBooked bool) syntheticvenue.Receipt {
	t.Helper()
	o := control.order
	id, tenant := o.command.OrderId, o.request.Tenant
	retained := verifyFundedRemainderStored(t, ctx, control, partial, want)
	requireFundedPendingFacts(t, ctx, control.proof, id, 4, nil)
	stream, err := s.js.Stream(ctx, "SYNTHETIC_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs != 4 {
		t.Fatalf("expected accepted, routed and two distinct execution FACTs: %v", err)
	}
	for _, subject := range []string{fillfact.SubjectPartiallyFilled, fillfact.SubjectFilled} {
		raw, err := stream.GetLastMsgForSubject(ctx, subject)
		if err != nil {
			t.Fatal(err)
		}
		var frame envelopepb.EventFrame
		if proto.Unmarshal(raw.Data, &frame) != nil || bus.Validate(frame.GetEnvelope()) != nil || frame.GetEnvelope().GetTenantId() != tenant || frame.GetEnvelope().GetPartitionKey() != id || frame.GetEnvelope().GetSource() != "oms" || frame.GetEnvelope().GetEventType() != subject {
			t.Fatal("broker execution FACT lost order attribution")
		}
		if subject == fillfact.SubjectFilled {
			var fact orderpb.OrderFilled
			if proto.Unmarshal(frame.Payload, &fact) != nil || !proto.Equal(&fact, want.projection.Fact) {
				t.Fatal("G3 broker FACT differs from independently authorized terminal execution")
			}
		} else {
			var fact orderpb.OrderPartiallyFilled
			if proto.Unmarshal(frame.Payload, &fact) != nil || !proto.Equal(fact.Fill, want.first) || !bytes.Equal(partial.State, marshalRemainderState(t, fact.State)) {
				t.Fatal("G2 partial FACT changed")
			}
		}
	}
	firstSequence := 1
	previouslyBooked := int64(0)
	if firstBooked {
		firstSequence = 2
		previouslyBooked = 126
	}
	checkVenueCash(t, ctx, control.proof, o, 1000-previouslyBooked, 252-previouslyBooked, 252, 0, 252, previouslyBooked)
	apply := syntheticvenue.CapitalCashHandler(syntheticPool(t, ctx, "CASH", tenant), tenant)
	for sequence := firstSequence; sequence <= 2; sequence++ {
		frames := bookSyntheticTrade(t, ctx, s, tenant, id, sequence)
		for _, frame := range frames {
			for range 2 {
				if err := apply(bus.WithTenantID(ctx, tenant), frame.Envelope, frame.Payload); err != nil {
					t.Fatal(err)
				}
				booked := int64(126 * sequence)
				checkVenueCash(t, ctx, control.proof, o, 1000-booked, 252-booked, 252, 0, 252, booked)
			}
		}
	}
	t.Log("XSIM placed one order, executed two distinct 0.625 fills at 200 USD plus 1 USD each, and received no cancellation. Go G3 replay preserved one filled FACT. Accounting booked 126 USD twice for two executions, leaving 748 USD and 8 BNB, both with zero reserves.")
	return retained
}

func marshalRemainderState(t *testing.T, state *orderpb.OrderState) []byte {
	t.Helper()
	wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

type fundedRemainderRecovery struct {
	control  fundedVenueControl
	partial  syntheticvenue.Receipt
	evidence remainderEvidence
}

type remainderPublication struct {
	rowID    int64
	eventID  string
	sequence uint64
	wire     []byte
}

// observe checks the same committed fill and cash immediately after each
// physical process exit. The broker frame is independent of the SQL mark.
func (r fundedRemainderRecovery) observe(t *testing.T, ctx context.Context, s syntheticSession, published, marked bool) remainderPublication {
	t.Helper()
	o := r.control.order
	verifyFundedRemainderStored(t, ctx, r.control, r.partial, r.evidence)
	checkVenueCash(t, ctx, r.control.proof, o, 1000, 252, 252, 0, 252, 0)
	var pending []string
	if !marked {
		pending = []string{fillfact.SubjectFilled}
	}
	requireFundedPendingFacts(t, ctx, r.control.proof, o.command.OrderId, 4, pending)

	var result remainderPublication
	var body []byte
	var cause, correlation, trace string
	var at time.Time
	var savedMark bool
	err := r.control.proof.QueryRow(ctx, `SELECT id,event_id::text,payload,event_time,causation_id,correlation_id,trace_context,published_at IS NOT NULL
        FROM outbox WHERE partition_key=$1 AND subject=$2`, o.command.OrderId, fillfact.SubjectFilled).
		Scan(&result.rowID, &result.eventID, &body, &at, &cause, &correlation, &trace, &savedMark)
	var fact orderpb.OrderFilled
	id, idErr := uuid.Parse(result.eventID)
	if err != nil || result.rowID <= 0 || idErr != nil || id.Version() != 7 || id.Variant() != uuid.RFC4122 || id.String() != result.eventID ||
		savedMark != marked || proto.Unmarshal(body, &fact) != nil || !proto.Equal(&fact, r.evidence.projection.Fact) ||
		cause != r.evidence.second.GetFillId() || correlation == "" || at.IsZero() {
		t.Fatalf("committed remainder publication differs from the approved FACT: %v", err)
	}

	stream, err := s.js.Stream(ctx, "SYNTHETIC_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	expected := uint64(3)
	if published {
		expected++
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs != expected || info.State.LastSeq != expected {
		t.Fatalf("unexpected broker count at remainder publication cut, expected %d: %v", expected, err)
	}
	raw, err := stream.GetLastMsgForSubject(ctx, fillfact.SubjectFilled)
	if !published {
		if !errors.Is(err, jetstream.ErrMsgNotFound) {
			t.Fatalf("the committed remainder reached JetStream before the publication cut: %v", err)
		}
		return result
	}
	if err != nil || raw.Sequence != expected {
		t.Fatalf("the retained remainder is not the fourth broker record: %v", err)
	}
	env, payload, err := bus.Unframe(raw.Data)
	if err != nil || bus.Validate(env) != nil || !bytes.Equal(payload, body) || env.GetSource() != "oms" ||
		env.GetTenantId() != o.request.Tenant || env.GetPartitionKey() != o.command.OrderId ||
		env.GetCorrelationId() != correlation || env.GetCausationId() != cause || env.GetTraceContext() != trace ||
		env.GetEventType() != fillfact.SubjectFilled || !env.GetEventTime().AsTime().Equal(at) ||
		env.GetEventId() != result.eventID || env.GetIdempotencyKey() != result.eventID || raw.Header.Get("Nats-Msg-Id") != result.eventID {
		t.Fatalf("broker frame differs from the committed remainder publication: %v", err)
	}
	result.sequence, result.wire = raw.Sequence, raw.Data
	return result
}
