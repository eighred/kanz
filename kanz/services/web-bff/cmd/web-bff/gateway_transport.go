package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/web-bff/internal/config"
)

// gatewayTransport binds only the gateway hop to its service SVID. It does not
// turn browser identity headers or a valid workload certificate into a user.
func gatewayTransport(rawURL string, source transport.Source) (*http.Transport, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || source == nil {
		return nil, errors.New("gateway workload transport requires HTTPS and an identity source")
	}
	peer, err := transport.ServiceID("kanz-services", "api-gateway")
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		// This is an internal direct connection, never an ambient HTTP proxy.
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     transport.ClientTLSConfig(source, transport.AuthorizeServices(peer)),
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second, MaxIdleConns: 100, MaxIdleConnsPerHost: 16,
		ForceAttemptHTTP2: true,
	}, nil
}

func openGatewayTransport(ctx context.Context, cfg config.Config) (http.RoundTripper, func(), error) {
	if cfg.GatewaySPIFFESocket == "" {
		return nil, func() {}, nil
	}
	source, err := transport.NewSource(ctx, cfg.GatewaySPIFFESocket)
	if err != nil {
		return nil, nil, err
	}
	tr, err := gatewayTransport(cfg.GatewayURL, source)
	if err != nil {
		_ = source.Close()
		return nil, nil, err
	}
	return tr, func() { tr.CloseIdleConnections(); _ = source.Close() }, nil
}
