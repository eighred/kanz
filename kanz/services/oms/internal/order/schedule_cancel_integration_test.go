package order

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Protocol simulation only: actual TCP gRPC, PostgreSQL and JetStream provide
// the network/persistence boundaries. No external broker or live venue is used.
type restingScheduleVenue struct {
	venuepb.UnimplementedVenueAdapterServiceServer
	mu      sync.Mutex
	orders  map[string]bool // false=working; true=withdrawn
	cancels map[string]int
	lostAck string
}

func (v *restingScheduleVenue) Execute(_ context.Context, req *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	if req.GetTenantId() != testTenant || req.GetState().GetParentOrderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "expected attributed scheduled child")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	id := req.GetState().GetOrderId()
	if _, exists := v.orders[id]; !exists {
		v.orders[id] = false
	}
	return &venuepb.ExecuteResponse{}, nil // accepted and genuinely resting
}

func (v *restingScheduleVenue) CancelOrder(_ context.Context, req *venuepb.CancelOrderRequest) (*venuepb.CancelOrderResponse, error) {
	if req.GetTenantId() != testTenant {
		return nil, status.Error(codes.PermissionDenied, "wrong tenant")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	id := req.GetState().GetOrderId()
	if _, exists := v.orders[id]; !exists {
		return nil, status.Error(codes.NotFound, "never placed")
	}
	v.cancels[id]++
	v.orders[id] = true
	if id == v.lostAck {
		v.lostAck = ""
		return nil, status.Error(codes.Unavailable, "withdrawal applied but acknowledgement lost")
	}
	return &venuepb.CancelOrderResponse{}, nil
}

func TestScheduleE2E_CancellingAParentWithdrawsItsLiveChildren(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires real PostgreSQL and JetStream")
	}
	pool := newPool(t)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(context.Background(), testTenant), 30*time.Second)
	defer cancel()
	var bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil || bypass {
		t.Fatalf("requires NOSUPERUSER NOBYPASSRLS: bypass=%v err=%v", bypass, err)
	}
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
	bustest.EnsureSubjects(t, ctx, js, "SCHEDULE_CANCEL_"+suffix, []string{"order.>"})
	stream, err := bustest.StreamFor(ctx, js, EventTypeCancelled)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: "schedule-cancel-" + suffix,
		FilterSubject: EventTypeCancelled, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteConsumer(context.Background(), stream, "schedule-cancel-"+suffix) }()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "schedule-cancel-proof"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "test", Tenant: testTenant})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &restingScheduleVenue{orders: map[string]bool{}, cancels: map[string]int{}}
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
	router := execution.NewRouter([]execution.Venue{execution.NewGRPCVenue("XSIM", "sim-account", conn, testTenant)})
	at := schedStart.Add(25 * time.Minute)
	newService := func() *Service {
		// No OMS close tracker: the remote adapter owns its close tracking.
		svc, err := NewService(testTenant, NewPostgres(pool), NewEmitter(producer), nil, router, nil, nil,
			WithAccountBindings(mustBind(t, testTenant+"/fund-alpha@XSIM=sim-account"), true, nil), WithHaltGate(halt.OpenGate(nil)))
		if err != nil {
			t.Fatal(err)
		}
		svc.now = func() time.Time { return at }
		return svc
	}
	svc := newService()
	id := "p" + suffix
	cmd := scheduledOrder(id, 6, nil)
	if err := svc.Handle(ctx, &envelopepb.Envelope{EventType: SubjectSubmit, TenantId: testTenant}, mustMarshal(t, cmd)); err != nil {
		t.Fatal(err)
	}
	if parent, _, err := svc.store.Load(ctx, id); err != nil || parent.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED {
		st, readErr := js.Stream(ctx, stream)
		if readErr != nil {
			t.Fatal(readErr)
		}
		last, readErr := st.GetLastMsgForSubject(ctx, EventTypeOutcome)
		if readErr != nil {
			t.Fatalf("parent=%v err=%v outcome lookup=%v", parent, err, readErr)
		}
		_, body, _ := bus.Unframe(last.Data)
		var outcome commandpb.CommandOutcome
		_ = proto.Unmarshal(body, &outcome)
		t.Fatalf("parent not admitted: %v %v; outcome=%v", parent, err, &outcome)
	}
	if n, err := svc.DriveSchedules(ctx); err != nil || n != 3 {
		t.Fatalf("created %d children: %v", n, err)
	}
	children, err := svc.store.ListByParent(ctx, id)
	if err != nil || len(children) != 3 {
		t.Fatalf("children=%d err=%v", len(children), err)
	}
	for _, child := range children {
		if IsTerminal(child) || child.GetVenueAckAt() == nil || dec.FromProto(child.GetFilledQuantity()).Sign() != 0 {
			t.Fatalf("child is not acknowledged and resting: %v", child)
		}
	}
	adapter.mu.Lock()
	accepted := len(adapter.orders)
	adapter.lostAck = children[1].OrderId
	adapter.mu.Unlock()
	if accepted != 3 {
		t.Fatalf("venue accepted %d children, want 3", accepted)
	}
	withdraw := mustMarshal(t, &orderpb.CancelOrder{OrderId: id, Metadata: &commandpb.CommandMetadata{
		TargetId: id, PrincipalPortfolios: []string{"fund-alpha"}}})
	cancelEnv := &envelopepb.Envelope{EventType: SubjectCancel, TenantId: testTenant}
	if err := newService().Handle(ctx, cancelEnv, withdraw); err == nil {
		t.Fatal("lost venue acknowledgement was reported as confirmed cancellation")
	}
	parent, _, err := svc.store.Load(ctx, id)
	if err != nil || IsTerminal(parent) || parent.CancelAnnouncedAt != nil {
		t.Fatalf("unconfirmed parent became terminal: %v %v", parent, err)
	}
	ambiguous, _, err := svc.store.Load(ctx, children[1].OrderId)
	if err != nil || IsTerminal(ambiguous) || ambiguous.CancelAnnouncedAt != nil {
		t.Fatalf("unconfirmed child became terminal: %v %v", ambiguous, err)
	}
	// The first child's confirmed cancellation did publish; neither the
	// ambiguous child nor its parent may have a success fact in the stream.
	first, err := watch.Next(jetstream.FetchMaxWait(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := bus.Unframe(first.Data())
	if err != nil {
		t.Fatal(err)
	}
	var fact orderpb.OrderCancelled
	if err := proto.Unmarshal(raw, &fact); err != nil || fact.OrderId != children[0].OrderId {
		t.Fatalf("unexpected first cancellation: %v %v", &fact, err)
	}
	if err := first.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := watch.Info(ctx)
	if err != nil || info.NumPending != 0 {
		t.Fatalf("unconfirmed cancellation was published: %+v %v", info, err)
	}
	// A new OMS instance resumes from durable state after the lost reply. The
	// adapter answers the duplicate identity without creating another order.
	restarted := newService()
	if err := restarted.Handle(ctx, cancelEnv, withdraw); err != nil {
		t.Fatal(err)
	}
	for _, oid := range []string{id, children[0].OrderId, children[1].OrderId, children[2].OrderId} {
		st, _, err := restarted.store.Load(ctx, oid)
		if err != nil || st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED || st.CancelAnnouncedAt == nil {
			t.Fatalf("not durably cancelled: %v %v", st, err)
		}
	}
	seen := map[string]bool{children[0].OrderId: true}
	for range 3 {
		msg, err := watch.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(msg.Data())
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		var cancelled orderpb.OrderCancelled
		if err := proto.Unmarshal(payload, &cancelled); err != nil {
			t.Fatal(err)
		}
		if env.TenantId != testTenant || seen[cancelled.OrderId] {
			t.Fatalf("wrong tenant or duplicate cancellation: %v %v", env, &cancelled)
		}
		want := d(10, 0)
		if cancelled.OrderId == id {
			want = d(30, 0)
		} else if cancelled.OrderId != children[1].OrderId && cancelled.OrderId != children[2].OrderId {
			t.Fatalf("foreign cancellation: %v", &cancelled)
		}
		if dec.FromProto(cancelled.CancelledQuantity).Cmp(dec.FromProto(want)) != 0 {
			t.Fatalf("wrong withdrawn quantity: %v", &cancelled)
		}
		seen[cancelled.OrderId] = true
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	at = schedEnd.Add(time.Hour)
	if n, err := restarted.DriveSchedules(ctx); err != nil || n != 0 {
		t.Fatalf("cancelled parent created %d new slices: %v", n, err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.orders) != 3 {
		t.Fatalf("unexpected venue order: %v", adapter.orders)
	}
	for i, child := range children {
		want := 1
		if i == 1 {
			want = 2
		}
		if !adapter.orders[child.OrderId] || adapter.cancels[child.OrderId] != want {
			t.Fatalf("venue cancellation lost or duplicated: %s, calls=%d", child.OrderId, adapter.cancels[child.OrderId])
		}
	}
}
