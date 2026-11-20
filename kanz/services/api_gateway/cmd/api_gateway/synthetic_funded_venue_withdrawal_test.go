package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go/jetstream"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"google.golang.org/protobuf/proto"
)

// The independent control issuer binds the gateway cancellation to the partial
// state seen at handoff. Its terminal approval additionally requires the actual
// XSIM query and the original execution journal bytes.
type fundedWithdrawalApproval struct {
	restart    bool
	control    *fundedVenueControl
	event      *envelopepb.Envelope
	generation int64
	partial    syntheticvenue.Receipt
	currencies []string
	mu         sync.Mutex
	terminal   *syntheticvenue.SettlementProjection
}

func (control *fundedVenueControl) prepareGoWithdrawal(t *testing.T, ctx context.Context, session syntheticSession, partial syntheticvenue.Receipt, generation int) *fundedWithdrawalApproval {
	t.Helper()
	o := control.order
	if generation != 3 {
		t.Fatal("fresh withdrawal requires the third writer generation")
	}
	a := &fundedWithdrawalApproval{control: control, generation: int64(generation), partial: partial}
	err := control.proof.QueryRow(ctx, "SELECT array_agg(currency ORDER BY currency) FROM capital_members WHERE order_id=$1 AND owner_order_id=$1", o.command.OrderId).Scan(&a.currencies)
	if err != nil || !slices.Equal(a.currencies, []string{"BNB", "USD"}) {
		t.Fatalf("funded membership at transfer: %v %v", a.currencies, err)
	}
	state, err := a.readPartial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view, err := control.venue.QueryOrder(ctx, &venuepb.QueryOrderRequest{TenantId: o.request.Tenant, State: state})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.confirmExecution(ctx, state, view, venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED); err != nil {
		t.Fatal(err)
	}
	frame := session.cancelOrder(o.command.OrderId, "after-funded-handback", "synthetic-trader", o.request.Portfolio)
	req := cancellationRequest(t, frame, o.request.Portfolio)
	if req.Principal != o.request.Principal || req.Tenant != o.request.Tenant || frame.Envelope.PartitionKey != o.command.OrderId {
		t.Fatal("fresh gateway withdrawal is outside the retained mandate")
	}
	recordCancellationAuthority(t, ctx, req, frame.Envelope, time.Now().Add(30*time.Second), nil, nil)
	a.event = frame.Envelope
	cmd := a.command("cancel_pending", a.event.EventId, a.event.EventTime.AsTime())
	projected, err := syntheticvenue.ProjectSettlement(state, cmd, nil, a.currencies)
	if err == nil {
		err = control.recordProjection(ctx, cmd, a.generation, int64(partial.OrderVersion), partial.State, projected, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (a *fundedWithdrawalApproval) command(op, eventID string, at time.Time) syntheticvenue.Command {
	o := a.control.order
	return syntheticvenue.Command{Op: op, Principal: syntheticvenue.OMSIdentity, Tenant: o.request.Tenant, Portfolio: o.request.Portfolio,
		OrderID: o.command.OrderId, EventID: eventID, EffectiveSeconds: at.Unix(), EffectiveNanos: int32(at.Nanosecond())}
}

func (a *fundedWithdrawalApproval) readPartial(ctx context.Context) (*orderpb.OrderState, error) {
	o := a.control.order
	var wire []byte
	var version uint64
	err := a.control.proof.QueryRow(ctx, "SELECT state,version FROM orders WHERE order_id=$1 AND portfolio_id=$2", o.command.OrderId, o.request.Portfolio).Scan(&wire, &version)
	if err != nil {
		return nil, err
	}
	var st orderpb.OrderState
	if version != a.partial.OrderVersion || !bytes.Equal(wire, a.partial.State) || proto.Unmarshal(wire, &st) != nil || st.Status != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED || st.Quarantine != nil {
		return nil, errors.New("handoff partial order differs from the durable state")
	}
	return &st, nil
}

// Once XSIM has withdrawn the order, the lost response still leaves the OMS
// with the original open state and holds. A redelivery can only resolve that
// gap using the persisted claim and a new venue query.
func (a *fundedWithdrawalApproval) checkClaim(ctx context.Context) error {
	if _, err := a.readPartial(ctx); err != nil {
		return err
	}
	var claimed bool
	err := a.control.proof.QueryRow(ctx, `SELECT EXISTS(SELECT FROM synthetic_oms_observations
		WHERE order_id=$1 AND writer_epoch=$2 AND operation='cancel_pending' AND event_id=$3)
		AND (SELECT count(*) FROM outbox WHERE partition_key=$1)=3`,
		a.control.order.command.OrderId, a.generation, a.event.EventId).Scan(&claimed)
	if err != nil {
		return err
	}
	if !claimed {
		return errors.New("the durable claim or original outbox does not match the handed-off order")
	}
	return compareVenueCash(ctx, a.control.proof, a.control.order, 1000, 252, 252, 1, 126, 0)
}

func (a *fundedWithdrawalApproval) confirmExecution(ctx context.Context, prior *orderpb.OrderState, view *venuepb.QueryOrderResponse, want venuepb.OrderViewState) ([]byte, error) {
	if view.GetState() != want || len(view.GetFills()) != 1 || view.GetExecutedQuantity() == nil || !proto.Equal(prior.GetFilledQuantity(), view.ExecutedQuantity) {
		return nil, errors.New("XSIM state or executed total differs from the retained partial")
	}
	fill := view.Fills[0]
	if fill.GetOrderId() != prior.OrderId || fill.GetVenue() != prior.Venue || fill.GetVenueAccountId() != prior.VenueAccountId ||
		fill.GetInstrumentId() != prior.InstrumentId || fill.GetSide() != prior.Side || !proto.Equal(fill.GetQuantity(), view.ExecutedQuantity) ||
		fill.GetFillId() == "" || fill.GetVenueExecutionId() != fill.FillId {
		return nil, errors.New("XSIM execution does not match the funded order")
	}
	var recorded [][]byte
	err := a.control.proof.QueryRow(ctx, "SELECT array_agg(fill) FROM order_executions WHERE order_id=$1", prior.OrderId).Scan(&recorded)
	if err != nil {
		return nil, err
	}
	wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(fill)
	if err != nil || len(recorded) != 1 || !bytes.Equal(recorded[0], wire) {
		return nil, errors.New("XSIM execution differs from the immutable OMS journal")
	}
	return wire, nil
}

// This is called in the XSIM request goroutine after its QueryOrder RPC has
// observed a terminal receipt. An unavailable or contradictory input withholds
// approval and the Go consumer receives an error.
func (a *fundedWithdrawalApproval) recordTerminal(ctx context.Context, view *venuepb.QueryOrderResponse) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminal != nil {
		return nil
	}
	prior, err := a.readPartial(ctx)
	if err != nil {
		return err
	}
	wire, err := a.confirmExecution(ctx, prior, view, venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED)
	if err != nil {
		return err
	}
	fill := view.Fills[0]
	at := a.event.EventTime.AsTime()
	if fill.GetExecutedAt().AsTime().After(at) {
		at = fill.ExecutedAt.AsTime()
	}
	cmd := a.command("cancelled", fill.FillId+"/cancelled", at)
	cmd.ExecutedQuantity, err = (proto.MarshalOptions{Deterministic: true}).Marshal(view.ExecutedQuantity)
	if err != nil {
		return err
	}
	p, err := syntheticvenue.ProjectSettlement(prior, cmd, view, a.currencies)
	if err != nil {
		return err
	}
	if err = a.control.recordProjection(ctx, cmd, a.generation, int64(a.partial.OrderVersion), a.partial.State, p, settlementHash(wire)); err != nil {
		return err
	}
	a.terminal = &p
	return nil
}

func (a *fundedWithdrawalApproval) verifyTerminal(t *testing.T, ctx context.Context, pending []string) syntheticvenue.Receipt {
	t.Helper()
	control := a.control
	o := control.order
	a.mu.Lock()
	expected := a.terminal
	a.mu.Unlock()
	if expected == nil {
		t.Fatal("missing independent terminal approval")
	}
	var state, payload []byte
	var version uint64
	var cause, trace string
	err := control.proof.QueryRow(ctx, `SELECT o.state,o.version,f.payload,f.causation_id,f.trace_context
		FROM orders o JOIN outbox f ON (f.tenant_id,f.partition_key)=(o.tenant_id,o.order_id)
		WHERE o.order_id=$1 AND o.portfolio_id=$2 AND f.subject='order.order.cancelled'`, o.command.OrderId, o.request.Portfolio).Scan(&state, &version, &payload, &cause, &trace)
	var st orderpb.OrderState
	var cancelled orderpb.OrderCancelled
	if err != nil || version != a.partial.OrderVersion+1 || proto.Unmarshal(state, &st) != nil || !proto.Equal(&st, expected.Next) ||
		proto.Unmarshal(payload, &cancelled) != nil || !proto.Equal(&cancelled, expected.Fact) || cause != a.event.EventId || trace != a.event.TraceContext {
		t.Fatalf("Go terminal state or FACT differs from the independently observed withdrawal: %v", err)
	}
	requireFundedPendingFacts(t, ctx, control.proof, o.command.OrderId, 4, pending)
	checkVenueCash(t, ctx, control.proof, o, 1000, 126, 126, 0, 126, 0)
	return syntheticvenue.Receipt{OK: true, Code: "FOUND", State: state, OrderVersion: version}
}

func verifyGoWithdrawalBooking(t *testing.T, ctx context.Context, s syntheticSession, control fundedVenueControl, a *fundedWithdrawalApproval) syntheticvenue.Receipt {
	t.Helper()
	terminal := a.verifyTerminal(t, ctx, nil)
	o := control.order
	facts, err := s.js.Stream(ctx, "SYNTHETIC_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	info, err := facts.Info(ctx)
	if err != nil || info.State.Msgs != 4 || info.State.LastSeq != 4 {
		t.Fatalf("expected precisely four published lifecycle records: %v", err)
	}
	raw, err := facts.GetLastMsgForSubject(ctx, "order.order.cancelled")
	if err != nil || raw.Sequence != 4 {
		t.Fatalf("terminal cancellation was not the fourth broker record: %v", err)
	}
	env, payload, err := bus.Unframe(raw.Data)
	var retained []byte
	var recordedAt time.Time
	if err != nil || env.GetTenantId() != o.request.Tenant || env.GetPartitionKey() != o.command.OrderId ||
		env.GetCorrelationId() != control.accepted.CorrelationId || env.GetCausationId() != a.event.EventId ||
		env.GetTraceContext() != a.event.TraceContext || env.GetEventTime().CheckValid() != nil {
		t.Fatalf("published withdrawal lineage differs from retained authority: %v", err)
	}
	if err = control.proof.QueryRow(ctx, "SELECT payload,event_time FROM outbox WHERE partition_key=$1 AND subject='order.order.cancelled'", o.command.OrderId).Scan(&retained, &recordedAt); err != nil ||
		!bytes.Equal(payload, retained) || !env.GetEventTime().AsTime().Equal(recordedAt) {
		t.Fatalf("published withdrawal differs from the terminal outbox record: %v", err)
	}
	applyVenueAccounting(t, ctx, s, control.proof, o, 126, 0)
	t.Log("XSIM funded withdrawal: gateway-authorized G3 cancel, one placement, one committed withdrawal, lost ACK, terminal query, one execution, one 126 USD debit; remaining cash 874 USD, zero reserves")
	return terminal
}

// Observe the remote store without adding a venue query on behalf of the first
// writer. Recovery must still ask XSIM through its own authenticated RPC.
func (a *fundedWithdrawalApproval) verifyRemoteAfterCrash(t *testing.T, ctx context.Context) {
	t.Helper()
	o := a.control.order
	remote := syntheticPool(t, ctx, "VENUE", o.request.Tenant)
	var working bool
	var wire, recorded []byte
	if err := remote.QueryRow(ctx, "SELECT working_partial,execution FROM synthetic_venue.receipts WHERE tenant_id=$1 AND portfolio_id=$2 AND order_id=$3",
		o.request.Tenant, o.request.Portfolio, o.command.OrderId).Scan(&working, &wire); err != nil || working || len(wire) == 0 {
		t.Fatalf("XSIM did not retain the first process's withdrawal and fill: %v", err)
	}
	if err := a.control.proof.QueryRow(ctx, "SELECT fill FROM order_executions WHERE order_id=$1", o.command.OrderId).Scan(&recorded); err != nil || !bytes.Equal(wire, recorded) {
		t.Fatalf("XSIM execution differs from the retained fill after process exit: %v", err)
	}
}

func (a *fundedWithdrawalApproval) verifyBrokerBeforePublish(t *testing.T, ctx context.Context, s syntheticSession, delivery uint64) {
	t.Helper()
	facts, err := s.js.Stream(ctx, "SYNTHETIC_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	info, err := facts.Info(ctx)
	if err != nil || info.State.Msgs != 3 {
		t.Fatalf("expected the three preceding OMS FACTs after delivery %d: %v", delivery, err)
	}
	if _, err = facts.GetLastMsgForSubject(ctx, "order.order.cancelled"); !errors.Is(err, jetstream.ErrMsgNotFound) {
		t.Fatalf("unexpected terminal FACT before restart at delivery %d: %v", delivery, err)
	}
	subject := bus.TenantRoutedSubject(a.event.TenantId, "order.order.cancel")
	durable := "synthetic-go-funded-withdrawal-" + strings.ReplaceAll(subject, ".", "_")
	consumer, err := s.js.Consumer(ctx, "SYNTHETIC_ORDERS", durable)
	if err != nil {
		t.Fatal(err)
	}
	state, err := consumer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Delivered.Consumer != delivery || state.AckFloor.Consumer != 0 || state.NumAckPending != 1 {
		t.Fatalf("withdrawal durable after process exit: deliveries=%d (want %d), ACK floor=%d, pending=%d", state.Delivered.Consumer, delivery, state.AckFloor.Consumer, state.NumAckPending)
	}
}
