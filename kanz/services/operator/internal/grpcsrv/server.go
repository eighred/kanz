// Package grpcsrv is the operator service's gRPC adapter: a thin translation
// of operator.v1.OperatorService onto the estate.Reader read model. It holds
// no logic — the node listing and grouping live in internal/estate; this layer
// only converts the read model to proto and maps errors to gRPC codes.
package grpcsrv

import (
	"context"
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
