package main

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/eighred/kanz/services/oms/internal/venuesrv"
)

// THE READ SURFACE (#399 order history, #406 tradeable instruments), IN ITS OWN
// FILE (#643).
//
// It was already a named function returning its product and an error — the shape
// #643 asks for — and it was in main.go, where the file's size is the reason
// nobody looks at any one part of it. Moved here it gains what it never had: a
// test. Three properties below fail SILENTLY and none of them had one.
//
// # Why it is started from runConsumers rather than beside the health server
//
// It needs the STORE, whose lifetime is that function's — serving reads from a
// store closeStores has already closed is the shape of bug the outbox relay's
// join comment describes, one layer up. And it needs the venue CATALOGUE, which
// only exists once every adapter has been dialled and asked: opening the port
// first would serve an EMPTY instrument list during startup, and an empty list is
// indistinguishable from "this deployment trades nothing". A caller cannot tell a
// race from a fact.

// serveOrderQuery starts the order.v1 read surface and returns a graceful stop.
//
// mTLS WHEN THE MESH HAS AN IDENTITY, plaintext otherwise — the same rule the
// risk engine's query surface follows, and for the same reason: a local run has
// no SPIFFE socket and must still be drivable, while a deployed one must not
// serve a portfolio's trading history to an unauthenticated peer.
//
// A bind failure is returned SYNCHRONOUSLY so startup fails loudly. Serving on a
// goroutine and logging the error would leave the pod Ready with no read surface
// — an outage that looks like a routing bug, which is the shape web-bff's static
// root refuses for the same reason.
func serveOrderQuery(cfg config.Config, mesh *transport.Mesh, store order.Store, catalogue []execution.VenueInstrument, logger *slog.Logger) (func(), error) {
	var opts []grpc.ServerOption
	if mesh.Enabled() {
		opts = append(opts, transport.ServerOption(mesh.Source, transport.AuthorizeMesh()))
		logger.Info("order query gRPC: mTLS enabled")
	} else {
		logger.Warn("order query gRPC: serving plaintext (no SPIFFE_ENDPOINT_SOCKET)")
	}

	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		return nil, fmt.Errorf("order query gRPC: listen %s: %w", cfg.GRPCListen, err)
	}
	srv := grpc.NewServer(opts...)
	// cfg.Tenant is the owning tenant of THIS deployment; grpcsrv stamps it on
	// every reply as the deny-by-default gate input. Empty fails closed.
	// THE PENDING QUEUE IS WIRED FROM THE SAME STORE (#410). A held order is in
	// order_proposals and nowhere else, so this route is the only way a person
	// can see one — building the read surface without it would hold orders
	// nobody could find, which is the drop the control exists to end.
	grpcsrv.New(store, store.Proposals(), time.Now, cfg.Tenant).Register(srv)
	venuesrv.New(catalogue, cfg.Tenant).Register(srv)

	go func() {
		logger.Info("order query gRPC listening", "addr", cfg.GRPCListen)
		if err := srv.Serve(lis); err != nil {
			logger.Error("order query gRPC server failed", "err", err)
		}
	}()
	return srv.GracefulStop, nil
}
