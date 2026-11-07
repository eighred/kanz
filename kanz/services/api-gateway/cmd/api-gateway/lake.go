package main

import (
	"context"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"google.golang.org/grpc"
)

// bindLake owns both connection and rotating workload source; neither identity
// nor backend address can be selected by an HTTP caller.
func bindLake(ctx context.Context, cfg config.Config, handler *gateway.Handler) (func(), error) {
	if cfg.LakeSnapshot == nil {
		return func() {}, nil
	}
	source, err := transport.NewSource(ctx, cfg.SPIFFESocket)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.LakeSnapshot.Address,
		transport.ClientDialOption(source, transport.AuthorizeServices(cfg.LakeSnapshot.Peer)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<20), grpc.MaxCallSendMsgSize(64<<10)))
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	closeOwned := func() { _ = conn.Close(); _ = source.Close() }
	if err := handler.WithLakeSnapshots(lakepb.NewLakeSnapshotServiceClient(conn), cfg.LakeSnapshot.Tenant); err != nil {
		closeOwned()
		return nil, err
	}
	return closeOwned, nil
}
