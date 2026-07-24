// Package grpcsrv is the operator service's gRPC adapter: a thin translation
// of operator.v1.OperatorService onto the estate.Reader read model. It holds
// no logic — the node listing and grouping live in internal/estate; this layer
// only converts the read model to proto and maps errors to gRPC codes.
package grpcsrv

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"

	"github.com/kanz-eng/kanz/services/operator/internal/estate"
	"github.com/kanz-eng/kanz/services/operator/internal/provision"
)

// Provisioner is the node-provisioning surface the operator gRPC depends on
// (provision.Provisioner satisfies it). Kept as an interface so the handler is
// unit-tested against a stub with no Kubernetes client.
type Provisioner interface {
	AddNode(ctx context.Context, r provision.Request) (string, error)
	List(ctx context.Context) ([]provision.Provision, error)
}

// Server adapts an estate.Reader to the generated OperatorService interface.
type Server struct {
	operatorpb.UnimplementedOperatorServiceServer
	reader estate.Reader
	prov   Provisioner
}

// New returns a read-only Server (no provisioning). Retained for callers/tests that
// only exercise ListNodes/ListClusters.
func New(r estate.Reader) *Server { return &Server{reader: r} }

// NewWithProvisioner returns a Server with the write path wired.
func NewWithProvisioner(r estate.Reader, p Provisioner) *Server {
	return &Server{reader: r, prov: p}
}

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

// testDialTimeout bounds the reachability probe.
const testDialTimeout = 5 * time.Second

// TestConnection is a pre-flight TCP reachability probe. It is a plain net.Dial —
// deliberately NOT crypto/ssh — so the operator never imports the SSH plane; the
// key is authenticated at provision time, not here. reachable=false is a normal
// result, not an RPC error.
//
// The caller (root@universe, reaching the operator only via a kubeconfig-gated
// port-forward) can make the operator dial an arbitrary ip:port. Accepted: the
// caller could already reach anything a cluster operator can, the probe returns
// only reachable/latency (never response bytes), and the dial is timeout-bounded.
func (s *Server) TestConnection(ctx context.Context, req *operatorpb.TestConnectionRequest) (*operatorpb.TestConnectionResponse, error) {
	if req.GetIp() == "" {
		return nil, status.Error(codes.InvalidArgument, "ip is required")
	}
	port := req.GetSshPort()
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(req.GetIp(), strconv.Itoa(int(port)))

	start := time.Now()
	d := net.Dialer{Timeout: testDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return &operatorpb.TestConnectionResponse{Reachable: false, Message: dialMessage(err)}, nil
	}
	_ = conn.Close()
	return &operatorpb.TestConnectionResponse{Reachable: true, LatencyMs: time.Since(start).Milliseconds()}, nil
}

// dialMessage trims a dial error to a short, client-safe reason.
func dialMessage(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		return msg[i+2:]
	}
	return msg
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
