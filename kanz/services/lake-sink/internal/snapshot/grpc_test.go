package snapshot

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/testidentity"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestSnapshotGRPCAuthenticatesExactGatewayBeforeDelegation(t *testing.T) {
	domain, err := spiffeid.TrustDomainFromString("snapshot.test")
	if err != nil {
		t.Fatal(err)
	}
	ca, err := testidentity.NewAuthority(domain)
	if err != nil {
		t.Fatal(err)
	}
	gateway := spiffeid.RequireFromString("spiffe://snapshot.test/ns/kanz-services/sa/api-gateway")
	lake := spiffeid.RequireFromString("spiffe://snapshot.test/ns/kanz-services/sa/lake-sink")
	issue := func(id spiffeid.ID) transport.Source {
		s, err := ca.Issue(id, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	policy := auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"creator": {ActionCreate}}})
	lifetime, stopOwner := context.WithCancel(context.Background())
	defer stopOwner()
	owner := &Server{store: &Store{tenant: "tenant-a", authorizer: policy}, lifetime: lifetime}
	server, err := NewGRPCServer(issue(lake), gateway, owner)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close(); <-done })
	for _, peer := range []spiffeid.ID{gateway, spiffeid.RequireFromString("spiffe://snapshot.test/ns/kanz-services/sa/other")} {
		t.Run(peer.Path(), func(t *testing.T) {
			conn, err := grpc.NewClient(listener.Addr().String(), transport.ClientDialOption(issue(peer), transport.AuthorizeServices(lake)))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			// Invalid business input reaches validation only after a real successful
			// authenticated HTTP/2 call. No DB is needed for this transport control.
			_, err = lakepb.NewLakeSnapshotServiceClient(conn).CreateSnapshot(ctx, &lakepb.CreateSnapshotRequest{Caller: creator("tenant-a", "operator"), Input: &lakepb.CreateSnapshotInput{RequestId: "request-authentication"}})
			if peer == gateway {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("authenticated call did not reach input validation: %s", status.Code(err))
				}
				stopOwner()
				// Leave the actual authenticated HTTP/2 connection and listener
				// alive: admission must refuse before runtime drain calls Stop.
				_, err = lakepb.NewLakeSnapshotServiceClient(conn).CreateSnapshot(ctx, &lakepb.CreateSnapshotRequest{Caller: creator("tenant-a", "operator"), Input: snapshotInput("request-after-stop")})
				if status.Code(err) != codes.Canceled {
					t.Fatalf("established channel bypassed owner lifetime: %s", status.Code(err))
				}
			} else if status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded {
				t.Fatalf("foreign workload reached owner: %s", status.Code(err))
			}
		})
	}
}

func TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction(t *testing.T) {
	policy := auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{
		"creator": {ActionCreate}, "reader": {ActionRead}, "generic": {"read"},
	}})
	server := &Server{store: &Store{tenant: "tenant-a", authorizer: policy}, lifetime: context.Background()}
	for _, method := range []struct {
		name    string
		role    string
		request func(*lakepb.GatewayCaller) proto.Message
	}{
		{lakepb.LakeSnapshotService_CreateSnapshot_FullMethodName, "creator", func(c *lakepb.GatewayCaller) proto.Message {
			return &lakepb.CreateSnapshotRequest{Caller: c, Input: snapshotInput("request-delegation")}
		}},
		{lakepb.LakeSnapshotService_GetSnapshot_FullMethodName, "reader", func(c *lakepb.GatewayCaller) proto.Message {
			return &lakepb.GetSnapshotRequest{Caller: c, SnapshotId: "snapshot-delegation"}
		}},
		{lakepb.LakeSnapshotService_QuerySnapshot_FullMethodName, "reader", func(c *lakepb.GatewayCaller) proto.Message {
			return &lakepb.QuerySnapshotRequest{Caller: c, SnapshotId: "snapshot-delegation"}
		}},
	} {
		t.Run(method.name, func(t *testing.T) {
			for _, variant := range []string{"valid", "missing", "foreign", "restricted", "generic", "unknown-wrapper", "wrong-method", "wrong-action"} {
				t.Run(variant, func(t *testing.T) {
					caller := &lakepb.GatewayCaller{Subject: "operator", Tenant: "tenant-a", Roles: []string{method.role}}
					switch variant {
					case "missing":
						caller = nil
					case "foreign":
						caller.Tenant = "tenant-b"
					case "restricted":
						caller.Portfolios = []string{"portfolio"}
					case "generic":
						caller.Roles = []string{"generic"}
					case "wrong-action":
						caller.Roles = []string{"creator"}
						if method.role == "creator" {
							caller.Roles = []string{"reader"}
						}
					}
					req := method.request(caller)
					if variant == "unknown-wrapper" {
						req.ProtoReflect().SetUnknown([]byte{0xa0, 6, 1})
					}
					name := method.name
					if variant == "wrong-method" {
						name = "/unknown/Call"
					}
					called := false
					_, err := server.delegate(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: name}, func(ctx context.Context, received any) (any, error) {
						called = true
						deadline, bounded := ctx.Deadline()
						if !bounded || time.Until(deadline) > 30*time.Second || received != req {
							t.Fatal("delegation changed request or lost work deadline")
						}
						return nil, nil
					})
					if variant == "valid" {
						if err != nil || !called {
							t.Fatal("explicit own-tenant delegation refused")
						}
					} else if err == nil || called {
						t.Fatal("refused caller reached resource owner")
					}
				})
			}
		})
	}
}

type heldSnapshotAuthorization struct{ entered, cancelled, release chan struct{} }

func (a *heldSnapshotAuthorization) Authorize(ctx context.Context, _ auth.Request) auth.Decision {
	close(a.entered)
	<-ctx.Done()
	close(a.cancelled)
	<-a.release // models handler-owned rollback/release that must finish before Stop
	return auth.Decision{Allow: false}
}

func TestSnapshotGRPCStopJoinsCancelledHeldHandler(t *testing.T) {
	domain := spiffeid.RequireTrustDomainFromString("snapshot.test")
	ca, err := testidentity.NewAuthority(domain)
	if err != nil {
		t.Fatal(err)
	}
	gateway := spiffeid.RequireFromString("spiffe://snapshot.test/ns/kanz-services/sa/api-gateway")
	lake := spiffeid.RequireFromString("spiffe://snapshot.test/ns/kanz-services/sa/lake-sink")
	issue := func(id spiffeid.ID) transport.Source {
		s, err := ca.Issue(id, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	held := &heldSnapshotAuthorization{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	owner := &Server{store: &Store{tenant: "tenant-a", authorizer: held}, lifetime: context.Background()}
	server, err := NewGRPCServer(issue(lake), gateway, owner)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
	t.Cleanup(func() { release(); server.Stop(); _ = listener.Close(); <-served })
	conn, err := grpc.NewClient(listener.Addr().String(), transport.ClientDialOption(issue(gateway), transport.AuthorizeServices(lake)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	called := make(chan error, 1)
	go func() {
		_, err := lakepb.NewLakeSnapshotServiceClient(conn).GetSnapshot(ctx, &lakepb.GetSnapshotRequest{Caller: creator("tenant-a", "operator"), SnapshotId: "snapshot-held-handler"})
		called <- err
	}()
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("authenticated handler did not start")
	}
	stopped := make(chan struct{})
	go func() { server.Stop(); close(stopped) }()
	select {
	case <-held.cancelled:
	case <-ctx.Done():
		t.Fatal("Stop did not cancel active handler")
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned before cancelled handler released ownership")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("Stop did not join released handler")
	}
	select {
	case err := <-called:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-ctx.Done():
		t.Fatal("cancelled client remained open")
	}
}
