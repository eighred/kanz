package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/testidentity"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/order"
)

type countedStateStore struct {
	store *order.MemoryStore
	calls atomic.Int32
}

func (s *countedStateStore) Load(ctx context.Context, id string) (*orderpb.OrderState, int64, error) {
	s.calls.Add(1)
	return s.store.Load(ctx, id)
}

func TestStateReadListenerRequiresExactMCPIdentityAndHasNoBroaderServices(t *testing.T) {
	authority, err := testidentity.NewAuthority(spiffeid.RequireTrustDomainFromString(transport.TrustDomain))
	if err != nil {
		t.Fatal(err)
	}
	issue := func(sa string) *testidentity.Source {
		t.Helper()
		id, err := transport.ServiceID("kanz-services", sa)
		if err != nil {
			t.Fatal(err)
		}
		source, err := authority.Issue(id, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	omsSource, mcpSource, stranger := issue("oms"), issue("mcp"), issue("api-gateway")
	store := &countedStateStore{store: order.NewMemoryStore()}
	row := &orderpb.OrderState{OrderId: "O", PortfolioId: "P", InstrumentId: "I", Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		OrderedQuantity: &commonpb.Decimal{Coefficient: 9007199254740993, Exponent: -9}, AsOf: timestamppb.New(time.Now().UTC())}
	if err := store.store.Create(context.Background(), row, nil); err != nil {
		t.Fatal(err)
	}
	addr, release := localPort(t)
	release()
	stop, err := serveStateRead(config.Config{StateQueryListen: addr, Tenant: "t1"}, &transport.Mesh{Source: omsSource}, store, readSurfaceLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	if extra, err := serveStateRead(config.Config{StateQueryListen: addr, Tenant: "t1"}, &transport.Mesh{Source: omsSource}, store, readSurfaceLogger()); err == nil || extra != nil {
		t.Fatal("occupied state listener did not refuse startup synchronously")
	}
	omsID, err := transport.ServiceID("kanz-services", "oms")
	if err != nil {
		t.Fatal(err)
	}
	connect := func(option grpc.DialOption) *grpc.ClientConn {
		t.Helper()
		conn, err := grpc.NewClient(addr, option)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	conn := connect(transport.ClientDialOption(mcpSource, transport.AuthorizeServices(omsID)))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := querypb.NewOrderStateReadServiceClient(conn).OrderStateRead(ctx, &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
	if err != nil || resp.GetOrderedQuantity().GetValue().GetCoefficient() != 9007199254740993 {
		t.Fatalf("actual authenticated read: %v %v", resp, err)
	}
	_, err = orderpb.NewOrderQueryServiceClient(conn).ListPendingApprovals(ctx, &orderpb.ListPendingApprovalsRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("broader approval API exposed to MCP identity: %v", err)
	}
	before := store.calls.Load()
	for _, option := range []grpc.DialOption{transport.ClientDialOption(stranger, transport.AuthorizeServices(omsID)), grpc.WithTransportCredentials(insecure.NewCredentials())} {
		foreign := connect(option)
		probeCtx, probeCancel := context.WithTimeout(context.Background(), time.Second)
		_, err := querypb.NewOrderStateReadServiceClient(foreign).OrderStateRead(probeCtx, &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
		probeCancel()
		if err == nil {
			t.Fatal("non-MCP or plaintext principal reached recorded state")
		}
	}
	if store.calls.Load() != before {
		t.Fatal("rejected transport principal reached the store")
	}
}

func TestStateReadListenerRefusesUnconfiguredIdentity(t *testing.T) {
	addr, release := localPort(t)
	defer release()
	if stop, err := serveStateRead(config.Config{StateQueryListen: addr, Tenant: "t1"}, &transport.Mesh{}, order.NewMemoryStore(), readSurfaceLogger()); err == nil || stop != nil {
		t.Fatal("state read opened without mTLS")
	}
}
