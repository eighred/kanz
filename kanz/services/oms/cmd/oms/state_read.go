package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/grpc"

	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
)

// serveStateRead is default-disabled and never opens an unauthenticated fallback.
// Its sole registered service cannot expose the adjacent approval/catalogue APIs.
func serveStateRead(ctx context.Context, cfg config.Config, mesh *transport.Mesh, store grpcsrv.StateReader, logger *slog.Logger) (func() error, error) {
	if ctx == nil {
		return nil, fmt.Errorf("OMS state read requires an owning lifecycle")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	reader := grpcsrv.NewStateRead(ctx, store, cfg.Tenant, time.Now)
	querypb.RegisterOrderStateReadServiceServer(srv, reader)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := srv.Serve(lis); err != nil {
			logger.Error("OMS state read server failed", "err", err)
		}
	}()
	var once sync.Once
	transportDone := make(chan struct{})
	stop := func() error {
		once.Do(func() {
			reader.CancelReads()
			go func() { defer close(transportDone); srv.Stop() }()
		})
		// One absolute budget covers transport and application joins. Stop
		// runs separately so even a noncooperative transport cannot turn this
		// into an unbounded GracefulStop wait. Repeated calls may finish local
		// teardown after a refused join; they never reopen read admission.
		joinCtx, cancel := context.WithTimeout(context.Background(), stateReadStopBudget)
		defer cancel()
		var stopErr error
		select {
		case <-transportDone:
		case <-joinCtx.Done():
			stopErr = errors.Join(stopErr, errStateReadTransportNotJoined)
		}
		select {
		case <-serveDone:
		case <-joinCtx.Done():
			stopErr = errors.Join(stopErr, errStateReadServeNotJoined)
		}
		stopErr = errors.Join(stopErr, reader.JoinReads(joinCtx))
		return stopErr
	}
	return stop, nil
}

const stateReadStopBudget = 5 * time.Second

var (
	errStateReadTransportNotJoined = errors.New("OMS state read transport did not stop")
	errStateReadServeNotJoined     = errors.New("OMS state read Serve did not join")
)
