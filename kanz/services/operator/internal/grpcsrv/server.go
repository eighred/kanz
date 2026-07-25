// Package grpcsrv is the operator service's gRPC adapter: a thin translation
// of operator.v1.OperatorService onto the estate.Reader read model. It holds
// no logic — the node listing and grouping live in internal/estate; this layer
// only converts the read model to proto and maps errors to gRPC codes.
package grpcsrv

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"

	"github.com/kanz-eng/kanz/internal/execution"
	"github.com/kanz-eng/kanz/internal/venueadapter/exchangeauth"
	"github.com/kanz-eng/kanz/services/operator/internal/estate"
	"github.com/kanz-eng/kanz/services/operator/internal/provision"
	"github.com/kanz-eng/kanz/services/operator/internal/secrets"
	"github.com/kanz-eng/kanz/services/operator/internal/venueproof"
)

// Provisioner is the node-provisioning surface the operator gRPC depends on
// (provision.Provisioner satisfies it). Kept as an interface so the handler is
// unit-tested against a stub with no Kubernetes client.
//
// Probe belongs here rather than on its own interface because it has the same
// prerequisite as AddNode: a configured provisioner image to run a Job from. The two
// are enabled and disabled together, so one nil check governs both.
type Provisioner interface {
	AddNode(ctx context.Context, r provision.Request) (string, error)
	List(ctx context.Context) ([]provision.Provision, error)
	Probe(ctx context.Context, ip string, sshPort int32) (provision.ProbeResult, error)
}

// NodeOps is the node-lifecycle surface the operator gRPC depends on (nodeops.Ops
// satisfies it). Optional — nil in a deployment without the node-writer RBAC.
type NodeOps interface {
	Cordon(ctx context.Context, name string) error
	Uncordon(ctx context.Context, name string) error
	Drain(ctx context.Context, name string) error
	SetRegion(ctx context.Context, name, region string) error
}

// VenueProver asks the exchange which account a candidate credential belongs
// to, before it is stored (venueproof.Prover satisfies it). Optional — nil
// disables pre-write proof (S4a behaviour: the write happens unproven).
type VenueProver interface {
	ProveAccount(ctx context.Context, venue string, keys secrets.VenueKeys) (string, error)
}

// Server adapts an estate.Reader to the generated OperatorService interface.
type Server struct {
	operatorpb.UnimplementedOperatorServiceServer
	reader     estate.Reader
	prov       Provisioner
	nodeOps    NodeOps
	secrets    secrets.Store
	venueProof VenueProver
}

// New returns a read-only Server (no provisioning). Retained for callers/tests that
// only exercise ListNodes/ListClusters.
func New(r estate.Reader) *Server { return &Server{reader: r} }

// NewWithProvisioner returns a Server with the write path wired.
func NewWithProvisioner(r estate.Reader, p Provisioner) *Server {
	return &Server{reader: r, prov: p}
}

// WithNodeOps attaches the node-lifecycle surface and returns the server (builder
// style, so it composes with New / NewWithProvisioner without new constructors).
func (s *Server) WithNodeOps(ops NodeOps) *Server { s.nodeOps = ops; return s }

// WithSecrets attaches the write-only venue-credential surface (builder style).
func (s *Server) WithSecrets(store secrets.Store) *Server { s.secrets = store; return s }

// WithVenueProof attaches the pre-write venue-key proof surface (builder style).
// Nil (the zero value) is a valid, supported configuration: SetVenueKeys writes
// unproven, exactly as in S4a.
func (s *Server) WithVenueProof(p VenueProver) *Server { s.venueProof = p; return s }

// Register binds the server onto a grpc.ServiceRegistrar.
func (s *Server) Register(r grpc.ServiceRegistrar) {
	operatorpb.RegisterOperatorServiceServer(r, s)
}

func (s *Server) ListNodes(ctx context.Context, _ *operatorpb.ListNodesRequest) (*operatorpb.ListNodesResponse, error) {
	nodes, err := s.reader.ListNodes(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*operatorpb.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, &operatorpb.Node{
			Name:           n.Name,
			Status:         protoStatus(n.Status),
			Roles:          n.Roles,
			Region:         n.Region,
			KubeletVersion: n.KubeletVersion,
			CreatedAt:      nonZeroTimestamp(n.CreatedAt),
			Schedulable:    n.Schedulable,
			EvictablePods:  int32(n.EvictablePods),
		})
	}
	return &operatorpb.ListNodesResponse{Nodes: out}, nil
}

func (s *Server) ListClusters(ctx context.Context, _ *operatorpb.ListClustersRequest) (*operatorpb.ListClustersResponse, error) {
	clusters, err := s.reader.ListClusters(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*operatorpb.Cluster, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, &operatorpb.Cluster{
			Region:  c.Region,
			Online:  int32(c.Online),
			Offline: int32(c.Offline),
		})
	}
	return &operatorpb.ListClustersResponse{Clusters: out}, nil
}

func (s *Server) AddNode(ctx context.Context, req *operatorpb.AddNodeRequest) (*operatorpb.AddNodeResponse, error) {
	if s.prov == nil {
		return nil, status.Error(codes.Unimplemented, "provisioning not configured")
	}
	if req.GetIp() == "" || req.GetSshUser() == "" || len(req.GetSshPrivateKey()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ip, ssh_user and ssh_private_key are required")
	}
	id, err := s.prov.AddNode(ctx, provision.Request{
		Hostname: req.GetHostname(), IP: req.GetIp(), SSHPort: req.GetSshPort(),
		SSHUser: req.GetSshUser(), SSHKey: req.GetSshPrivateKey(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &operatorpb.AddNodeResponse{ProvisionId: id, Status: operatorpb.ProvisionStatus_PROVISION_STATUS_PENDING}, nil
}

func (s *Server) ListProvisions(ctx context.Context, _ *operatorpb.ListProvisionsRequest) (*operatorpb.ListProvisionsResponse, error) {
	if s.prov == nil {
		return &operatorpb.ListProvisionsResponse{}, nil
	}
	ps, err := s.prov.List(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*operatorpb.Provision, 0, len(ps))
	for _, p := range ps {
		out = append(out, &operatorpb.Provision{
			Id: p.ID, Hostname: p.Hostname, Status: provStatus(p.Status), Message: p.Message,
		})
	}
	return &operatorpb.ListProvisionsResponse{Provisions: out}, nil
}

func (s *Server) Cordon(ctx context.Context, req *operatorpb.CordonRequest) (*operatorpb.CordonResponse, error) {
	if err := s.nodeWrite(ctx, req.GetName(), func() error { return s.nodeOps.Cordon(ctx, req.GetName()) }); err != nil {
		return nil, err
	}
	return &operatorpb.CordonResponse{}, nil
}

func (s *Server) Uncordon(ctx context.Context, req *operatorpb.UncordonRequest) (*operatorpb.UncordonResponse, error) {
	if err := s.nodeWrite(ctx, req.GetName(), func() error { return s.nodeOps.Uncordon(ctx, req.GetName()) }); err != nil {
		return nil, err
	}
	return &operatorpb.UncordonResponse{}, nil
}

func (s *Server) Drain(ctx context.Context, req *operatorpb.DrainRequest) (*operatorpb.DrainResponse, error) {
	if err := s.nodeWrite(ctx, req.GetName(), func() error { return s.nodeOps.Drain(ctx, req.GetName()) }); err != nil {
		return nil, err
	}
	return &operatorpb.DrainResponse{}, nil
}

func (s *Server) SetNodeRegion(ctx context.Context, req *operatorpb.SetNodeRegionRequest) (*operatorpb.SetNodeRegionResponse, error) {
	// The region check only applies once we know ops is configured, so an unconfigured
	// deployment still returns Unimplemented (via nodeWrite) rather than InvalidArgument.
	if s.nodeOps != nil && req.GetRegion() == "" {
		return nil, status.Error(codes.InvalidArgument, "region is required")
	}
	if err := s.nodeWrite(ctx, req.GetName(), func() error {
		return s.nodeOps.SetRegion(ctx, req.GetName(), req.GetRegion())
	}); err != nil {
		return nil, err
	}
	return &operatorpb.SetNodeRegionResponse{}, nil
}

// nodeWrite is the shared guard+error mapping for the node-write handlers.
func (s *Server) nodeWrite(_ context.Context, name string, do func() error) error {
	if s.nodeOps == nil {
		return status.Error(codes.Unimplemented, "node operations not configured")
	}
	if name == "" {
		return status.Error(codes.InvalidArgument, "node name is required")
	}
	if err := do(); err != nil {
		if apierrors.IsNotFound(err) {
			return status.Error(codes.NotFound, err.Error())
		}
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}

func (s *Server) SetVenueKeys(ctx context.Context, req *operatorpb.SetVenueKeysRequest) (*operatorpb.SetVenueKeysResponse, error) {
	if s.secrets == nil {
		return nil, status.Error(codes.Unimplemented, "API management not configured")
	}
	keys := secrets.VenueKeys{APIKey: req.GetApiKey(), APISecret: req.GetApiSecret(), Passphrase: req.GetPassphrase()}
	if err := secrets.ValidateVenueKeys(req.GetVenue(), keys); err != nil {
		// The error names the venue and the field rule — NEVER the key material.
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	var accountID string
	if s.venueProof != nil {
		id, err := s.venueProof.ProveAccount(ctx, req.GetVenue(), keys)
		if err != nil {
			// Sanitized: the exchange's own words never reach the caller, and neither
			// does the credential. No write happens — the stored key set is untouched.
			return nil, status.Error(codes.FailedPrecondition, proofReason(err))
		}
		accountID = id
	}
	if err := s.secrets.SetVenueKeys(ctx, req.GetVenue(), keys); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &operatorpb.SetVenueKeysResponse{ExchangeAccountId: accountID}, nil
}

// proofReason maps a pre-write proof error onto a FIXED, sanitized reason
// string. The exchange's own response body (err.Error() from exchangeauth)
// must never reach the client — this is a closed set chosen by errors.Is/As,
// with a catch-all, not a passthrough of err.Error().
func proofReason(err error) string {
	var apiErr *execution.APIError
	switch {
	case errors.Is(err, venueproof.ErrNoEndpoint):
		return "venue key proof is required but no exchange endpoint is configured for this venue"
	case errors.Is(err, execution.ErrEgressDenied):
		return "the exchange rejected these credentials"
	case errors.As(err, &apiErr):
		// The numeric code only — apiErr.Msg is the exchange's own words and
		// must never reach the caller.
		return fmt.Sprintf("the exchange rejected these credentials (code %d)", apiErr.Code)
	case errors.Is(err, exchangeauth.ErrUnsupportedVenue):
		return "this venue cannot be proved"
	default:
		return "the exchange could not be asked which account these credentials belong to"
	}
}

func (s *Server) ListVenueKeys(ctx context.Context, _ *operatorpb.ListVenueKeysRequest) (*operatorpb.ListVenueKeysResponse, error) {
	if s.secrets == nil {
		return nil, status.Error(codes.Unimplemented, "API management not configured")
	}
	vs, err := s.secrets.ListVenues(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*operatorpb.VenueKeyStatus, 0, len(vs))
	for _, v := range vs {
		out = append(out, &operatorpb.VenueKeyStatus{Venue: v.Venue, Configured: v.Configured})
	}
	return &operatorpb.ListVenueKeysResponse{Venues: out}, nil
}

// TestConnection is the pre-flight reachability check the Add Node form runs before
// anything is provisioned. reachable=false is a normal result, not an RPC error — the
// point of a pre-flight check is to report a bad target, not to fail.
//
// THE OPERATOR DOES NOT DIAL. It asks the provisioner to run a one-shot Job that dials
// and reports back (provision.Probe). The target address comes from the caller, so this
// is an arbitrary-destination TCP connect on someone's behalf; the placement is what
// bounds it. The dialling pod exists for one dial and then dies, and the operator
// Deployment's own NetworkPolicy grants no :22 egress at all — so the estate never holds
// a standing port-22 primitive on a pod that is always running and reachable from
// api-gateway, which is internet-facing.
//
// An earlier version of this comment justified dialling from the operator on the grounds
// that the caller reached it "only via a kubeconfig-gated port-forward". OPS-M2a made
// that false — the RPC arrives over mTLS from api-gateway — and the stale premise is
// exactly what made the surface look acceptable for as long as it did.
//
// What crosses back is reachability, latency, and a short trimmed reason. Never a byte
// read from the peer: the probe does not read the socket at all.
func (s *Server) TestConnection(ctx context.Context, req *operatorpb.TestConnectionRequest) (*operatorpb.TestConnectionResponse, error) {
	// Same gate as AddNode: no provisioner image, no Job to run, so there is nothing to
	// probe with. Reporting every target unreachable would be a lie about the target.
	if s.prov == nil {
		return nil, status.Error(codes.Unimplemented, "provisioning not configured")
	}
	if req.GetIp() == "" {
		return nil, status.Error(codes.InvalidArgument, "ip is required")
	}
	port := req.GetSshPort()
	if port == 0 {
		port = 22
	}
	// Refuse a port the cluster cannot probe HERE, at the boundary, so it is reported as
	// the caller's input error it is. node-provisioner-egress pins TCP:22, so a probe of
	// any other port is dropped by policy and would come back looking like a host that is
	// down. Probe() re-checks this as defence in depth, but a refusal that only surfaces
	// from there arrives as Internal — telling an operator the platform broke when what
	// actually happened is that they typed a port this estate cannot reach.
	if port != 22 {
		return nil, status.Errorf(codes.InvalidArgument,
			"cannot probe port %d: only port 22 can be probed, because cluster egress policy "+
				"pins SSH to :22; provisioning has the same constraint", port)
	}
	res, err := s.prov.Probe(ctx, req.GetIp(), port)
	if err != nil {
		// The probe could not be RUN — a different fact from an unreachable host, and one
		// the caller must not see as reachable=false.
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &operatorpb.TestConnectionResponse{
		Reachable: res.Reachable, LatencyMs: res.LatencyMS, Message: res.Message,
	}, nil
}

func provStatus(s provision.Status) operatorpb.ProvisionStatus {
	switch s {
	case provision.StatusInstalling:
		return operatorpb.ProvisionStatus_PROVISION_STATUS_INSTALLING
	case provision.StatusJoined:
		return operatorpb.ProvisionStatus_PROVISION_STATUS_JOINED
	case provision.StatusFailed:
		return operatorpb.ProvisionStatus_PROVISION_STATUS_FAILED
	default:
		return operatorpb.ProvisionStatus_PROVISION_STATUS_PENDING
	}
}

func protoStatus(s estate.NodeStatus) operatorpb.NodeStatus {
	switch s {
	case estate.StatusReady:
		return operatorpb.NodeStatus_NODE_STATUS_READY
	case estate.StatusNotReady:
		return operatorpb.NodeStatus_NODE_STATUS_NOT_READY
	default:
		return operatorpb.NodeStatus_NODE_STATUS_UNSPECIFIED
	}
}

func nonZeroTimestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// compile-time assertion the adapter satisfies the generated interface.
var _ operatorpb.OperatorServiceServer = (*Server)(nil)
