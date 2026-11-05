package main

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/services/tv-sync/internal/config"
	"github.com/eighred/kanz/services/tv-sync/internal/server"
)

// An absent API address is the deliberate compatibility mode. Bind errors in
// split mode never fall back to the shared listener.
func newHTTPServers(cfg config.Config, api *server.Server) []*http.Server {
	if cfg.APIListen == "" {
		return []*http.Server{httpserver.New(cfg.Listen, api.LegacyHandler(), httpserver.Standard())}
	}
	return []*http.Server{
		httpserver.New(cfg.Listen, api.OperationsHandler(), httpserver.Standard()),
		httpserver.New(cfg.APIListen, api, httpserver.Standard()),
	}
}

// Bind every socket before serving or setting readiness. A bad API address must
// not leave an apparently healthy operations listener behind.
func bindHTTPListeners(ctx context.Context, servers []*http.Server) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(servers))
	lc := net.ListenConfig{}
	for _, srv := range servers {
		ln, err := lc.Listen(ctx, "tcp", srv.Addr)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("bind %s: %w", srv.Addr, err)
		}
		listeners = append(listeners, ln)
	}
	return listeners, nil
}
