package syntheticvenue

import (
	"context"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/testidentity"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Each count is one invocation of the real mTLS client, including failures.
type countedRPC struct {
	venuepb.VenueAdapterServiceClient
	executes, cancels, queries atomic.Int32
}

func (c *countedRPC) Execute(ctx context.Context, req *venuepb.ExecuteRequest, opts ...grpc.CallOption) (*venuepb.ExecuteResponse, error) {
	c.executes.Add(1)
	return c.VenueAdapterServiceClient.Execute(ctx, req, opts...)
}
func (c *countedRPC) CancelOrder(ctx context.Context, req *venuepb.CancelOrderRequest, opts ...grpc.CallOption) (*venuepb.CancelOrderResponse, error) {
	c.cancels.Add(1)
	return c.VenueAdapterServiceClient.CancelOrder(ctx, req, opts...)
}
func (c *countedRPC) QueryOrder(ctx context.Context, req *venuepb.QueryOrderRequest, opts ...grpc.CallOption) (*venuepb.QueryOrderResponse, error) {
	c.queries.Add(1)
	return c.VenueAdapterServiceClient.QueryOrder(ctx, req, opts...)
}

func issue(t *testing.T, ca *testidentity.Authority, id string) *testidentity.Source {
	t.Helper()
	s, err := ca.Issue(spiffeid.RequireFromString(id), time.Now().Add(-time.Minute), time.Now().Add(time.Minute*10))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fixtureOrder() *orderpb.OrderState {
	return &orderpb.OrderState{OrderId: uuid.NewString(), PortfolioId: "synthetic-portfolio", Venue: MIC, VenueAccountId: Account, InstrumentId: Instrument,
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		OrderedQuantity: &commonpb.Decimal{Coefficient: 3, Exponent: -9}, LimitPrice: &commonpb.Decimal{Coefficient: 125, Exponent: -2}}
}

func TestAcceptedRejectsUntrustedScopeBeforeClaim(t *testing.T) {
	st := fixtureOrder()
	payload, _ := proto.Marshal(&orderpb.OrderAccepted{OrderId: st.OrderId, State: st})
	env := &envelopepb.Envelope{EventType: AcceptedSubject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "order", SchemaVersion: 1,
		Source: "oms", TenantId: "fixture", PartitionKey: st.OrderId, EventId: uuid.NewString(), EventTime: timestamppb.Now()}
	for _, mutate := range []func(*envelopepb.Envelope){func(e *envelopepb.Envelope) { e.TenantId = "other" }, func(e *envelopepb.Envelope) { e.Source = "gateway" },
		func(e *envelopepb.Envelope) { e.PartitionKey = "unrelated" }, func(e *envelopepb.Envelope) { e.SchemaVersion = 2 }} {
		e := proto.Clone(env).(*envelopepb.Envelope)
		mutate(e)
		i := &Ingress{tenant: "fixture", portfolio: st.PortfolioId, owner: func(context.Context, Command) (Receipt, error) {
			t.Fatal("untrusted accepted fact reserved a route")
			return Receipt{}, nil
		}}
		if err := i.HandleAccepted(t.Context(), e, payload); err == nil {
			t.Fatal("untrusted accepted event succeeded")
		}
	}
}

func TestExactPartialLotAdmittedBeforePlacement(t *testing.T) {
	for _, tc := range []struct{ q, half *commonpb.Decimal }{
		{&commonpb.Decimal{Coefficient: 3, Exponent: -9}, &commonpb.Decimal{Coefficient: 15, Exponent: -10}},
		{&commonpb.Decimal{Coefficient: 2, Exponent: -64}, &commonpb.Decimal{Coefficient: 1, Exponent: -64}},
		{&commonpb.Decimal{Coefficient: 1, Exponent: -64}, nil},
		{&commonpb.Decimal{Coefficient: math.MaxInt64}, nil},
	} {
		st := fixtureOrder()
		st.OrderedQuantity = tc.q
		got, ok := partialQuantity(tc.q)
		_, err := terms(st)
		if ok != (tc.half != nil) || (err == nil) != ok || (ok && !proto.Equal(got, tc.half)) {
			t.Fatalf("quantity %v: half=%v, ok=%t, terms error=%v", tc.q, got, ok, err)
		}
	}
}

func TestFundingProjectsOnlyAttestedPartialExecution(t *testing.T) {
	state := fixtureOrder()
	state.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
	state.OrderedQuantity = &commonpb.Decimal{Coefficient: 125, Exponent: -2}
	state.LeavesQuantity = state.OrderedQuantity
	state.FilledQuantity = &commonpb.Decimal{}
	state.LimitPrice = &commonpb.Decimal{Coefficient: 200}
	when := timestamppb.Now()
	base := &orderpb.Fill{FillId: "venue-half", VenueExecutionId: "venue-half", OrderId: state.OrderId, Venue: MIC, VenueAccountId: Account, InstrumentId: Instrument, Side: state.Side,
		Quantity: &commonpb.Decimal{Coefficient: 625, Exponent: -3}, Price: state.LimitPrice, ExecutedAt: when, Fee: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1}}}
	for _, tc := range []struct {
		name     string
		change   func(*orderpb.Fill)
		accepted bool
	}{
		{"reported", func(*orderpb.Fill) {}, true},
		{"missing_fee", func(f *orderpb.Fill) { f.Fee = nil }, false},
		{"fee_currency", func(f *orderpb.Fill) { f.Fee.CurrencyCode = "EUR" }, false},
		{"above_limit", func(f *orderpb.Fill) { f.Price = &commonpb.Decimal{Coefficient: 201} }, false},
		{"foreign_account", func(f *orderpb.Fill) { f.VenueAccountId = "other" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fill := proto.Clone(base).(*orderpb.Fill)
			tc.change(fill)
			wire, err := proto.Marshal(fill)
			if err != nil {
				t.Fatal(err)
			}
			c := Command{Op: "fill", Principal: OMSIdentity, Portfolio: state.PortfolioId, OrderID: state.OrderId, EventID: fill.FillId, EffectiveSeconds: when.Seconds, EffectiveNanos: when.Nanos, Fill: wire}
			view := &venuepb.QueryOrderResponse{State: venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED, ExecutedQuantity: fill.Quantity, Fills: []*orderpb.Fill{fill}}
			projection, err := ProjectSettlement(state, c, view, []string{"BNB", "USD"})
			if (err == nil) != tc.accepted {
				t.Fatalf("unexpected projection decision: %v", err)
			}
			if tc.accepted && (len(projection.ExecutedDebits) != 2 || projection.ExecutedDebits[0] != "0" || projection.ExecutedDebits[1] != "126") {
				t.Fatal("incorrect confirmed per-currency execution debit")
			}
		})
	}
}

func TestFundedWorkingAcknowledgementRequiresMeasuredZero(t *testing.T) {
	state := fixtureOrder()
	state.Status = orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW
	at := time.Now()
	c := Command{Op: "ack", Principal: OMSIdentity, Portfolio: state.PortfolioId, OrderID: state.OrderId,
		VenueOrderID: state.OrderId, EffectiveSeconds: at.Unix(), EffectiveNanos: int32(at.Nanosecond())}
	for _, tc := range []struct {
		name string
		view *venuepb.QueryOrderResponse
		ok   bool
	}{
		{"working_zero", &venuepb.QueryOrderResponse{State: venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING, ExecutedQuantity: &commonpb.Decimal{}}, true},
		{"unknown", &venuepb.QueryOrderResponse{ExecutedQuantity: &commonpb.Decimal{}}, false},
		{"missing", &venuepb.QueryOrderResponse{State: venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING}, false},
		{"unexplained_execution", &venuepb.QueryOrderResponse{State: venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING, ExecutedQuantity: &commonpb.Decimal{Coefficient: 1}}, false},
		{"unexplained_fill", &venuepb.QueryOrderResponse{State: venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING, ExecutedQuantity: &commonpb.Decimal{}, Fills: []*orderpb.Fill{{}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ProjectSettlement(state, c, tc.view, []string{"BNB", "USD"})
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected funded acknowledgement: %v", err)
			}
			if tc.ok && (p.Next.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_ROUTED || p.Next.VenueAckAt == nil || p.Fact == nil || len(p.ExecutedDebits) != 0) {
				t.Fatal("working acknowledgement changed settlement or lost the routed order")
			}
		})
	}
}

func TestReceiptReplayAcrossBrokerAndVenueRestart(t *testing.T) {
	dsn, url := os.Getenv("TEST_SYNTHETIC_VENUE_POSTGRES_URL"), os.Getenv("TEST_SYNTHETIC_VENUE_NATS_URL")
	if dsn == "" || url == "" {
		t.Skip("requires disposable synthetic role/receipt schema and isolated loopback JetStream")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("open synthetic receipt pool")
	}
	defer pool.Close()
	st := fixtureOrder()
	cfg := Config{Tenant: "fixture", Portfolio: st.PortfolioId, FillTime: time.Unix(1800000002, 123).UTC(), LoseExecuteAck: true}
	v, err := New(ctx, cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := testidentity.NewAuthority(spiffeid.RequireTrustDomainFromString("kanz.internal"))
	if err != nil {
		t.Fatal(err)
	}
	venueID, omsID := issue(t, ca, VenueIdentity), issue(t, ca, OMSIdentity)
	address, server := Start(t, v, venueID)
	rpc := Client(t, address, omsID)
	bad := Client(t, address, issue(t, ca, "spiffe://kanz.internal/ns/kanz-services/sa/gateway"))
	if _, err = bad.Describe(ctx, &venuepb.DescribeRequest{}); err == nil {
		t.Fatal("untrusted workload passed mutual TLS")
	}
	if _, err = rpc.Execute(ctx, &venuepb.ExecuteRequest{TenantId: "other", State: st}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-tenant execute: %v", err)
	}
	desc, err := rpc.Describe(ctx, &venuepb.DescribeRequest{})
	if err != nil || !desc.GetAccountVerified() || desc.GetExchangeAccountId() != Account {
		t.Fatalf("account discovery: %v", err)
	}

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal("connect disposable broker")
	}
	// EnsureSubjects registers stream removal in Cleanup. Keep its connection
	// open until removal completes, including for consecutive fixture runs.
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "XSIM_ACCEPTED", []string{AcceptedSubject})
	tuning := bus.RetainedWorkTuning()
	broker, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "xsim", ConsumerTuning: &tuning})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close() }()
	consumer, err := bus.NewConsumer(broker, bus.WithDLQ(broker), bus.WithRetainedRetries(AcceptedSubject))
	if err != nil {
		t.Fatal(err)
	}
	producer, err := bus.NewProducer(broker, bus.ProducerConfig{Source: "oms", ProducerVersion: "fixture", Tenant: cfg.Tenant})
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := proto.Marshal(st)
	// A narrow spy stands in for the Rust owner: the test observes venue
	// effects, broker redelivery and which commands reach the owner process.
	// The integrated slice independently exercises Rust admission and commits.
	var mu sync.Mutex
	seen := map[string]bool{}
	var commands []Command
	acked := make(chan struct{})
	owner := func(_ context.Context, c Command) (Receipt, error) {
		mu.Lock()
		defer mu.Unlock()
		commands = append(commands, c)
		if c.Principal != OMSIdentity || c.Tenant != cfg.Tenant || c.Portfolio != st.PortfolioId || c.OrderID != st.OrderId {
			t.Error("wrong owner coordinates")
		}
		r := Receipt{OK: true, State: wire, Code: "OK"}
		switch c.Op {
		case "dispatch", "cancel_pending":
			if seen[c.Op] {
				r.Code = "RECONCILE_REQUIRED"
			} else if c.Op == "dispatch" {
				r.Code = "DISPATCH_ONCE"
			} else {
				r.Code = "CANCEL_ONCE"
			}
		case "ack":
			if !seen["ack"] {
				close(acked)
			}
		}
		seen[c.Op] = true
		return r, nil
	}
	dispatchRPC := &countedRPC{VenueAdapterServiceClient: rpc}
	ingress, err := NewIngress(cfg.Tenant, cfg.Portfolio, dispatchRPC, owner)
	if err != nil {
		t.Fatal(err)
	}
	ready, done := make(chan struct{}), make(chan error, 1)
	subctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { done <- ingress.Subscribe(subctx, consumer, "xsim-"+st.OrderId, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("subscribe: %v", err)
	case <-ctx.Done():
		t.Fatal("no durable readiness")
	}
	if err := producer.Publish(ctx, bus.Event{Subject: AcceptedSubject, EventType: AcceptedSubject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		Domain: "order", SchemaVersion: 1, EventTime: time.Unix(1800000000, 0), PartitionKey: st.OrderId, Payload: &orderpb.OrderAccepted{OrderId: st.OrderId, State: st}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-acked:
	case <-ctx.Done():
		t.Fatal("lost ACK was not reconciled after durable redelivery")
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(commands) != 4 || commands[0].Op != "dispatch" || commands[1].Op != "timeout" || commands[2].Op != "dispatch" || commands[3].Op != "ack" ||
		commands[0].EventID != commands[2].EventID || commands[3].VenueOrderID != st.OrderId {
		t.Fatalf("unexpected loss/redelivery commands: %v", commands)
	}
	if dispatchRPC.executes.Load() != 1 || dispatchRPC.queries.Load() != 1 {
		t.Fatal("lost acknowledgement resent or skipped reconciliation")
	}

	// A fresh venue instance sees the previously committed remote order. A
	// successful reconciliation is only possible if the first send committed.
	server.Stop()
	v2, err := New(ctx, cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	address2, _ := Start(t, v2, venueID)
	rpc2 := Client(t, address2, omsID)
	conflicting := proto.Clone(st).(*orderpb.OrderState)
	conflicting.LimitPrice = &commonpb.Decimal{Coefficient: 99}
	if _, err := rpc2.Execute(ctx, &venuepb.ExecuteRequest{TenantId: cfg.Tenant, State: conflicting}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("conflicting retry: %v", err)
	}
	cancelRPC := &countedRPC{VenueAdapterServiceClient: rpc2}
	next, err := NewIngress(cfg.Tenant, cfg.Portfolio, cancelRPC, owner)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := next.Cancel(ctx, st.OrderId, "cancel-request", cfg.FillTime); err != nil {
			t.Fatal(err)
		}
	}
	if cancelRPC.cancels.Load() != 1 || cancelRPC.queries.Load() != 2 {
		t.Fatal("cancel replay resent or skipped reconciliation")
	}
	if len(commands) != 10 || commands[4].Op != "cancel_pending" || commands[5].Op != "fill" || commands[6].Op != "cancelled" ||
		commands[5].EventID != commands[8].EventID || commands[6].EventID != commands[9].EventID ||
		string(commands[5].Fill) != string(commands[8].Fill) {
		t.Fatal("cancel replay changed source evidence")
	}
	var fill orderpb.Fill
	var confirmed commonpb.Decimal
	if proto.Unmarshal(commands[5].Fill, &fill) != nil || proto.Unmarshal(commands[6].ExecutedQuantity, &confirmed) != nil ||
		confirmed.Coefficient != 15 || confirmed.Exponent != -10 || !proto.Equal(fill.Quantity, &confirmed) ||
		!proto.Equal(fill.Price, st.LimitPrice) || !fill.ExecutedAt.AsTime().Equal(cfg.FillTime) {
		t.Fatal("wrong partial execution/venue clock")
	}
	other := &venuepb.QueryOrderRequest{TenantId: cfg.Tenant, State: proto.Clone(st).(*orderpb.OrderState)}
	other.State.OrderId += "missing"
	view, err := rpc2.QueryOrder(ctx, other)
	if err != nil || view.GetState() != venuepb.OrderViewState_ORDER_VIEW_STATE_UNKNOWN || view.ExecutedQuantity != nil {
		t.Fatal("missing venue order was not unknown")
	}
	// The durable claim outlived its sender, but no receipt exists. UNKNOWN
	// must persist uncertainty and never create a second dispatch or cancel.
	unknownWire, _ := proto.Marshal(other.State)
	for _, op := range []string{"dispatch", "cancel_pending"} {
		var observed []string
		unknownRPC := &countedRPC{VenueAdapterServiceClient: rpc2}
		recovered, err := NewIngress(cfg.Tenant, cfg.Portfolio, unknownRPC, func(_ context.Context, c Command) (Receipt, error) {
			observed = append(observed, c.Op)
			return Receipt{OK: true, Code: "RECONCILE_REQUIRED", State: unknownWire}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := recovered.route(ctx, recovered.command(op, other.State.OrderId, "prior-claim", cfg.FillTime)); err == nil ||
			len(observed) != 2 || observed[0] != op || observed[1] != "timeout" ||
			unknownRPC.executes.Load() != 0 || unknownRPC.cancels.Load() != 0 || unknownRPC.queries.Load() != 1 {
			t.Fatal("unknown remote order escaped the unresolved claim")
		}
	}
}

func TestRemainderIsOneDurableExecution(t *testing.T) {
	dsn := os.Getenv("TEST_SYNTHETIC_VENUE_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires the disposable XSIM receipt database")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("open synthetic receipt pool")
	}
	defer pool.Close()
	st := fixtureOrder()
	cfg := Config{Tenant: "fixture", Portfolio: st.PortfolioId, FillTime: time.Unix(1800000002, 123).UTC(), FillOnExecute: true,
		ExecutionFee: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1}}}
	v, err := New(ctx, cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := testidentity.NewAuthority(spiffeid.RequireTrustDomainFromString("kanz.internal"))
	if err != nil {
		t.Fatal(err)
	}
	address, _ := Start(t, v, issue(t, ca, VenueIdentity))
	rpc := Client(t, address, issue(t, ca, OMSIdentity))
	execute := func(st *orderpb.OrderState) {
		t.Helper()
		if _, err := rpc.Execute(ctx, &venuepb.ExecuteRequest{TenantId: cfg.Tenant, State: st}); err != nil {
			t.Fatal(err)
		}
	}
	query := func(st *orderpb.OrderState) *venuepb.QueryOrderResponse {
		t.Helper()
		reply, err := rpc.QueryOrder(ctx, &venuepb.QueryOrderRequest{TenantId: cfg.Tenant, State: st})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	cancel := func(st *orderpb.OrderState) error {
		_, err := rpc.CancelOrder(ctx, &venuepb.CancelOrderRequest{TenantId: cfg.Tenant, State: st})
		return err
	}
	at := cfg.FillTime.Add(time.Nanosecond)
	if err := v.RecordRemainder(ctx, st.OrderId, at); status.Code(err) != codes.NotFound {
		t.Fatal("missing order", err)
	}
	execute(st)
	partial := query(st)
	if partial.State != venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED || len(partial.Fills) != 1 {
		t.Fatal("missing first execution", partial)
	}
	if err := v.RecordRemainder(ctx, st.OrderId, cfg.FillTime); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("out-of-order time", err)
	}
	// A restarted controller uses only persisted evidence for this execution.
	restartedCfg := cfg
	restartedCfg.ExecutionFee = nil
	restartedCfg.FillTime = at.Add(time.Hour)
	restarted, err := New(ctx, restartedCfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- v.RecordRemainder(ctx, st.OrderId, at) }()
	go func() { done <- restarted.RecordRemainder(ctx, st.OrderId, at) }()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal("concurrent retry", err)
		}
	}
	for _, err := range []error{v.RecordRemainder(ctx, st.OrderId, at.Add(time.Nanosecond)), cancel(st)} {
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatal("terminal contradiction", err)
		}
	}
	execute(st)
	filled := query(st)
	if filled.State != venuepb.OrderViewState_ORDER_VIEW_STATE_FILLED || len(filled.Fills) != 2 ||
		!proto.Equal(filled.ExecutedQuantity, st.OrderedQuantity) || !proto.Equal(filled.Fills[0], partial.Fills[0]) {
		t.Fatal("incomplete terminal evidence", filled)
	}
	expected := proto.Clone(partial.Fills[0]).(*orderpb.Fill)
	expected.FillId = sourceID("xsim-remainder", cfg.Tenant, cfg.Portfolio, st.OrderId)
	expected.VenueExecutionId, expected.ExecutedAt = expected.FillId, timestamppb.New(at)
	if !proto.Equal(filled.Fills[1], expected) || expected.FillId == filled.Fills[0].FillId {
		t.Fatal("second receipt lost its identity, time or economics", filled.Fills[1])
	}
	err = v.transaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE synthetic_venue.receipts SET execution=remainder_execution WHERE tenant_id=$1 AND portfolio_id=$2 AND order_id=$3", cfg.Tenant, cfg.Portfolio, st.OrderId)
		return err
	})
	if err == nil || !proto.Equal(query(st), filled) {
		t.Fatal("restricted role rewrote the first execution")
	}
	other := cfg
	other.Tenant += "-other"
	foreign, err := New(ctx, other, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.RecordRemainder(ctx, st.OrderId, at); status.Code(err) != codes.NotFound {
		t.Fatal("cross-tenant order visible", err)
	}
	withdrawn := fixtureOrder()
	execute(withdrawn)
	if err := cancel(withdrawn); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordRemainder(ctx, withdrawn.OrderId, at); status.Code(err) != codes.FailedPrecondition ||
		query(withdrawn).State != venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED {
		t.Fatal("cancelled order admitted a remainder", err)
	}
}
