package main

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
)

// The secure endpoint runs the same authentication/signing/authorization router.
// A BFF workload certificate authorizes the connection, never a trading action.
func newBFFTLSServer(addr string, handler http.Handler, bounds httpserver.Timeouts, source transport.Source) (*http.Server, error) {
	if source == nil || handler == nil {
		return nil, errors.New("BFF TLS listener requires workload identity and the gateway router")
	}
	peer, err := transport.ServiceID("kanz-services", "web-bff")
	if err != nil {
		return nil, err
	}
	server := httpserver.New(addr, handler, bounds)
	server.TLSConfig = transport.ServerTLSConfig(source, transport.AuthorizeServices(peer))
	return server, nil
}

type bffTLSListener struct {
	server      *http.Server
	listener    net.Listener
	closeSource func()
}

func prepareBFFTLS(ctx context.Context, cfg config.Config, handler http.Handler, bounds httpserver.Timeouts) (*bffTLSListener, error) {
	if cfg.BFFTLSListen == "" {
		return nil, nil
	}
	if cfg.SPIFFESocket == "" {
		return nil, errors.New("BFF TLS listener requires SPIFFE socket")
	}
	source, err := transport.NewSource(ctx, cfg.SPIFFESocket)
	if err != nil {
		return nil, err
	}
	server, err := newBFFTLSServer(cfg.BFFTLSListen, handler, bounds, source)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.BFFTLSListen)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	return &bffTLSListener{server: server, listener: listener, closeSource: func() { _ = source.Close() }}, nil
}

func (p *bffTLSListener) close() {
	if p != nil {
		_ = p.listener.Close()
		p.closeSource()
	}
}
