//go:build binance

package execution

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// DNS bypass (M3 network mandate): the live execution loop must never block on
// an un-cached DNS lookup against api.binance.com. cachedDialer resolves a host
// at most once per TTL and dials the cached IP directly, so steady-state
// placement/reconciliation calls skip resolution entirely — raw TCP velocity,
// co-located with the matching engine. A cold miss resolves once inline and
// caches; a background refresh keeps hot entries warm.
type cachedDialer struct {
	resolve func(ctx context.Context, host string) ([]string, error)
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]dnsEntry
}

type dnsEntry struct {
	ips     []string
	expires time.Time
}

func newCachedDialer(ttl time.Duration) *cachedDialer {
	base := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	r := &net.Resolver{}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &cachedDialer{
		resolve: func(ctx context.Context, host string) ([]string, error) {
			addrs, err := r.LookupHost(ctx, host)
			return addrs, err
		},
		dial:  base.DialContext,
		ttl:   ttl,
		now:   time.Now,
		cache: map[string]dnsEntry{},
	}
}

// DialContext dials addr, substituting a cached IP for the host so no live call
// pays for a blocking DNS lookup.
func (d *cachedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
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
func (d *cachedDialer) cachedIP(ctx context.Context, host string) (string, error) {
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
	d.cache[host] = dnsEntry{ips: ips, expires: d.now().Add(d.ttl)}
	d.mu.Unlock()
	if len(ips) == 0 {
		return "", nil
	}
	return ips[0], nil
}

// newBinanceHTTPClient builds an http.Client whose transport dials via the
// cached resolver — the client both the REST connector and the websocket
// listenKey calls use.
func newBinanceHTTPClient(dnsTTL time.Duration) *http.Client {
	d := newCachedDialer(dnsTTL)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:         d.DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
}
