package main

import (
	"context"
	"errors"
	"net"
	"sync"
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
	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
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
	stop, err := serveStateRead(context.Background(), config.Config{StateQueryListen: addr, Tenant: "t1"}, &transport.Mesh{Source: omsSource}, store, readSurfaceLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	if extra, err := serveStateRead(context.Background(), config.Config{StateQueryListen: addr, Tenant: "t1"}, &transport.Mesh{Source: omsSource}, store, readSurfaceLogger()); err == nil || extra != nil {
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
	if stop, err := serveStateRead(context.Background(), config.Config{StateQueryListen: addr, Tenant: "t1"}, &transport.Mesh{}, order.NewMemoryStore(), readSurfaceLogger()); err == nil || stop != nil {
		t.Fatal("state read opened without mTLS")
	}
}

type blockedStateReader struct {
	entered     chan context.Context
	canceled    chan error
	finished    chan struct{}
	release     chan struct{}
	cooperative bool
	calls       atomic.Int32
	finishOnce  sync.Once
}

func newBlockedStateReader(cooperative bool) *blockedStateReader {
	return &blockedStateReader{entered: make(chan context.Context, 2), canceled: make(chan error, 2), finished: make(chan struct{}), release: make(chan struct{}), cooperative: cooperative}
}

func (s *blockedStateReader) Load(ctx context.Context, _ string) (*orderpb.OrderState, int64, error) {
	s.calls.Add(1)
	defer s.finishOnce.Do(func() { close(s.finished) })
	s.entered <- ctx
	if s.cooperative {
		<-ctx.Done()
		s.canceled <- ctx.Err()
		<-s.release
	} else {
		<-s.release
	}
	return nil, 0, ctx.Err()
}

func blockedStateEndpoint(t *testing.T, root context.Context, reader grpcsrv.StateReader) (querypb.OrderStateReadServiceClient, func() error, string) {
	t.Helper()
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
	addr, release := localPort(t)
	release()
	stop, err := serveStateRead(root, config.Config{StateQueryListen: addr, Tenant: "t1"}, &transport.Mesh{Source: issue("oms")}, reader, readSurfaceLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	omsID, err := transport.ServiceID("kanz-services", "oms")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, transport.ClientDialOption(issue("mcp"), transport.AuthorizeServices(omsID)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return querypb.NewOrderStateReadServiceClient(conn), stop, addr
}

func TestStateReadListenerCancelsAndJoinsAcceptedReadWithoutCallerDeadline(t *testing.T) {
	for _, rootCancellation := range []bool{false, true} {
		name := "explicit-stop"
		if rootCancellation {
			name = "OMS-root-cancellation"
		}
		t.Run(name, func(t *testing.T) {
			root, cancelRoot := context.WithCancel(context.Background())
			defer cancelRoot()
			reader := newBlockedStateReader(true)
			client, stop, addr := blockedStateEndpoint(t, root, reader)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(reader.release) }) }
			t.Cleanup(release)
			rpcDone := make(chan error, 1)
			go func() {
				_, err := client.OrderStateRead(context.Background(), &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
				rpcDone <- err
			}()
			var rpcCtx context.Context
			select {
			case rpcCtx = <-reader.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("authenticated read did not enter the loader")
			}
			deadline, bounded := rpcCtx.Deadline()
			if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
				t.Fatal("server did not impose its own finite read deadline")
			}
			secondCtx, cancelSecond := context.WithTimeout(context.Background(), time.Second)
			_, second := client.OrderStateRead(secondCtx, &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "second"})
			cancelSecond()
			if status.Code(second) != codes.ResourceExhausted || reader.calls.Load() != 1 {
				t.Fatalf("second MCP read entered or queued for the execution store: %v calls=%d", second, reader.calls.Load())
			}
			if rootCancellation {
				cancelRoot()
			}
			stopDone := make(chan error, 1)
			if !rootCancellation {
				go func() { stopDone <- stop() }()
			}
			select {
			case err := <-reader.canceled:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("accepted read cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("accepted read did not observe lifecycle cancellation")
			}
			if rootCancellation {
				go func() { stopDone <- stop() }()
			}
			// Keep driver cleanup behind an explicit fixture barrier. Transport
			// cancellation alone must not be reported as a joined store reader.
			select {
			case err := <-stopDone:
				t.Fatalf("stop returned before the accepted Load joined: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			release()
			select {
			case err := <-stopDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("cooperative read and Serve did not join inside the stop budget")
			}
			select {
			case <-reader.finished:
			default:
				t.Fatal("stop claimed success before Load returned")
			}
			select {
			case err := <-rpcDone:
				if err == nil {
					t.Fatal("stopped RPC returned successful state")
				}
			case <-time.After(time.Second):
				t.Fatal("accepted RPC did not return after joined stop")
			}
			if conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
				_ = conn.Close()
				t.Fatal("state listener remained open after joined stop")
			}
		})
	}
}

func TestStateReadListenerRefusesUnjoinedReaderWithoutHidingServeCleanup(t *testing.T) {
	reader := newBlockedStateReader(false)
	client, stop, addr := blockedStateEndpoint(t, context.Background(), reader)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(reader.release) }) }
	t.Cleanup(release)
	rpcDone := make(chan error, 1)
	go func() {
		_, err := client.OrderStateRead(context.Background(), &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
		rpcDone <- err
	}()
	select {
	case <-reader.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("authenticated read did not enter noncooperative fixture")
	}
	started := time.Now()
	err := stop()
	if !errors.Is(err, grpcsrv.ErrStateReadNotJoined) || errors.Is(err, errStateReadServeNotJoined) || errors.Is(err, errStateReadTransportNotJoined) {
		t.Fatalf("cleanup confused the unjoined Load with transport/Serve cleanup: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 6*time.Second {
		t.Fatalf("read cleanup exceeded the trusted stop budget: %v", elapsed)
	}
	if conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("refused read join left the listener open")
	}
	// The fixture owns the blocked implementation and releases it after the
	// expected refusal. Production cannot manufacture this cooperation.
	release()
	if err := stop(); err != nil {
		t.Fatalf("released fixture did not finish local teardown: %v", err)
	}
	select {
	case <-reader.finished:
	case <-time.After(time.Second):
		t.Fatal("test-owned noncooperative reader leaked")
	}
	select {
	case <-rpcDone:
	case <-time.After(time.Second):
		t.Fatal("test-owned RPC did not join")
	}
}

func TestStateReadListenerEnforcesDeadlineWithoutCallerBudget(t *testing.T) {
	reader := newBlockedStateReader(true)
	client, stop, _ := blockedStateEndpoint(t, context.Background(), reader)
	close(reader.release)
	rpcDone := make(chan error, 1)
	go func() {
		_, err := client.OrderStateRead(context.Background(), &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
		rpcDone <- err
	}()
	select {
	case <-reader.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("authenticated read did not enter loader")
	}
	select {
	case err := <-reader.canceled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("server deadline did not reach loader: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("caller without deadline held the loader past the server budget")
	}
	select {
	case err := <-rpcDone:
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("expired read result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expired RPC did not return")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}
