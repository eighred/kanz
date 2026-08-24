package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/order"
)

// THE READ SURFACE'S THREE SILENT FAILURES (#399, #406, #643).
//
// serveOrderQuery was already a named function returning its product and an
// error. It had no test, and every property its own doc claims fails quietly:
//
//  1. A BIND FAILURE MUST BE RETURNED SYNCHRONOUSLY. Served on a goroutine and
//     logged, a taken port leaves the pod READY with no read surface — an outage
//     that looks like a routing bug.
//  2. THE PORT MUST ACTUALLY SERVE the order.v1 service. A server built and never
//     registered accepts the connection and fails every call with Unimplemented,
//     which reads to a caller as a version mismatch rather than a wiring fault.
//  3. THE STOP FUNCTION MUST STOP IT. A returned no-op means the deferred stop in
//     runConsumers does nothing and the listener outlives the store it reads
//     from — serving from a closed pool, which is the shape the outbox relay's
//     join comment describes one layer up.

func readSurfaceLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// localPort reserves a port and hands back both the address and a closer, so a
// test can decide whether the port is free or taken when serveOrderQuery runs.
func localPort(t *testing.T) (addr string, release func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	return lis.Addr().String(), func() { _ = lis.Close() }
}

// A BIND FAILURE IS RETURNED, NOT LOGGED. The port is deliberately still held.
func TestABindFailureIsReturnedRatherThanLeavingThePodReady(t *testing.T) {
	addr, release := localPort(t)
	defer release() // the port stays TAKEN for the duration of this test

	mesh := &transport.Mesh{}
	stop, err := serveOrderQuery(config.Config{GRPCListen: addr, Tenant: "acme"},
		mesh, order.NewMemoryStore(), nil, readSurfaceLogger())
	if err == nil {
		if stop != nil {
			stop()
		}
		t.Fatal("serveOrderQuery accepted a port it could not bind — startup would continue, the " +
			"pod would report Ready, and the read surface the gateway proxies would not exist")
	}
	if stop != nil {
		t.Error("a stop function was returned alongside the bind failure — a caller deferring it " +
			"would be stopping a server that was never started")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("the error does not name the address that could not be bound: %v", err)
	}
}

// THE SURFACE ACTUALLY ANSWERS. Registration is a separate step from listening,
// and skipping it produces a port that accepts connections and Unimplemented on
// every call.
func TestTheReadSurfaceServesTheOrderQueryService(t *testing.T) {
	addr, release := localPort(t)
	release() // free it: serveOrderQuery binds this address itself

	stop, err := serveOrderQuery(config.Config{GRPCListen: addr, Tenant: "acme"},
		&transport.Mesh{}, order.NewMemoryStore(), nil, readSurfaceLogger())
	if err != nil {
		t.Fatalf("serveOrderQuery: %v", err)
	}
	defer stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// An empty store answers with an empty list, or refuses on the tenant gate.
	// EITHER proves the service is REGISTERED; only Unimplemented means the port
	// is open and the surface is not on it.
	_, err = orderpb.NewOrderQueryServiceClient(conn).ListOrders(ctx,
		&orderpb.ListOrdersRequest{PortfolioId: "fund-alpha", Limit: 1})
	if err != nil && strings.Contains(err.Error(), "Unimplemented") {
		t.Fatalf("the port is open and order.v1 is not registered on it — a caller reads this as a "+
			"version mismatch rather than a wiring fault: %v", err)
	}
}

// THE STOP FUNCTION RELEASES THE PORT. If it did not, the listener would outlive
// the store it reads from.
func TestStoppingTheReadSurfaceReleasesThePort(t *testing.T) {
	addr, release := localPort(t)
	release()

	stop, err := serveOrderQuery(config.Config{GRPCListen: addr, Tenant: "acme"},
		&transport.Mesh{}, order.NewMemoryStore(), nil, readSurfaceLogger())
	if err != nil {
		t.Fatalf("serveOrderQuery: %v", err)
	}
	stop()

	// Rebinding is the proof: a GracefulStop that returned without closing the
	// listener leaves this failing with "address already in use".
	var lis net.Listener
	for i := 0; i < 50; i++ {
		if lis, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the port is still held after stop() — the read surface outlives the store whose "+
			"lifetime it borrows: %v", err)
	}
	_ = lis.Close()
}
