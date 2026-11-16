package snapshot

import (
	"context"
	"time"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// NewGRPCServer accepts delegation only from the configured gateway workload.
// The workload authenticates the delegation source; the required typed Caller
// is independently authorized against the fixed tenant on every operation.
// No reflection or other services are registered on this listener.
func NewGRPCServer(source transport.Source, gateway spiffeid.ID, handler *Server) (*grpc.Server, error) {
	if source == nil || gateway.String() == "" || gateway.Path() != "/ns/kanz-services/sa/api-gateway" || handler == nil || handler.store == nil || handler.lifetime == nil {
		return nil, ErrInvalid
	}
	srv := grpc.NewServer(
		transport.ServerOption(source, transport.AuthorizeServices(gateway)),
		grpc.ConnectionTimeout(5*time.Second),
		grpc.WaitForHandlers(true),
		grpc.MaxRecvMsgSize(MaxControlBytes),
		grpc.MaxSendMsgSize(MaxResponseBytes),
		grpc.MaxConcurrentStreams(16),
		grpc.UnaryInterceptor(handler.delegate),
	)
	lakepb.RegisterLakeSnapshotServiceServer(srv, handler)
	return srv, nil
}

func (s *Server) delegate(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	if info == nil {
		return nil, rpcError(ErrInvalid)
	}
	message, ok := req.(proto.Message)
	if !ok || !message.ProtoReflect().IsValid() || len(message.ProtoReflect().GetUnknown()) != 0 {
		return nil, rpcError(ErrInvalid)
	}
	var caller *lakepb.GatewayCaller
	action, id := ActionRead, ""
	switch info.FullMethod {
	case lakepb.LakeSnapshotService_CreateSnapshot_FullMethodName:
		r, ok := req.(*lakepb.CreateSnapshotRequest)
		if !ok || r == nil || r.Input == nil {
			return nil, rpcError(ErrInvalid)
		}
		caller, action, id = r.Caller, ActionCreate, r.Input.RequestId
	case lakepb.LakeSnapshotService_GetSnapshot_FullMethodName:
		r, ok := req.(*lakepb.GetSnapshotRequest)
		if !ok || r == nil {
			return nil, rpcError(ErrInvalid)
		}
		caller, id = r.Caller, r.SnapshotId
	case lakepb.LakeSnapshotService_QuerySnapshot_FullMethodName:
		r, ok := req.(*lakepb.QuerySnapshotRequest)
		if !ok || r == nil {
			return nil, rpcError(ErrInvalid)
		}
		caller, id = r.Caller, r.SnapshotId
	default:
		return nil, rpcError(ErrDenied)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.lifetime == nil || s.lifetime.Err() != nil {
		return nil, rpcError(context.Canceled)
	}
	stop := context.AfterFunc(s.lifetime, cancel)
	defer stop()
	if err := s.store.authorize(ctx, caller, action, id); err != nil {
		return nil, rpcError(err)
	}
	return next(ctx, req)
}
