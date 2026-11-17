package main

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/grpc"

	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
)

// serveStateRead is default-disabled and never opens an unauthenticated fallback.
// Its sole registered service cannot expose the adjacent approval/catalogue APIs.
func serveStateRead(cfg config.Config, mesh *transport.Mesh, store grpcsrv.StateReader, logger *slog.Logger) (func(), error) {
	if !mesh.Enabled() || cfg.Tenant == "" {
		return nil, fmt.Errorf("OMS state read requires mTLS and an owning tenant")
	}
	peer, err := transport.ServiceID("kanz-services", "mcp")
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", cfg.StateQueryListen)
	if err != nil {
		return nil, fmt.Errorf("OMS state read listen: %w", err)
	}
	srv := grpc.NewServer(transport.ServerOption(mesh.Source, transport.AuthorizeServices(peer)))
	querypb.RegisterOrderStateReadServiceServer(srv, grpcsrv.NewStateRead(store, cfg.Tenant, time.Now))
	go func() {
		if err := srv.Serve(lis); err != nil {
			logger.Error("OMS state read server failed", "err", err)
		}
	}()
	return srv.GracefulStop, nil
}
