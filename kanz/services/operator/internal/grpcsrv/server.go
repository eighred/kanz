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
)

// Server adapts an estate.Reader to the generated OperatorService interface.
type Server struct {
	operatorpb.UnimplementedOperatorServiceServer
	reader estate.Reader
}

// New returns a Server over the given Reader.
func New(r estate.Reader) *Server { return &Server{reader: r} }

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
