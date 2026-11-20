package syntheticvenue

import (
	"context"
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
	defer nc.Close()
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
	mu.Lock()
	if len(commands) != 4 || commands[0].Op != "dispatch" || commands[1].Op != "timeout" || commands[2].Op != "dispatch" || commands[3].Op != "ack" ||
		commands[0].EventID != commands[2].EventID || commands[3].VenueOrderID != st.OrderId {
		t.Fatalf("unexpected loss/redelivery commands: %v", commands)
	}
	mu.Unlock()
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
	mu.Lock()
	defer mu.Unlock()
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
