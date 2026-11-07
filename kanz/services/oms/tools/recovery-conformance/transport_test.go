package recoveryconformance

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/testidentity"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/conformancetestkit"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const clientID = "spiffe://kanz.internal/ns/kanz-services/sa/oms"
const serverID = "spiffe://kanz.internal/ns/kanz-services/sa/venue-fixture"

// venue answers independently supplied exchange observations. It does not
// project fills, decide recovery eligibility or manufacture persisted evidence.
type venue struct {
	venuepb.UnimplementedVenueAdapterServiceServer
	mu                                 sync.Mutex
	description                        *venuepb.DescribeResponse
	history                            *venuepb.QueryOrderResponse
	tenant, orderID                    string
	queryCode                          codes.Code
	queryWait                          bool
	queryDelay, describeDelay          time.Duration
	queryRelease                       <-chan struct{}
	queries, describes, writes, active atomic.Int32
}

func (v *venue) Describe(ctx context.Context, _ *venuepb.DescribeRequest) (*venuepb.DescribeResponse, error) {
	v.describes.Add(1)
	v.mu.Lock()
	description := proto.Clone(v.description).(*venuepb.DescribeResponse)
	delay := v.describeDelay
	v.mu.Unlock()
	if err := syntheticDelay(ctx, delay); err != nil {
		return nil, err
	}
	return description, nil
}
func (v *venue) QueryOrder(ctx context.Context, request *venuepb.QueryOrderRequest) (*venuepb.QueryOrderResponse, error) {
	v.queries.Add(1)
	v.mu.Lock()
	matched := request.GetTenantId() == v.tenant && request.GetState().GetOrderId() == v.orderID
	wait, code, release := v.queryWait, v.queryCode, v.queryRelease
	delay := v.queryDelay
	history := proto.Clone(v.history).(*venuepb.QueryOrderResponse)
	v.mu.Unlock()
	if !matched {
		return nil, status.Error(codes.PermissionDenied, "synthetic query attribution differs")
	}
	if err := syntheticDelay(ctx, delay); err != nil {
		return nil, err
	}
	if wait {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	if code != codes.OK {
		return nil, status.Error(code, "synthetic history unavailable")
	}
	return history, nil
}
func (v *venue) Execute(context.Context, *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	v.writes.Add(1)
	return nil, status.Error(codes.PermissionDenied, "recovery must not execute")
}
func (v *venue) CancelOrder(context.Context, *venuepb.CancelOrderRequest) (*venuepb.CancelOrderResponse, error) {
	v.writes.Add(1)
	return nil, status.Error(codes.PermissionDenied, "recovery must not cancel")
}

type venueStats struct{ v *venue }

func (s venueStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (s venueStats) HandleConn(_ context.Context, event stats.ConnStats) {
	switch event.(type) {
	case *stats.ConnBegin:
		s.v.active.Add(1)
	case *stats.ConnEnd:
		s.v.active.Add(-1)
	}
}
func (s venueStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (s venueStats) HandleRPC(context.Context, stats.RPCStats)                       {}

type transport struct {
	venue            *venue
	source           *conformancetestkit.Workload
	endpoint, socket string
	goConn           *grpc.ClientConn
	closeGo          func()
}

func startTransport(t *testing.T, v *venue) *transport {
	t.Helper()
	domain := spiffeid.RequireTrustDomainFromString("kanz.internal")
	authority, err := testidentity.NewAuthority(domain)
	if err != nil {
		t.Fatal("synthetic authority failed")
	}
	issue := func(id string) *testidentity.Source {
		source, err := authority.Issue(spiffeid.RequireFromString(id), time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal("synthetic identity failed")
		}
		return source
	}
	client, server := issue(clientID), issue(serverID)
	source, socket := conformancetestkit.StartWorkload(t, client)
	endpoint, _ := conformancetestkit.StartVenue(t, v, "", grpc.Creds(credentials.NewTLS(tlsconfig.MTLSServerConfig(server, server, tlsconfig.AuthorizeID(spiffeid.RequireFromString(clientID))))), grpc.StatsHandler(venueStats{v}), grpc.MaxSendMsgSize(4<<20), grpc.MaxRecvMsgSize(1<<20))
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(tlsconfig.MTLSClientConfig(client, client, tlsconfig.AuthorizeID(spiffeid.RequireFromString(serverID))))), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4<<20)))
	if err != nil {
		t.Fatal("Go venue client construction failed")
	}
	var closeOnce sync.Once
	closeGo := func() {
		closeOnce.Do(func() {
			if conn.Close() != nil {
				t.Error("Go venue client cleanup failed")
			}
		})
	}
	t.Cleanup(closeGo)
	return &transport{venue: v, source: source, endpoint: endpoint, socket: socket, goConn: conn, closeGo: closeGo}
}
func (f *fixture) start(t *testing.T, tr *transport) *worker {
	return f.startWithin(t, tr, 180*time.Second)
}
func (f *fixture) startWithin(t *testing.T, tr *transport, lifetime time.Duration) *worker {
	t.Helper()
	w, result := startWorkerWithin(t, map[string]any{"endpoint": "https://" + tr.endpoint, "workload_socket": tr.socket, "expected_peer": serverID, "tenant": f.tenant, "mic": "MOCK", "account": "fixture-account", "portfolio": "fixture-portfolio", "schema": f.schema, "timeout_ms": 10000}, lifetime, f.postgresURL)
	if result.Status != "connected" {
		t.Fatal("actual recovery owner refused verified fixture")
	}
	return w
}

func syntheticDelay(ctx context.Context, delay time.Duration) error {
	if delay < 0 || delay > 2*time.Second {
		return status.Error(codes.InvalidArgument, "synthetic delay outside bound")
	}
	if delay == 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}
