package venueconformance

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type observed struct {
	method, tenant string
	state          *orderpb.OrderState
}
type adapter struct {
	venuepb.UnimplementedVenueAdapterServiceServer
	mu          sync.Mutex
	seen        []observed
	active      atomic.Int32
	serials     []string
	description *venuepb.DescribeResponse
	catalog     *venuepb.ListInstrumentsResponse
	fills       []*orderpb.Fill
	view        venuepb.OrderViewState
	fail        map[string]codes.Code
	wait        map[string]bool
	raw         map[string][]byte
}

func newAdapter() *adapter {
	return &adapter{description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, catalog: &venuepb.ListInstrumentsResponse{Mic: "MOCK", Instruments: []*venuepb.VenueInstrument{{InstrumentId: "fixture-instrument", VenueSymbol: "FIXTURE", BaseAsset: "", QuoteAsset: ""}}}, view: venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING, fail: map[string]codes.Code{}, wait: map[string]bool{}, raw: map[string][]byte{}}
}
func (a *adapter) configure(f func()) { a.mu.Lock(); defer a.mu.Unlock(); f() }
func (a *adapter) call(ctx context.Context, method, tenant string, state *orderpb.OrderState) error {
	a.mu.Lock()
	var cloned *orderpb.OrderState
	if state != nil {
		cloned = proto.Clone(state).(*orderpb.OrderState)
	}
	a.seen = append(a.seen, observed{method, tenant, cloned})
	if p, ok := peer.FromContext(ctx); ok {
		if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
			a.serials = append(a.serials, ti.State.PeerCertificates[0].SerialNumber.String())
		}
	}
	code, wait := a.fail[method], a.wait[method]
	a.mu.Unlock()
	if wait {
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err()
	}
	if code != codes.OK {
		return status.Error(code, "synthetic fixture refusal")
	}
	return nil
}
func (a *adapter) Execute(ctx context.Context, r *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	if err := a.call(ctx, "Execute", r.TenantId, r.State); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return proto.Clone(&venuepb.ExecuteResponse{Fills: a.fills}).(*venuepb.ExecuteResponse), nil
}
func (a *adapter) CancelOrder(ctx context.Context, r *venuepb.CancelOrderRequest) (*venuepb.CancelOrderResponse, error) {
	if err := a.call(ctx, "CancelOrder", r.TenantId, r.State); err != nil {
		return nil, err
	}
	return &venuepb.CancelOrderResponse{}, nil
}
func (a *adapter) Describe(ctx context.Context, _ *venuepb.DescribeRequest) (*venuepb.DescribeResponse, error) {
	if err := a.call(ctx, "Describe", "", nil); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return proto.Clone(a.description).(*venuepb.DescribeResponse), nil
}
func (a *adapter) ListInstruments(ctx context.Context, _ *venuepb.ListInstrumentsRequest) (*venuepb.ListInstrumentsResponse, error) {
	if err := a.call(ctx, "ListInstruments", "", nil); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return proto.Clone(a.catalog).(*venuepb.ListInstrumentsResponse), nil
}
func (a *adapter) QueryOrder(ctx context.Context, r *venuepb.QueryOrderRequest) (*venuepb.QueryOrderResponse, error) {
	if err := a.call(ctx, "QueryOrder", r.TenantId, r.State); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return proto.Clone(&venuepb.QueryOrderResponse{State: a.view, Fills: a.fills, Reason: "fixture observation", ExecutedQuantity: &commonpb.Decimal{Coefficient: 7, Exponent: -10}}).(*venuepb.QueryOrderResponse), nil
}

type rawCodec struct{ a *adapter }

func (c rawCodec) Name() string { return "proto" }
func (c rawCodec) Marshal(v any) ([]byte, error) {
	msg := v.(proto.Message)
	c.a.mu.Lock()
	raw, ok := c.a.raw[string(msg.ProtoReflect().Descriptor().FullName())]
	c.a.mu.Unlock()
	if ok {
		return bytes.Clone(raw), nil
	}
	return proto.Marshal(msg)
}
func (c rawCodec) Unmarshal(raw []byte, v any) error { return proto.Unmarshal(raw, v.(proto.Message)) }
func serveAdapter(t *testing.T, a *adapter, config *tls.Config, address string) (string, *grpc.Server) {
	t.Helper()
	if address == "" {
		address = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	opts := []grpc.ServerOption{grpc.ForceServerCodec(rawCodec{a}), grpc.StatsHandler(connectionStats{a})}
	if config != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(config)))
	}
	server := grpc.NewServer(opts...)
	venuepb.RegisterVenueAdapterServiceServer(server, a)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("venue server did not close")
		}
	})
	return listener.Addr().String(), server
}
func state() *orderpb.OrderState {
	return &orderpb.OrderState{OrderId: "fixture-order", PortfolioId: "fixture-portfolio", InstrumentId: "fixture-instrument", Venue: "MOCK", VenueAccountId: "fixture-account", Side: orderpb.Side_SIDE_BUY, OrderedQuantity: &commonpb.Decimal{Coefficient: 7, Exponent: -10}, LimitPrice: &commonpb.Decimal{Coefficient: 123456789, Exponent: -10}}
}
func fill() *orderpb.Fill {
	return &orderpb.Fill{OrderId: "fixture-order", FillId: "fixture-fill", InstrumentId: "fixture-instrument", Venue: "MOCK", VenueAccountId: "fixture-account", Side: orderpb.Side_SIDE_BUY, Quantity: &commonpb.Decimal{Coefficient: 7, Exponent: -10}, Price: &commonpb.Decimal{Coefficient: 123456789, Exponent: -10}, ExecutedAt: timestamppb.New(time.Unix(1700000000, 123456789))}
}
func setup(t *testing.T) (*adapter, *worker, *workloadServer, *grpc.ClientConn) {
	return setupRequirements(t, true)
}
func setupRequirements(t *testing.T, requireKnown bool) (*adapter, *worker, *workloadServer, *grpc.ClientConn) {
	t.Helper()
	binary(t)
	ca := newCA(t)
	client := ca.issue(t, clientID, false)
	source, socket := startWorkload(t, ca, client)
	a := newAdapter()
	endpoint, _ := serveAdapter(t, a, serverTLS(ca, ca.issue(t, serverID, false)), "")
	config := configuration(endpoint, socket)
	config["require_verified_account"] = requireKnown
	config["require_order_types"] = requireKnown
	w, r := startWorker(t, config)
	if r.Status != "connected" {
		t.Fatal("authenticated Rust connection refused")
	}
	return a, w, source, goConnection(t, endpoint, ca, client)
}
func rpc(t *testing.T, w *worker, method string, st *orderpb.OrderState) response {
	t.Helper()
	raw := []byte{}
	if st != nil {
		raw = marshal(t, st)
	}
	return w.call(map[string]any{"method": method, "state": raw, "timeout_ms": 2000})
}
func requireOK(t *testing.T, r response) {
	t.Helper()
	if r.Status != "ok" || r.Code != 0 {
		t.Fatal("Rust RPC unexpectedly refused")
	}
}
func exact(t *testing.T, raw []byte, want proto.Message) {
	t.Helper()
	got := want.ProtoReflect().New().Interface()
	if proto.Unmarshal(raw, got) != nil || !proto.Equal(got, want) || !bytes.Equal(raw, marshal(t, want)) {
		t.Fatal("canonical response bytes or optional/unknown fields differ")
	}
}

func TestRustVenueActualTLSAndRPC(t *testing.T) {
	t.Run("five-rpcs-and-full-request-attribution", func(t *testing.T) {
		a, w, source, conn := setup(t)
		st := state()
		a.configure(func() { a.fills = []*orderpb.Fill{fill()} })
		goOwner := execution.NewGRPCVenue("MOCK", "fixture-account", conn, "fixture-tenant")
		client := venuepb.NewVenueAdapterServiceClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		want, err := goOwner.Execute(ctx, st)
		if err != nil {
			t.Fatal(err)
		}
		r := rpc(t, w, "Execute", st)
		requireOK(t, r)
		if len(r.Fills) != len(want) {
			t.Fatal("fill cardinality differs")
		}
		for i, f := range want {
			exact(t, r.Fills[i], f)
		}
		if err = goOwner.CancelOrder(ctx, st); err != nil {
			t.Fatal(err)
		}
		requireOK(t, rpc(t, w, "CancelOrder", st))
		d, err := client.Describe(ctx, &venuepb.DescribeRequest{})
		if err != nil {
			t.Fatal(err)
		}
		r = rpc(t, w, "Describe", nil)
		requireOK(t, r)
		exact(t, r.Response, d)
		catalog, err := client.ListInstruments(ctx, &venuepb.ListInstrumentsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		r = rpc(t, w, "ListInstruments", nil)
		requireOK(t, r)
		exact(t, r.Response, catalog)
		view, err := goOwner.QueryOrder(ctx, st)
		if err != nil || view.State != execution.OrderViewWorking {
			t.Fatal("Go query oracle failed")
		}
		r = rpc(t, w, "QueryOrder", st)
		requireOK(t, r)
		if r.View != "Working" {
			t.Fatal("working state differs")
		}
		exact(t, r.Response, &venuepb.QueryOrderResponse{State: a.view, Fills: a.fills, Reason: view.Reason, ExecutedQuantity: view.ExecutedQuantity})
		a.mu.Lock()
		for _, call := range a.seen {
			if call.state != nil && (call.tenant != "fixture-tenant" || !proto.Equal(call.state, st)) {
				t.Fatal("request state or tenant changed")
			}
		}
		a.mu.Unlock()
		w.close(source)
	})
	t.Run("unknown-duplicate-and-noncanonical-wire", func(t *testing.T) {
		a, w, source, conn := setup(t)
		st := state()
		st.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 199, protowire.VarintType), 77))
		raw := marshal(t, st)
		field := st.ProtoReflect().Descriptor().Fields().ByName("limit_price").Number()
		fragment := marshal(t, &commonpb.Decimal{Coefficient: 9})
		raw = protowire.AppendBytes(protowire.AppendTag(raw, field, protowire.BytesType), fragment)
		var normalized orderpb.OrderState
		if proto.Unmarshal(raw, &normalized) != nil {
			t.Fatal("Go state normalization failed")
		}
		f := fill()
		f.Price.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x81, 0x00})
		fillRaw := marshal(t, f)
		priceField := f.ProtoReflect().Descriptor().Fields().ByName("price").Number()
		fillRaw = protowire.AppendBytes(protowire.AppendTag(fillRaw, priceField, protowire.BytesType), marshal(t, &commonpb.Decimal{Coefficient: 11}))
		responseRaw := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), fillRaw)
		responseRaw = append(responseRaw, 0xb8, 0x0c, 0x81, 0x00)
		a.configure(func() { a.raw["venue.v1.ExecuteResponse"] = responseRaw })
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		goOwner := execution.NewGRPCVenue("MOCK", "fixture-account", conn, "fixture-tenant")
		want, err := goOwner.Execute(ctx, &normalized)
		if err != nil {
			t.Fatal(err)
		}
		r := w.call(map[string]any{"method": "Execute", "state": raw, "timeout_ms": 2000})
		requireOK(t, r)
		if len(r.Fills) != 1 {
			t.Fatal("merged fill absent")
		}
		exact(t, r.Fills[0], want[0])
		var full venuepb.ExecuteResponse
		if proto.Unmarshal(responseRaw, &full) != nil {
			t.Fatal("Go response decode failed")
		}
		exact(t, r.Response, &full)
		a.mu.Lock()
		for _, call := range a.seen {
			if call.method == "Execute" && !proto.Equal(call.state, &normalized) {
				t.Fatal("merged request metadata lost")
			}
		}
		a.mu.Unlock()
		w.close(source)
	})
	t.Run("query-three-state-and-unimplemented", func(t *testing.T) {
		a, w, source, conn := setup(t)
		owner := execution.NewGRPCVenue("MOCK", "fixture-account", conn, "fixture-tenant")
		cases := []struct {
			wire    venuepb.OrderViewState
			goState execution.OrderViewState
			rust    string
		}{{0, execution.OrderViewIndeterminate, "Indeterminate"}, {1, execution.OrderViewIndeterminate, "Indeterminate"}, {2, execution.OrderViewUnknown, "Unknown"}, {3, execution.OrderViewWorking, "Working"}, {4, execution.OrderViewPartiallyFilled, "PartiallyFilled"}, {5, execution.OrderViewFilled, "Filled"}, {6, execution.OrderViewRejected, "Rejected"}, {7, execution.OrderViewCancelled, "Cancelled"}, {8, execution.OrderViewExpired, "Expired"}, {999, execution.OrderViewIndeterminate, "Indeterminate"}}
		for _, tc := range cases {
			a.configure(func() { a.view = tc.wire })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			want, err := owner.QueryOrder(ctx, state())
			cancel()
			if err != nil || want.State != tc.goState {
				t.Fatal("Go enum oracle differs")
			}
			r := rpc(t, w, "QueryOrder", state())
			requireOK(t, r)
			if r.View != tc.rust {
				t.Fatal("unknown/terminal query semantics differ")
			}
		}
		a.configure(func() { a.fail["QueryOrder"] = codes.Unimplemented })
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		want, err := owner.QueryOrder(ctx, state())
		cancel()
		if err != nil || want.State != execution.OrderViewIndeterminate {
			t.Fatal("Go legacy adapter oracle differs")
		}
		r := rpc(t, w, "QueryOrder", state())
		requireOK(t, r)
		if r.View != "Indeterminate" || len(r.Fills) != 0 {
			t.Fatal("unimplemented became affirmative venue result")
		}
		a.configure(func() { a.fail["QueryOrder"] = codes.Unavailable })
		r = rpc(t, w, "QueryOrder", state())
		if r.Status != "error" || r.Code != int(codes.Unavailable) || r.View == "Unknown" {
			t.Fatal("transport error became venue absence")
		}
		w.close(source)
	})
	t.Run("cancel-deadline-is-not-confirmed", func(t *testing.T) {
		a, w, source, conn := setup(t)
		a.configure(func() { a.wait["CancelOrder"] = true })
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := execution.NewGRPCVenue("MOCK", "fixture-account", conn, "fixture-tenant").CancelOrder(ctx, state())
		cancel()
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatal("Go cancellation oracle differs")
		}
		r := w.call(map[string]any{"method": "CancelOrder", "state": marshal(t, state()), "timeout_ms": 50})
		if r.Status != "error" || r.Code != int(codes.DeadlineExceeded) {
			t.Fatal("ambiguous cancellation was confirmed")
		}
		w.close(source)
	})
}

type connectionStats struct{ a *adapter }

func (s connectionStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (s connectionStats) HandleConn(_ context.Context, v stats.ConnStats) {
	switch v.(type) {
	case *stats.ConnBegin:
		s.a.active.Add(1)
	case *stats.ConnEnd:
		s.a.active.Add(-1)
	}
}
func (s connectionStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (s connectionStats) HandleRPC(context.Context, stats.RPCStats)                       {}

func TestRustVenueWireRefusals(t *testing.T) {
	for _, name := range []string{"malformed-request", "oversized-request", "malformed-response", "oversized-response"} {
		t.Run(name, func(t *testing.T) {
			a, w, source, conn := setup(t)
			bad := []byte{0x0a, 0xff}
			large := protowire.AppendBytes(protowire.AppendTag(nil, 199, protowire.BytesType), make([]byte, 1<<20))
			request := marshal(t, state())
			switch name {
			case "malformed-request":
				request = bad
			case "oversized-request":
				request = large
			case "malformed-response":
				a.configure(func() { a.raw["venue.v1.ExecuteResponse"] = bad })
			case "oversized-response":
				a.configure(func() { a.raw["venue.v1.ExecuteResponse"] = large })
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				result, err := venuepb.NewVenueAdapterServiceClient(conn).Execute(ctx, &venuepb.ExecuteRequest{State: state(), TenantId: "fixture-tenant"})
				cancel()
				if err != nil || len(result.ProtoReflect().GetUnknown()) <= 1<<20 {
					t.Fatal("Go-valid oversized response control failed")
				}
			}
			r := w.call(map[string]any{"method": "Execute", "state": request, "timeout_ms": 2000})
			if r.Status != "error" || r.Code == 0 || len(r.Fills) != 0 {
				t.Fatal("malformed or bounded input produced success")
			}
			wantCode := codes.InvalidArgument
			if name == "oversized-request" {
				wantCode = codes.ResourceExhausted
			}
			if name == "oversized-response" {
				wantCode = codes.OutOfRange
			}
			if r.Code != int(wantCode) {
				t.Fatal("wrong canonical wire refusal")
			}
			if name == "malformed-request" || name == "oversized-request" {
				a.mu.Lock()
				n := len(a.seen)
				a.mu.Unlock()
				if n != 0 {
					t.Fatal("invalid request reached adapter")
				}
			}
			a.configure(func() { delete(a.raw, "venue.v1.ExecuteResponse") })
			requireOK(t, rpc(t, w, "Describe", nil))
			w.close(source)
		})
	}
}
