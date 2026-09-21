package order

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/platform/halt"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A protocol simulation over a real TCP gRPC server, not a venue sandbox.
// The real PostgreSQL and JetStream boundaries below are not replaced.
type recoveryProtocolVenue struct {
	venuepb.UnimplementedVenueAdapterServiceServer
	queries, executions atomic.Int32
	fill                *orderpb.Fill
}

func (v *recoveryProtocolVenue) Describe(context.Context, *venuepb.DescribeRequest) (*venuepb.DescribeResponse, error) {
	return &venuepb.DescribeResponse{Mic: "BINANCE", Account: "account", AccountVerified: true, ExchangeAccountId: "exchange-account"}, nil
}
func (v *recoveryProtocolVenue) QueryOrder(_ context.Context, req *venuepb.QueryOrderRequest) (*venuepb.QueryOrderResponse, error) {
	v.queries.Add(1)
	if req.GetTenantId() != testTenant || req.GetState().GetOrderId() != v.fill.GetOrderId() {
		return nil, errors.New("wrong query attribution")
	}
	return &venuepb.QueryOrderResponse{State: venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED, ExecutedQuantity: v.fill.Quantity, Fills: []*orderpb.Fill{v.fill}}, nil
}
func (v *recoveryProtocolVenue) Execute(context.Context, *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	v.executions.Add(1)
	return nil, errors.New("recovery must never place an order")
}

func TestRecoveryHandlerRestartsAtFrozenEvidenceOverRealBroker(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires real JetStream and PostgreSQL")
	}
	pool := newPool(t)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(context.Background(), testTenant), 30*time.Second)
	defer cancel()
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	bustest.EnsureSubjects(t, ctx, js, "RECOVERY_HANDLER_"+suffix, []string{"order.>"})
	stream, err := bustest.StreamFor(ctx, js, fillfact.SubjectRecovered)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: "recovery-handler-" + suffix, FilterSubject: fillfact.SubjectRecovered, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteConsumer(context.Background(), stream, "recovery-handler-"+suffix) }()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "recovery-handler-proof"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "test", Tenant: testTenant})
	if err != nil {
		t.Fatal(err)
	}
	d := func(raw string) *commonpb.Decimal { result, _ := execution.ParseDec(raw); return result }
	st := &orderpb.OrderState{OrderId: "terminal-" + suffix, PortfolioId: "fund", Venue: "BINANCE", VenueAccountId: "account", InstrumentId: "BTC-USD", OrderedQuantity: d("2"), Side: orderpb.Side_SIDE_BUY, Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}
	store := NewPostgres(pool)
	if err := store.Create(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	f := &orderpb.Fill{OrderId: st.OrderId, FillId: "fill-" + suffix, VenueExecutionId: "trade-" + suffix, Venue: st.Venue, VenueAccountId: st.VenueAccountId, InstrumentId: st.InstrumentId, Side: st.Side, Quantity: d("1"), Price: d("100"), Fee: &commonpb.Money{Amount: d("0.1"), CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(t0)}
	adapter := &recoveryProtocolVenue{fill: f}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	venuepb.RegisterVenueAdapterServiceServer(server, adapter)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { server.Stop(); <-done }()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	venue := execution.NewGRPCVenue("BINANCE", "account", conn, testTenant)
	router := execution.NewRouter([]execution.Venue{venue})
	bindings := mustBind(t, testTenant+"/fund@BINANCE=account")
	newService := func() *Service {
		svc, err := NewService(testTenant, NewPostgres(pool), NewEmitter(producer), nil, router, nil, nil, WithAccountBindings(bindings, true, nil), WithHaltGate(halt.OpenGate(nil)))
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	observation := &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st, Reason: "missed terminal fill", DetectedAt: timestamppb.Now()}
	payload, err := proto.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	env := &envelopepb.Envelope{EventId: "observation-" + suffix, TenantId: testTenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
	// Crash after freezing the source, before claiming the financial execution.
	c, err := store.ObserveRecovery(ctx, env.EventId, st.OrderId, env.EventId, payload)
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := store.RecordRecoveryMapping(ctx, RecoveryMapping{testTenant, "fund", "BINANCE", "account", "exchange-account"}, execution.AccountProof{Verified: true, ExchangeAccountID: "exchange-account"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FreezeRecoveryHistory(ctx, c, mapping, st, execution.OrderView{State: execution.OrderViewCancelled, ExecutedQuantity: f.Quantity, Fills: []*orderpb.Fill{f}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := newService().HandleRecovery(ctx, env, payload); err != nil {
			t.Fatal(err)
		}
	}
	if adapter.queries.Load() != 0 || adapter.executions.Load() != 0 {
		t.Fatalf("restart re-queried or traded: queries=%d executions=%d", adapter.queries.Load(), adapter.executions.Load())
	}
	msg, err := consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	wireEnv, wirePayload, err := bus.Unframe(msg.Data())
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Validate(wireEnv); err != nil {
		t.Fatal(err)
	}
	recovered, portfolio, err := fillfact.DecodeRecovery(wirePayload)
	if err != nil || portfolio != "fund" || !fillfact.SameExecution(recovered, f) || recovered.GetRecovery().GetCaseId() != c.ID {
		t.Fatalf("execution=%v portfolio=%s err=%v", recovered, portfolio, err)
	}
	// A failed book delivery must become a durable blocked lifecycle, not remain
	// silently investigating forever. Repeated notifications are idempotent.
	for range 2 {
		if err := newService().HandleRecoveryParked(ctx, wireEnv, wirePayload); err != nil {
			t.Fatal(err)
		}
	}
	parked, err := store.RecoveryCase(ctx, c.ID)
	if err != nil || parked.Status != "blocked" {
		t.Fatalf("parked=%+v err=%v", parked, err)
	}
	// Redrive may later succeed. Only both bound acknowledgements resolve the
	// blocked case; a delayed failure notification cannot reopen it afterwards.
	for _, subject := range []string{fillfact.RecoveryPositionApplied, fillfact.RecoveryLedgerApplied} {
		record, err := fillfact.RecoveryAck(ctx, subject, recovered, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ackEnv := &envelopepb.Envelope{TenantId: testTenant, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
		if err := newService().HandleRecoveryAck(ctx, ackEnv, record.Payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := newService().HandleRecoveryParked(ctx, wireEnv, wirePayload); err != nil {
		t.Fatal(err)
	}
	parked, err = store.RecoveryCase(ctx, c.ID)
	if err != nil || parked.Status != "corrected" {
		t.Fatalf("late failure reopened case: %+v %v", parked, err)
	}
	var unbound orderpb.ExecutionRecovered
	if err := proto.Unmarshal(wirePayload, &unbound); err != nil {
		t.Fatal(err)
	}
	unbound.Fill.Recovery.PayloadDigest = strings.Repeat("f", 64)
	unboundBytes, err := proto.Marshal(&unbound)
	if err != nil {
		t.Fatal(err)
	}
	if err := newService().HandleRecoveryParked(ctx, wireEnv, unboundBytes); !errors.Is(err, ErrRecoveryEvidenceConflict) {
		t.Fatalf("completed case acknowledged unbound evidence: %v", err)
	}
	if err := msg.Nak(); err != nil {
		t.Fatal(err)
	}
	again, err := consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := again.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
	actual, _, err := store.Load(ctx, st.OrderId)
	if err != nil || actual.GetStatus() != st.Status || dec.Cmp(actual.GetFilledQuantity(), f.Quantity) != 0 {
		t.Fatalf("order=%v err=%v", actual, err)
	}
	// A fresh observation uses the real query boundary, but reposts no economics.
	env.EventId = "second-" + suffix
	if err := newService().HandleRecovery(ctx, env, payload); err != nil {
		t.Fatal(err)
	}
	if adapter.queries.Load() != 1 || adapter.executions.Load() != 0 {
		t.Fatalf("queries=%d executions=%d", adapter.queries.Load(), adapter.executions.Load())
	}
	var claims int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM order_fills`).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("claims=%d err=%v", claims, err)
	}
	// Tenant isolation is checked before the observation may create a case.
	env.TenantId = "other"
	if err := newService().HandleRecovery(ctx, env, payload); err == nil {
		t.Fatal("cross-tenant recovery accepted")
	}
}
