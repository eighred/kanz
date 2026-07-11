// Package netdial is the exchange network transport shared by every process that
// talks to an exchange on the live path — the OMS's REST/user-data connectors and
// market-ingest's depth-feed websockets.
//
// DNS bypass (the M3 network mandate): the live path must never block on an
// un-cached DNS lookup against api.binance.com / ws.okx.com. CachedDialer
// resolves a host at most once per TTL and dials the cached IP directly, so
// steady-state placement, reconciliation, and depth frames skip resolution
// entirely — raw TCP velocity, co-located with the matching engine. A cold miss
// resolves once inline and caches; a resolver failure serves the stale entry
// rather than failing a live call.
//
// It is deliberately UNTAGGED and stdlib-only (no vendor import), so promoting it
// out of the OMS does not put a vendor dependency into the default binary: the
// exchange websocket/SDK code stays behind its per-venue build tags.
package netdial

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// CachedDialer dials with a TTL-cached DNS resolution.
type CachedDialer struct {
	resolve func(ctx context.Context, host string) ([]string, error)
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	ips     []string
	expires time.Time
}

// NewCachedDialer builds a dialer caching resolutions for ttl (<=0 ⇒ 5m).
func NewCachedDialer(ttl time.Duration) *CachedDialer {
	base := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	r := &net.Resolver{}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &CachedDialer{
		resolve: func(ctx context.Context, host string) ([]string, error) {
			return r.LookupHost(ctx, host)
		},
		dial:  base.DialContext,
		ttl:   ttl,
		now:   time.Now,
		cache: map[string]entry{},
	}
}

// DialContext dials addr, substituting a cached IP for the host so no live call
// pays for a blocking DNS lookup.
func (d *CachedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return d.dial(ctx, network, addr)
	}
	// Already an IP literal — nothing to resolve.
	if net.ParseIP(host) != nil {
		return d.dial(ctx, network, addr)
	}
	ip, err := d.cachedIP(ctx, host)
	if err != nil || ip == "" {
		return d.dial(ctx, network, addr) // fall back to the resolver's own dial
	}
	return d.dial(ctx, network, net.JoinHostPort(ip, port))
}

// cachedIP returns a resolved IP for host, resolving (once) only on a cold or
// expired entry.
func (d *CachedDialer) cachedIP(ctx context.Context, host string) (string, error) {
	d.mu.Lock()
	e, ok := d.cache[host]
	fresh := ok && d.now().Before(e.expires) && len(e.ips) > 0
	d.mu.Unlock()
	if fresh {
		return e.ips[0], nil
	}
	ips, err := d.resolve(ctx, host)
	if err != nil {
		// Serve a stale entry rather than fail the live call, if we have one.
		if ok && len(e.ips) > 0 {
			return e.ips[0], nil
		}
		return "", err
	}
	d.mu.Lock()
	d.cache[host] = entry{ips: ips, expires: d.now().Add(d.ttl)}
	d.mu.Unlock()
	if len(ips) == 0 {
		return "", nil
	}
	return ips[0], nil
}

// NewHTTPClient builds an http.Client whose transport dials via the cached
// resolver — the client the REST connectors use. Its 10s Timeout bounds a
// request/response call.
func NewHTTPClient(dnsTTL time.Duration) *http.Client {
	c := newClient(dnsTTL)
	c.Timeout = 10 * time.Second
	return c
}

// NewWebsocketHTTPClient builds the DNS-bypass client for a websocket handshake
// (coder/websocket DialOptions.HTTPClient), so the depth stream resolves through
// the same cache as the rest of the live loop.
//
// Its Timeout is deliberately ZERO: a websocket is long-lived, and coder/websocket
// rejects a client with a non-zero Timeout outright — that timeout would apply to
// the whole connection, not the handshake, and kill the stream. Cancellation is
// the caller's ctx, never a client deadline.
func NewWebsocketHTTPClient(dnsTTL time.Duration) *http.Client {
	return newClient(dnsTTL) // Timeout stays 0
}

func newClient(dnsTTL time.Duration) *http.Client {
	d := NewCachedDialer(dnsTTL)
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         d.DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
}
