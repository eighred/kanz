package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

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
