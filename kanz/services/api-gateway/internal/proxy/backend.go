package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// SVCWIRE-01c — the concrete mesh Backend: a per-service HTTP client that
// forwards a proxied read request to its upstream Phase-7 service over the
// SEC-01b mTLS mesh, propagating the gateway-authenticated principal as trusted
// identity headers. The transport (TLS) is injected as an *http.Client so this
// package stays free of the SPIFFE dependency — the composition root
// (cmd/api-gateway) builds the mTLS client from the workload SVID and passes it
// in; tests pass a plain client against an httptest upstream.
//
// Identity propagation: the gateway is the sole identity authority, so it
// forwards the verified principal over the mesh in these headers. The upstream
// trusts them BECAUSE the connection is mutually authenticated to the gateway's
// SVID (a non-mesh caller cannot reach the service). The upstream-side
// middleware that reads these headers back into an auth.Principal is the
// carried-forward half of this seam (one small trust-the-mesh-identity
// middleware per Phase-7 service, at their composition roots).
const (
	HeaderPrincipalSubject = "X-Kanz-Principal-Subject"
	HeaderPrincipalTenant  = "X-Kanz-Principal-Tenant"
	HeaderPrincipalRoles   = "X-Kanz-Principal-Roles" // comma-separated
)

// maxRespBytes bounds an upstream response body the gateway buffers before
// writing it back — generous for these read surfaces, a guard against an
// unbounded upstream.
const maxRespBytes = 8 << 20 // 8 MiB

// MeshBackend forwards to upstream services by base URL over a shared
// (typically mTLS) HTTP client.
type MeshBackend struct {
	bases  map[Service]string
	client *http.Client
}

// NewMeshBackend builds a backend over per-service base URLs (e.g.
// "https://wealth.kanz-services:8080") and an HTTP client. A service absent from
// bases forwards as ErrBackendUnavailable (so a partially-configured gateway
// 503s only the unwired surfaces). A nil client falls back to http.DefaultClient.
func NewMeshBackend(bases map[Service]string, client *http.Client) *MeshBackend {
	if client == nil {
		client = http.DefaultClient
	}
	cleaned := make(map[Service]string, len(bases))
	for svc, base := range bases {
		if b := strings.TrimRight(strings.TrimSpace(base), "/"); b != "" {
			cleaned[svc] = b
		}
	}
	return &MeshBackend{bases: cleaned, client: client}
}

// Configured reports whether any upstream is wired — main uses it to decide
// between a live backend and the nil (all-503) backend.
func (b *MeshBackend) Configured() bool { return len(b.bases) > 0 }

// Forward sends the request to its upstream and returns the reply verbatim. An
// unconfigured service ⇒ ErrBackendUnavailable (→ 503); a transport/timeout
// fault ⇒ the raw error (→ 502). The upstream's own status/body pass through
// unchanged — a 404/403/500 from the service is the client's 404/403/500.
func (b *MeshBackend) Forward(ctx context.Context, req Request) (Response, error) {
	base, ok := b.bases[req.Service]
	if !ok {
		return Response{}, ErrBackendUnavailable
	}
	u, err := url.Parse(base + req.Path)
	if err != nil {
		return Response{}, fmt.Errorf("proxy: bad upstream url: %w", err)
	}
	if len(req.Query) > 0 {
		u.RawQuery = req.Query.Encode()
	}

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, u.String(), body)
	if err != nil {
		return Response{}, err
	}
	if len(req.Body) > 0 {
		hreq.Header.Set("Content-Type", "application/json")
	}
	if p := req.Principal; p != nil {
		hreq.Header.Set(HeaderPrincipalSubject, p.Subject)
		hreq.Header.Set(HeaderPrincipalTenant, p.Tenant)
		if len(p.Roles) > 0 {
			hreq.Header.Set(HeaderPrincipalRoles, strings.Join(p.Roles, ","))
		}
	}

	hresp, err := b.client.Do(hreq)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = hresp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(hresp.Body, maxRespBytes))
	if err != nil {
		return Response{}, err
	}
	return Response{
		Status:      hresp.StatusCode,
		ContentType: hresp.Header.Get("Content-Type"),
		Body:        respBody,
	}, nil
}

var _ Backend = (*MeshBackend)(nil)
