package main

import (
	"context"
	"fmt"
	"log/slog"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/grpc"

	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/mcp/internal/config"
	"github.com/eighred/kanz/services/mcp/internal/riskread"
	"github.com/eighred/kanz/services/mcp/internal/server"
	"github.com/eighred/kanz/services/mcp/internal/stateread"
)

func buildReaders(ctx context.Context, cfg config.Config, logger *slog.Logger) (*riskread.Client, server.StateReader, func(), error) {
	if !cfg.StateReads {
		conn, err := dialRiskQuery(ctx, cfg, logger)
		if err != nil {
			return nil, nil, nil, err
		}
		return riskread.New(querypb.NewRiskQueryServiceClient(conn)), nil, func() { _ = conn.Close() }, nil
	}
	if cfg.Tenant == "" || cfg.RiskQueryAddr == "" || cfg.OrderQueryAddr == "" || cfg.SPIFFESocket == "" {
		return nil, nil, nil, fmt.Errorf("state reads require complete authenticated domain configuration")
	}
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return nil, nil, nil, err
	}
	riskID, err := transport.ServiceID("kanz-services", "risk-engine")
	if err != nil {
		_ = mesh.Close()
		return nil, nil, nil, err
	}
	omsID, err := transport.ServiceID("kanz-services", "oms")
	if err != nil {
		_ = mesh.Close()
		return nil, nil, nil, err
	}
	riskConn, err := grpc.NewClient(cfg.RiskQueryAddr, transport.ClientDialOption(mesh.Source, transport.AuthorizeServices(riskID)))
	if err != nil {
		_ = mesh.Close()
		return nil, nil, nil, err
	}
	orderConn, err := grpc.NewClient(cfg.OrderQueryAddr, transport.ClientDialOption(mesh.Source, transport.AuthorizeServices(omsID)))
	if err != nil {
		_ = riskConn.Close()
		_ = mesh.Close()
		return nil, nil, nil, err
	}
	closeReaders := func() { _ = orderConn.Close(); _ = riskConn.Close(); _ = mesh.Close() }
	stateReader, err := stateread.New(querypb.NewPositionQueryServiceClient(riskConn), querypb.NewOrderStateReadServiceClient(orderConn), cfg.Tenant)
	if err != nil {
		closeReaders()
		return nil, nil, nil, err
	}
	return riskread.NewScoped(querypb.NewRiskQueryServiceClient(riskConn), cfg.Tenant), stateReader, closeReaders, nil
}
