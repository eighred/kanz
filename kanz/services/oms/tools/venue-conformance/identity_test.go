package venueconformance

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"google.golang.org/grpc/codes"
)

func TestRustVenueRegistration(t *testing.T) {
	for _, name := range []string{"identity-and-catalog", "wrong-mic", "wrong-account", "unverified-account", "unknown-order-types", "catalog-unavailable", "catalog-empty", "explicit-legacy-unknown-retains-unknown"} {
		t.Run(name, func(t *testing.T) {
			a, w, source, _ := setupRequirements(t, name != "explicit-legacy-unknown-retains-unknown")
			a.configure(func() {
				switch name {
				case "wrong-mic":
					a.description.Mic = "OTHER"
				case "wrong-account":
					a.description.Account = "other-account"
				case "unverified-account":
					a.description.AccountVerified = false
				case "unknown-order-types":
					a.description.SupportedOrderTypes = nil
				case "catalog-unavailable":
					a.fail["ListInstruments"] = codes.Unavailable
				case "catalog-empty":
					a.catalog.Instruments = nil
				case "explicit-legacy-unknown-retains-unknown":
					a.description.AccountVerified = false
					a.description.SupportedOrderTypes = nil
				}
			})
			r := rpc(t, w, "Register", nil)
			switch name {
			case "wrong-mic", "wrong-account", "unverified-account", "unknown-order-types":
				if r.Status != "error" || r.Code != int(codes.FailedPrecondition) {
					t.Fatal("unknown or mismatched capability admitted")
				}
				a.mu.Lock()
				for _, call := range a.seen {
					if call.method == "ListInstruments" {
						t.Error("catalog requested after failed identity gate")
					}
				}
				a.mu.Unlock()
				closed := rpc(t, w, "Execute", state())
				if closed.Status != "error" || closed.Code != int(codes.Unavailable) {
					t.Fatal("registration refusal left client usable")
				}
			default:
				requireOK(t, r)
				exact(t, r.Response, a.description)
				if name == "catalog-unavailable" {
					if r.CatalogCode != int(codes.Unavailable) || len(r.Catalog) != 0 {
						t.Fatal("failed catalog became successful empty capability")
					}
				} else {
					if r.CatalogCode != 0 {
						t.Fatal("catalog refused")
					}
					exact(t, r.Catalog, a.catalog)
				}
			}
			w.close(source)
		})
	}
}

func TestRustVenueIdentityRefusals(t *testing.T) {
	for _, name := range []string{"exact-peer-positive", "wrong-peer", "wrong-trust-domain", "untrusted-root", "expired-peer", "plaintext-server", "missing-workload-source"} {
		t.Run(name, func(t *testing.T) {
			binary(t)
			ca := newCA(t)
			client := ca.issue(t, clientID, false)
			source, socket := startWorkload(t, ca, client)
			serverCA := ca
			id := serverID
			switch name {
			case "wrong-peer":
				id = "spiffe://kanz.internal/ns/kanz-services/sa/other"
			case "wrong-trust-domain":
				id = "spiffe://other.internal/ns/kanz-services/sa/venue-fixture"
			case "untrusted-root":
				serverCA = newCA(t)
			}
			// The negative server certificate never changes its acceptance of the
			// valid client. This isolates the client's peer/root verification.
			config := serverTLS(ca, serverCA.issue(t, id, name == "expired-peer"))
			if name == "plaintext-server" {
				config = nil
			}
			a := newAdapter()
			endpoint, _ := serveAdapter(t, a, config, "")
			if name == "wrong-peer" || name == "wrong-trust-domain" || name == "untrusted-root" {
				control := identity(serverCA, client)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				dialer := tls.Dialer{Config: tlsconfig.MTLSClientConfig(control, control, tlsconfig.AuthorizeID(spiffeid.RequireFromString(id)))}
				conn, err := dialer.DialContext(ctx, "tcp", endpoint)
				cancel()
				if err != nil {
					t.Fatal("otherwise valid negative peer control failed")
				}
				_ = conn.Close()
			}
			init := configuration(endpoint, socket)
			if name == "missing-workload-source" {
				init["workload_socket"] = socket + "-absent"
			}
			w, r := startWorker(t, init)
			if name == "exact-peer-positive" {
				if r.Status != "connected" {
					t.Fatal("positive authenticated peer refused")
				}
				requireOK(t, rpc(t, w, "Describe", nil))
			} else {
				if r.Status != "error" {
					t.Fatal("unauthenticated transport admitted")
				}
				a.mu.Lock()
				n := len(a.seen)
				a.mu.Unlock()
				if n != 0 {
					t.Fatal("application RPC reached refused peer")
				}
			}
			w.close(source)
		})
	}
}

func TestRustVenueLifetime(t *testing.T) {
	t.Run("close-joins-live-channel-and-workload-before-exit", func(t *testing.T) {
		binary(t)
		ca := newCA(t)
		source, socket := startWorkload(t, ca, ca.issue(t, clientID, false))
		a := newAdapter()
		endpoint, _ := serveAdapter(t, a, serverTLS(ca, ca.issue(t, serverID, false)), "")
		w, r := startWorker(t, configuration(endpoint, socket))
		if r.Status != "connected" {
			t.Fatal("connection failed")
		}
		requireOK(t, rpc(t, w, "Describe", nil))
		if a.active.Load() == 0 || source.Active() == 0 {
			t.Fatal("positive owned connections not established")
		}
		r = w.call(map[string]any{"method": "Close", "timeout_ms": 2000})
		if r.Status != "ok" || !r.SessionsClosed {
			t.Fatal("Close failed")
		}
		deadline := time.Now().Add(2 * time.Second)
		for (a.active.Load() != 0 || source.Active() != 0) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if a.active.Load() != 0 || source.Active() != 0 {
			t.Fatal("owned connection survived Close")
		}
		select {
		case err := <-w.done:
			t.Fatalf("process exit masked connection cleanup: %v", err)
		default:
		}
		finishClosed(t, w)
	})
	t.Run("cancel-connect-joins-before-process-exit", func(t *testing.T) {
		binary(t)
		ca := newCA(t)
		source, socket := startWorkload(t, ca, ca.issue(t, clientID, false))
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		accepted := make(chan net.Conn, 1)
		stop := make(chan struct{})
		done := make(chan struct{})
		t.Cleanup(func() {
			close(stop)
			_ = listener.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("pending transport fixture did not close")
			}
		})
		go func() {
			defer close(done)
			c, e := listener.Accept()
			if e == nil {
				defer func() { _ = c.Close() }()
				accepted <- c
				<-stop
			}
		}()
		init := configuration(listener.Addr().String(), socket)
		init["timeout_ms"] = 3000
		init["cancel_connect_after_ms"] = 1000
		w, r := startWorker(t, init)
		if r.Status != "cancelled" {
			t.Fatal("controlled pending-connect cancellation absent")
		}
		var conn net.Conn
		select {
		case conn = <-accepted:
		case <-time.After(time.Second):
			t.Fatal("cancellation did not reach actual transport")
		}
		defer func() { _ = conn.Close() }()
		if source.Started() == 0 {
			t.Fatal("workload source never acquired")
		}
		if err = conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		// Drain the finite TLS ClientHello, then require transport closure while the process lives.
		b := make([]byte, 4096)
		total := 0
		for {
			n, e := conn.Read(b)
			total += n
			if total > 65536 {
				t.Fatal("unexpected transport volume")
			}
			if e != nil {
				if ne, ok := e.(net.Error); ok && ne.Timeout() {
					t.Fatal("cancelled transport remains open")
				}
				break
			}
		}
		select {
		case err := <-w.done:
			t.Fatalf("process exit masked cancellation: %v", err)
		default:
		}
		w.close(source)
	})
}

func finishClosed(t *testing.T, w *worker) {
	t.Helper()
	if err := w.input.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-w.done:
		if err != nil {
			t.Fatal("closed worker failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closed worker did not exit")
	}
	w.closed = true
	w.cancel()
}

func TestRustVenueEmptyFillIsNotFailure(t *testing.T) {
	a, w, source, conn := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	want, err := venuepb.NewVenueAdapterServiceClient(conn).Execute(ctx, &venuepb.ExecuteRequest{TenantId: "fixture-tenant", State: state()})
	if err != nil || len(want.Fills) != 0 {
		t.Fatal("Go zero-fill oracle failed")
	}
	r := rpc(t, w, "Execute", state())
	requireOK(t, r)
	exact(t, r.Response, want)
	if len(r.Fills) != 0 {
		t.Fatal("fabricated fill")
	}
	a.configure(func() { a.fail["Execute"] = codes.Unavailable })
	r = rpc(t, w, "Execute", state())
	if r.Status != "error" || r.Code != int(codes.Unavailable) {
		t.Fatal("failed execution became valid zero fills")
	}
	w.close(source)
}

func TestRustVenueRotationReconnect(t *testing.T) {
	binary(t)
	ca := newCA(t)
	first := ca.issue(t, clientID, false)
	source, socket := startWorkload(t, ca, first)
	a := newAdapter()
	config := serverTLS(ca, ca.issue(t, serverID, false))
	endpoint, server := serveAdapter(t, a, config, "")
	w, r := startWorker(t, configuration(endpoint, socket))
	if r.Status != "connected" {
		t.Fatal("initial connection failed")
	}
	requireOK(t, rpc(t, w, "Describe", nil))
	a.mu.Lock()
	initial := append([]string(nil), a.serials...)
	a.mu.Unlock()
	if len(initial) != 1 || initial[0] != first.Certificates[0].SerialNumber.String() {
		t.Fatal("initial client identity not observed")
	}
	second := ca.issue(t, clientID, false)
	source.Set(t, identity(ca, second))
	server.Stop()
	_, server = serveAdapter(t, a, config, endpoint)
	deadline := time.Now().Add(10 * time.Second)
	observed := false
	for time.Now().Before(deadline) {
		remaining := min(time.Until(deadline).Milliseconds(), int64(2000))
		if remaining < 1 {
			break
		}
		r = w.call(map[string]any{"method": "Describe", "timeout_ms": remaining})
		a.mu.Lock()
		for _, serial := range a.serials {
			if serial == second.Certificates[0].SerialNumber.String() {
				observed = true
			}
		}
		a.mu.Unlock()
		if r.Status == "ok" && observed {
			break
		}
		// Workload API streaming has no consumer receipt acknowledgement. A
		// reconnect that raced delivery may legitimately use the former SVID;
		// require a later fresh connection to observe the actual new identity.
		server.Stop()
		_, server = serveAdapter(t, a, config, endpoint)
		time.Sleep(20 * time.Millisecond)
	}
	if !observed || r.Status != "ok" || !time.Now().Before(deadline) {
		t.Fatal("fresh identity was not used after actual reconnect")
	}
	w.close(source)
}

func TestRustVenueHTTP1Refusal(t *testing.T) {
	binary(t)
	ca := newCA(t)
	source, socket := startWorkload(t, ca, ca.issue(t, clientID, false))
	config := serverTLS(ca, ca.issue(t, serverID, false))
	config.NextProtos = []string{"http/1.1"}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2 {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
			b := make([]byte, 4096)
			_, _ = conn.Read(b)
			_ = conn.Close()
		}
	}()
	control := identity(ca, ca.issue(t, clientID, false))
	clientTLS := tlsconfig.MTLSClientConfig(control, control, tlsconfig.AuthorizeID(spiffeid.RequireFromString(serverID)))
	clientTLS.NextProtos = []string{"http/1.1"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	dialer := tls.Dialer{Config: clientTLS}
	conn, err := dialer.DialContext(ctx, "tcp", listener.Addr().String())
	cancel()
	if err != nil {
		t.Fatal("authenticated HTTP/1 positive control failed")
	}
	negotiated := conn.(*tls.Conn).ConnectionState().NegotiatedProtocol
	_ = conn.Close()
	if negotiated != "http/1.1" {
		t.Fatal("HTTP/1 positive control did not negotiate protocol")
	}
	w, r := startWorker(t, configuration(listener.Addr().String(), socket))
	if r.Status != "error" {
		t.Fatal("HTTP/1-only TLS peer admitted as gRPC")
	}
	w.close(source)
	_ = listener.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP/1 fixture did not close")
	}
}

func TestGoVenueFixtureOracle(t *testing.T) {
	ca := newCA(t)
	a := newAdapter()
	a.fills = []*orderpb.Fill{fill()}
	endpoint, _ := serveAdapter(t, a, serverTLS(ca, ca.issue(t, serverID, false)), "")
	conn := goConnection(t, endpoint, ca, ca.issue(t, clientID, false))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := venuepb.NewVenueAdapterServiceClient(conn).Execute(ctx, &venuepb.ExecuteRequest{TenantId: "fixture-tenant", State: state()})
	if err != nil || len(r.GetFills()) != 1 {
		t.Fatal("synthetic mTLS Go oracle unavailable")
	}
	exact(t, marshal(t, r.Fills[0]), fill())
}

func TestRustVenueIPv6RPC(t *testing.T) {
	binary(t)
	ca := newCA(t)
	client := ca.issue(t, clientID, false)
	source, socket := startWorkload(t, ca, client)
	a := newAdapter()
	endpoint, _ := serveAdapter(t, a, serverTLS(ca, ca.issue(t, serverID, false)), "[::1]:0")
	conn := goConnection(t, endpoint, ca, client)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	want, err := venuepb.NewVenueAdapterServiceClient(conn).Describe(ctx, &venuepb.DescribeRequest{})
	cancel()
	if err != nil {
		t.Fatal("Go IPv6 authenticated control failed")
	}
	w, r := startWorker(t, configuration(endpoint, socket))
	if r.Status != "connected" {
		t.Fatal("Rust IPv6 authenticated connection failed")
	}
	r = rpc(t, w, "Describe", nil)
	requireOK(t, r)
	exact(t, r.Response, want)
	w.close(source)
}
