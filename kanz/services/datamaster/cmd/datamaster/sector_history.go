package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/datamaster/internal/config"
	"github.com/eighred/kanz/services/datamaster/internal/server"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

func startSectorHistory(cfg config.Config, golden store.GoldenStore, mesh *transport.Mesh, fail func(error)) (func(), error) {
	if cfg.SectorHistoryListen == "" {
		return nil, nil
	}
	history, ok := golden.(store.SectorHistory)
	if !ok || cfg.Tenant == "" || cfg.Tenant == "__system__" {
		return nil, errors.New("datamaster: sector history requires a durable tenant store")
	}
	host, _, err := net.SplitHostPort(cfg.SectorHistoryListen)
	if err != nil {
		return nil, err
	}
	srv := httpserver.New(cfg.SectorHistoryListen, server.SectorHistoryHandler(cfg.Tenant, history), httpserver.Standard())
	if mesh.Enabled() {
		peer, err := transport.ServiceID("kanz-services", "accounting")
		if err != nil {
			return nil, err
		}
		srv.TLSConfig = transport.ServerTLSConfig(mesh.Source, transport.AuthorizeServices(peer))
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("datamaster: plaintext sector history requires literal local loopback")
	}
	listener, err := net.Listen("tcp", cfg.SectorHistoryListen)
	if err != nil {
		return nil, err
	}
	go func() {
		var err error
		if srv.TLSConfig == nil {
			err = srv.Serve(listener)
		} else {
			err = srv.ServeTLS(listener, "", "")
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fail(err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}
