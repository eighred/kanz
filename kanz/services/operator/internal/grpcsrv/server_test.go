package grpcsrv

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"

	"github.com/kanz-eng/kanz/internal/execution"
	"github.com/kanz-eng/kanz/internal/venueadapter/exchangeauth"
	"github.com/kanz-eng/kanz/services/operator/internal/estate"
	"github.com/kanz-eng/kanz/services/operator/internal/provision"
	"github.com/kanz-eng/kanz/services/operator/internal/secrets"
	"github.com/kanz-eng/kanz/services/operator/internal/venueproof"
)

type stubReader struct {
	nodes    []estate.Node
	clusters []estate.Cluster
	err      error
}

func (s stubReader) ListNodes(context.Context) ([]estate.Node, error) {
	return s.nodes, s.err
}
func (s stubReader) ListClusters(context.Context) ([]estate.Cluster, error) {
	return s.clusters, s.err
}

func TestListNodesMapsStatusToEnum(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	srv := New(stubReader{nodes: []estate.Node{
		{Name: "london", Status: estate.StatusReady, Roles: []string{"control-plane"}, Region: "europe", KubeletVersion: "v1.31.3", CreatedAt: created},
		{Name: "tokyo", Status: estate.StatusNotReady, Region: "asia"},
		{Name: "ghost", Status: estate.StatusUnknown},
	}})
	resp, err := srv.ListNodes(context.Background(), &operatorpb.ListNodesRequest{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(resp.GetNodes()) != 3 {
		t.Fatalf("want 3 nodes, got %d", len(resp.GetNodes()))
	}
	got := map[string]*operatorpb.Node{}
	for _, n := range resp.GetNodes() {
		got[n.GetName()] = n
	}
	if got["london"].GetStatus() != operatorpb.NodeStatus_NODE_STATUS_READY {
		t.Errorf("london status = %v", got["london"].GetStatus())
	}
	if got["tokyo"].GetStatus() != operatorpb.NodeStatus_NODE_STATUS_NOT_READY {
		t.Errorf("tokyo status = %v", got["tokyo"].GetStatus())
	}
	if got["ghost"].GetStatus() != operatorpb.NodeStatus_NODE_STATUS_UNSPECIFIED {
		t.Errorf("ghost status = %v, want UNSPECIFIED (deny-by-default)", got["ghost"].GetStatus())
	}
	if got["london"].GetCreatedAt().AsTime().UTC() != created {
		t.Errorf("london created_at = %v, want %v", got["london"].GetCreatedAt().AsTime(), created)
	}
	if got["tokyo"].GetCreatedAt() != nil {
		t.Errorf("tokyo created_at = %v, want nil (zero CreatedAt must not leak the epoch)", got["tokyo"].GetCreatedAt())
	}
	if got["ghost"].GetCreatedAt() != nil {
		t.Errorf("ghost created_at = %v, want nil (zero CreatedAt must not leak the epoch)", got["ghost"].GetCreatedAt())
	}
}

func TestListClustersMapsCounts(t *testing.T) {
	srv := New(stubReader{clusters: []estate.Cluster{{Region: "usa", Online: 2, Offline: 1}}})
	resp, err := srv.ListClusters(context.Background(), &operatorpb.ListClustersRequest{})
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(resp.GetClusters()) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(resp.GetClusters()))
	}
	c := resp.GetClusters()[0]
	if c.GetRegion() != "usa" || c.GetOnline() != 2 || c.GetOffline() != 1 {
		t.Errorf("cluster = %+v", c)
	}
}

func TestReaderErrorMapsToInternal(t *testing.T) {
	srv := New(stubReader{err: errors.New("boom")})
	_, err := srv.ListNodes(context.Background(), &operatorpb.ListNodesRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
}

type stubProvisioner struct {
	gotReq     provision.Request
	id         string
	provisions []provision.Provision
	err        error
}

func (s *stubProvisioner) AddNode(_ context.Context, r provision.Request) (string, error) {
	s.gotReq = r
	return s.id, s.err
}
func (s *stubProvisioner) List(context.Context) ([]provision.Provision, error) {
	return s.provisions, s.err
}

func TestAddNodeDecodesRequestAndReturnsID(t *testing.T) {
	sp := &stubProvisioner{id: "provision-london-abcde"}
	srv := NewWithProvisioner(stubReader{}, sp)
	resp, err := srv.AddNode(context.Background(), &operatorpb.AddNodeRequest{
		Hostname: "london", Ip: "10.0.0.5", SshPort: 22, SshUser: "root", SshPrivateKey: []byte("PEM")})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if resp.GetProvisionId() != "provision-london-abcde" {
		t.Errorf("provision_id = %q", resp.GetProvisionId())
	}
	if sp.gotReq.IP != "10.0.0.5" || string(sp.gotReq.SSHKey) != "PEM" {
		t.Errorf("request not decoded: %+v", sp.gotReq)
	}
}

func TestAddNodeRejectsEmptyKey(t *testing.T) {
	srv := NewWithProvisioner(stubReader{}, &stubProvisioner{})
	_, err := srv.AddNode(context.Background(), &operatorpb.AddNodeRequest{Ip: "10.0.0.5", SshUser: "root"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for empty key, got %v", err)
	}
}

func TestTestConnectionReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	srv := New(stubReader{})
	resp, err := srv.TestConnection(context.Background(), &operatorpb.TestConnectionRequest{Ip: host, SshPort: int32(port)})
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !resp.GetReachable() {
		t.Errorf("want reachable, got %+v", resp)
	}
	if resp.GetLatencyMs() < 0 {
		t.Errorf("latency should be non-negative, got %d", resp.GetLatencyMs())
	}
}

func TestTestConnectionUnreachable(t *testing.T) {
	// Bind then immediately close, so the port is (almost certainly) closed.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	resp, err := New(stubReader{}).TestConnection(context.Background(), &operatorpb.TestConnectionRequest{Ip: host, SshPort: int32(port)})
	if err != nil {
		t.Fatalf("TestConnection returned an RPC error for an unreachable host (should be a normal result): %v", err)
	}
	if resp.GetReachable() {
		t.Errorf("want unreachable")
	}
	if resp.GetMessage() == "" {
		t.Errorf("unreachable result should carry a message")
	}
}

func TestTestConnectionRejectsEmptyIP(t *testing.T) {
	_, err := New(stubReader{}).TestConnection(context.Background(), &operatorpb.TestConnectionRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for empty ip, got %v", err)
	}
}

type stubNodeOps struct {
	cordoned, uncordoned, drained string
	regionNode, region            string
	err                           error
}

func (s *stubNodeOps) Cordon(_ context.Context, name string) error { s.cordoned = name; return s.err }
func (s *stubNodeOps) Uncordon(_ context.Context, name string) error {
	s.uncordoned = name
	return s.err
}
func (s *stubNodeOps) Drain(_ context.Context, name string) error { s.drained = name; return s.err }
func (s *stubNodeOps) SetRegion(_ context.Context, name, region string) error {
	s.regionNode, s.region = name, region
	return s.err
}

func TestCordonUncordonDrainDelegate(t *testing.T) {
	sn := &stubNodeOps{}
	srv := New(stubReader{}).WithNodeOps(sn)

	if _, err := srv.Cordon(context.Background(), &operatorpb.CordonRequest{Name: "london"}); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	if _, err := srv.Uncordon(context.Background(), &operatorpb.UncordonRequest{Name: "london"}); err != nil {
		t.Fatalf("Uncordon: %v", err)
	}
	if _, err := srv.Drain(context.Background(), &operatorpb.DrainRequest{Name: "london"}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if sn.cordoned != "london" || sn.uncordoned != "london" || sn.drained != "london" {
		t.Errorf("delegation failed: %+v", sn)
	}
}

func TestCordonRejectsEmptyName(t *testing.T) {
	srv := New(stubReader{}).WithNodeOps(&stubNodeOps{})
	if _, err := srv.Cordon(context.Background(), &operatorpb.CordonRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestCordonUnconfiguredIsUnimplemented(t *testing.T) {
	if _, err := New(stubReader{}).Cordon(context.Background(), &operatorpb.CordonRequest{Name: "x"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("want Unimplemented when nodeOps is nil, got %v", err)
	}
}

// The ListNodes handler must carry the new estate fields to the proto, or the TUI
// never sees drain status.
func TestListNodesSurfacesSchedulableAndEvictable(t *testing.T) {
	srv := New(stubReader{nodes: []estate.Node{
		{Name: "london", Status: estate.StatusReady, Schedulable: false, EvictablePods: 3},
	}})
	resp, err := srv.ListNodes(context.Background(), &operatorpb.ListNodesRequest{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(resp.GetNodes()) != 1 {
		t.Fatalf("want 1 node")
	}
	n := resp.GetNodes()[0]
	if n.GetSchedulable() || n.GetEvictablePods() != 3 {
		t.Errorf("ListNodes must map schedulable/evictable_pods from estate: schedulable=%v evictable=%d", n.GetSchedulable(), n.GetEvictablePods())
	}
}

func TestSetNodeRegionDelegates(t *testing.T) {
	sn := &stubNodeOps{}
	srv := New(stubReader{}).WithNodeOps(sn)
	if _, err := srv.SetNodeRegion(context.Background(), &operatorpb.SetNodeRegionRequest{Name: "london", Region: "asia"}); err != nil {
		t.Fatalf("SetNodeRegion: %v", err)
	}
	if sn.regionNode != "london" || sn.region != "asia" {
		t.Errorf("delegation failed: node=%q region=%q", sn.regionNode, sn.region)
	}
}

func TestSetNodeRegionRejectsEmpty(t *testing.T) {
	srv := New(stubReader{}).WithNodeOps(&stubNodeOps{})
	if _, err := srv.SetNodeRegion(context.Background(), &operatorpb.SetNodeRegionRequest{Region: "asia"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty name → want InvalidArgument, got %v", err)
	}
	if _, err := srv.SetNodeRegion(context.Background(), &operatorpb.SetNodeRegionRequest{Name: "london"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty region → want InvalidArgument, got %v", err)
	}
}

func TestSetNodeRegionUnconfiguredIsUnimplemented(t *testing.T) {
	if _, err := New(stubReader{}).SetNodeRegion(context.Background(), &operatorpb.SetNodeRegionRequest{Name: "x", Region: "y"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("want Unimplemented, got %v", err)
	}
}

type stubSecretStore struct {
	venue   string
	keys    secrets.VenueKeys
	setErr  error
	listOut []secrets.VenueStatus
	logged  []string // anything the store "would log" — asserted empty of key material
}

func (s *stubSecretStore) SetVenueKeys(_ context.Context, venue string, k secrets.VenueKeys) error {
	s.venue, s.keys = venue, k
	return s.setErr
}
func (s *stubSecretStore) ListVenues(context.Context) ([]secrets.VenueStatus, error) {
	return s.listOut, nil
}

func TestSetVenueKeysDelegates(t *testing.T) {
	st := &stubSecretStore{}
	srv := New(stubReader{}).WithSecrets(st)
	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{
		Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p",
	})
	if err != nil {
		t.Fatalf("SetVenueKeys: %v", err)
	}
	if st.venue != "okx" || st.keys.APIKey != "k" || st.keys.APISecret != "s" || st.keys.Passphrase != "p" {
		t.Errorf("store got venue=%q keys=%+v, want the request's values", st.venue, st.keys)
	}
	if len(st.logged) != 0 {
		t.Errorf("store must never be handed anything to log, got %v", st.logged)
	}
}

func TestSetVenueKeysNilStoreUnimplemented(t *testing.T) {
	srv := New(stubReader{}) // no WithSecrets
	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p"})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", status.Code(err))
	}
}

func TestSetVenueKeysValidationInvalidArgument(t *testing.T) {
	st := &stubSecretStore{}
	srv := New(stubReader{}).WithSecrets(st)
	// binance with a passphrase is rejected before the store is touched.
	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{Venue: "binance", ApiKey: "k", ApiSecret: "s", Passphrase: "p"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
	if st.venue != "" {
		t.Errorf("store must NOT be called on a validation failure")
	}
}

func TestSetVenueKeysWriteErrorInternal(t *testing.T) {
	st := &stubSecretStore{setErr: errors.New("backend down")}
	srv := New(stubReader{}).WithSecrets(st)
	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p"})
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
}

func TestListVenueKeysMapsPresence(t *testing.T) {
	st := &stubSecretStore{listOut: []secrets.VenueStatus{{Venue: "okx", Configured: true}, {Venue: "binance", Configured: false}}}
	srv := New(stubReader{}).WithSecrets(st)
	resp, err := srv.ListVenueKeys(context.Background(), &operatorpb.ListVenueKeysRequest{})
	if err != nil {
		t.Fatalf("ListVenueKeys: %v", err)
	}
	if len(resp.GetVenues()) != 2 || resp.GetVenues()[0].GetVenue() != "okx" || !resp.GetVenues()[0].GetConfigured() {
		t.Errorf("venues = %+v, want okx configured + binance not", resp.GetVenues())
	}
}

func TestListVenueKeysNilStoreUnimplemented(t *testing.T) {
	_, err := New(stubReader{}).ListVenueKeys(context.Background(), &operatorpb.ListVenueKeysRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", status.Code(err))
	}
}

type stubVenueProver struct {
	calls    int
	gotKeys  secrets.VenueKeys
	gotVenue string
	uid      string
	err      error
}

func (p *stubVenueProver) ProveAccount(_ context.Context, venue string, keys secrets.VenueKeys) (string, error) {
	p.calls++
	p.gotVenue, p.gotKeys = venue, keys
	return p.uid, p.err
}

func TestSetVenueKeysProvesBeforeWrite(t *testing.T) {
	st := &stubSecretStore{}
	prover := &stubVenueProver{uid: "4711"}
	srv := New(stubReader{}).WithSecrets(st).WithVenueProof(prover)

	resp, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{
		Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p",
	})
	if err != nil {
		t.Fatalf("SetVenueKeys: %v", err)
	}
	if resp.GetExchangeAccountId() != "4711" {
		t.Errorf("exchange_account_id = %q, want 4711", resp.GetExchangeAccountId())
	}
	if st.venue != "okx" || st.keys.APIKey != "k" {
		t.Errorf("store did not receive the write: %+v", st)
	}
	if prover.calls != 1 {
		t.Errorf("prover called %d times, want 1", prover.calls)
	}
	wantKeys := secrets.VenueKeys{APIKey: "k", APISecret: "s", Passphrase: "p"}
	if prover.gotVenue != "okx" {
		t.Errorf("prover got venue = %q, want okx", prover.gotVenue)
	}
	if prover.gotKeys != wantKeys {
		t.Errorf("prover got keys = %+v, want %+v", prover.gotKeys, wantKeys)
	}
	if prover.gotVenue != st.venue || prover.gotKeys != st.keys {
		t.Errorf("prover was handed venue=%q keys=%+v but the store received venue=%q keys=%+v — prove/write mismatch",
			prover.gotVenue, prover.gotKeys, st.venue, st.keys)
	}
}

func TestSetVenueKeysRefusesWriteWhenProofFails(t *testing.T) {
	st := &stubSecretStore{}
	prover := &stubVenueProver{err: errors.New("boom")}
	srv := New(stubReader{}).WithSecrets(st).WithVenueProof(prover)

	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{
		Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if st.venue != "" {
		t.Errorf("store must NOT be called when proof fails, got venue=%q", st.venue)
	}
}

func TestSetVenueKeysProofErrorIsSanitized(t *testing.T) {
	st := &stubSecretStore{}
	prover := &stubVenueProver{err: errors.New("credential KEYSENTINEL rejected: exchange body {\"msg\":\"bad key\"}")}
	srv := New(stubReader{}).WithSecrets(st).WithVenueProof(prover)

	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{
		Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p",
	})
	if err == nil {
		t.Fatal("SetVenueKeys: want error when proof fails")
	}
	msg := status.Convert(err).Message()
	if strings.Contains(msg, "KEYSENTINEL") {
		t.Errorf("gRPC message leaked credential material: %q", msg)
	}
	if strings.Contains(msg, "bad key") || strings.Contains(msg, "exchange body") {
		t.Errorf("gRPC message leaked the exchange response body: %q", msg)
	}
	want := "the exchange could not be asked which account these credentials belong to"
	if msg != want {
		t.Errorf("message = %q, want fixed reason %q", msg, want)
	}
}

// TestSetVenueKeysProofErrorSanitizesAllBranches drives every branch of
// proofReason (server.go), not just the catch-all. The *execution.APIError
// branch is the only one that interpolates a field off the exchange's own
// error object (fmt.Sprintf of apiErr.Code) — a plausible future edit is to
// also interpolate apiErr.Msg, which is the exchange's own words and must
// never reach the caller. EXCHANGEMSGSENTINEL below exists to make that
// regression fail this test by name, not just "some string changed".
func TestSetVenueKeysProofErrorSanitizesAllBranches(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "no endpoint configured",
			err:  venueproof.ErrNoEndpoint,
			want: "venue key proof is required but no exchange endpoint is configured for this venue",
		},
		{
			name: "egress denied",
			err:  execution.ErrEgressDenied,
			want: "the exchange rejected these credentials",
		},
		{
			name: "exchange api error",
			err:  &execution.APIError{Code: 50113, Msg: "EXCHANGEMSGSENTINEL"},
			want: "the exchange rejected these credentials (code 50113)",
		},
		{
			name: "unsupported venue",
			err:  exchangeauth.ErrUnsupportedVenue,
			want: "this venue cannot be proved",
		},
		{
			name: "unmapped error (catch-all)",
			err:  errors.New("some unmapped internal detail"),
			want: "the exchange could not be asked which account these credentials belong to",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &stubSecretStore{}
			prover := &stubVenueProver{err: tc.err}
			srv := New(stubReader{}).WithSecrets(st).WithVenueProof(prover)

			_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{
				Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p",
			})
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
			}
			msg := status.Convert(err).Message()
			if msg != tc.want {
				t.Errorf("message = %q, want fixed reason %q", msg, tc.want)
			}
			if strings.Contains(msg, "KEYSENTINEL") || strings.Contains(msg, "SECSENTINEL") || strings.Contains(msg, "PASSENTINEL") {
				t.Errorf("gRPC message leaked credential material: %q", msg)
			}
			if strings.Contains(msg, "EXCHANGEMSGSENTINEL") {
				t.Errorf("gRPC message leaked the exchange's own error text: %q", msg)
			}
			if st.venue != "" {
				t.Errorf("store must NOT be called when proof fails, got venue=%q", st.venue)
			}
		})
	}
}

func TestSetVenueKeysWithoutProverWritesUnproven(t *testing.T) {
	st := &stubSecretStore{}
	srv := New(stubReader{}).WithSecrets(st) // no WithVenueProof

	resp, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{
		Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p",
	})
	if err != nil {
		t.Fatalf("SetVenueKeys: %v", err)
	}
	if resp.GetExchangeAccountId() != "" {
		t.Errorf("exchange_account_id = %q, want empty (unproven S4a behaviour)", resp.GetExchangeAccountId())
	}
	if st.venue != "okx" {
		t.Errorf("store did not receive the write: %+v", st)
	}
}

func TestSetVenueKeysValidatesBeforeProving(t *testing.T) {
	st := &stubSecretStore{}
	prover := &stubVenueProver{uid: "4711"}
	srv := New(stubReader{}).WithSecrets(st).WithVenueProof(prover)

	// binance with a passphrase is an invalid key set.
	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{
		Venue: "binance", ApiKey: "k", ApiSecret: "s", Passphrase: "p",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
	if prover.calls != 0 {
		t.Errorf("prover must NOT be called when validation fails, got %d calls", prover.calls)
	}
	if st.venue != "" {
		t.Errorf("store must NOT be called when validation fails")
	}
}

func TestListProvisionsMapsStatus(t *testing.T) {
	sp := &stubProvisioner{provisions: []provision.Provision{
		{ID: "p1", Hostname: "london", Status: provision.StatusJoined},
		{ID: "p2", Hostname: "tokyo", Status: provision.StatusFailed, Message: "dial refused"},
	}}
	srv := NewWithProvisioner(stubReader{}, sp)
	resp, err := srv.ListProvisions(context.Background(), &operatorpb.ListProvisionsRequest{})
	if err != nil {
		t.Fatalf("ListProvisions: %v", err)
	}
	if len(resp.GetProvisions()) != 2 {
		t.Fatalf("want 2, got %d", len(resp.GetProvisions()))
	}
	if resp.GetProvisions()[0].GetStatus() != operatorpb.ProvisionStatus_PROVISION_STATUS_JOINED {
		t.Errorf("p1 status = %v", resp.GetProvisions()[0].GetStatus())
	}
	if resp.GetProvisions()[1].GetMessage() != "dial refused" {
		t.Errorf("p2 message not surfaced")
	}
}
