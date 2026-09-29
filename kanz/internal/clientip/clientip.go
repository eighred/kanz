// Package clientip resolves the real caller's address behind a trusted edge
// (#371).
//
// WHY THIS IS NOT A ONE-LINER, AND WHY IT FAILS CLOSED.
//
// A pre-authentication limiter keys on the caller's address. If that address
// comes from a header the caller can set, the limiter is not a limiter: an
// attacker sends a different CF-Connecting-IP on every request and guesses
// credentials without bound, while the metrics show a wide spread of
// well-behaved clients. The bug is invisible precisely because it looks like
// normal traffic.
//
// A forwarded header is trustworthy only when the connection carrying it could
// not have come from anywhere else. Under the zero-ingress model that condition
// holds — cloudflared holds an OUTBOUND tunnel and nothing listens publicly, so
// the only peer that can reach the BFF is the tunnel daemon itself. But "holds
// today" is a deployment property, not a code property, and a process that
// trusts the header unconditionally is one `docker run -p` away from being an
// open oracle.
//
// So the header is honoured ONLY when the immediate peer is one of the
// configured trusted proxies. Configure neither and the peer address is used,
// which is correct for direct local development and safe everywhere else.
//
// # WHY IT LIVES IN internal/ RATHER THAN IN ONE SERVICE (#835)
//
// It was services/web-bff/internal/clientip while the BFF's credential login
// was the only pre-auth surface that needed it. The api-gateway's pre-auth
// limiter (middleware.PreAuth) is the second consumer, and Go's internal rule
// makes that promotion mandatory rather than stylistic: a package under
// services/web-bff/internal is importable only from services/web-bff, so the
// alternative to moving it is a second copy — and a copied helper is how a fix
// stops spreading (AGENTS.md). The two callers face the same question with two
// different edges in front of them (cloudflared for the BFF, ingress-nginx for
// the gateway), which is exactly why the trusted set is configuration and not a
// constant in here.
package clientip

import (
	"net"
	"net/http"
	"strings"
)

// Resolver turns a request into the address rate limits and audit lines should
// attribute it to.
type Resolver struct {
	header  string
	trusted []*net.IPNet
}

// NewResolver builds a resolver.
//
// header is the forwarded-for header to honour (e.g. "CF-Connecting-IP"), and
// trustedCIDRs are the peers permitted to set it. An empty header, or an empty
// trust list, disables header handling entirely — the peer address is then the
// answer. That default is deliberate: a misconfiguration must cost accuracy
// (every caller behind the edge looks like the edge, and the limiter is merely
// too strict) rather than correctness (any caller can claim any address, and the
// limiter stops existing).
func NewResolver(header string, trustedCIDRs []string) (*Resolver, error) {
	r := &Resolver{header: http.CanonicalHeaderKey(strings.TrimSpace(header))}
	for _, c := range trustedCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		// A bare address is accepted as a /32 or /128 so an operator can name a
		// single sidecar without knowing CIDR notation.
		if !strings.Contains(c, "/") {
			if ip := net.ParseIP(c); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				r.trusted = append(r.trusted, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
				continue
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		r.trusted = append(r.trusted, n)
	}
	return r, nil
}

// Trusts reports whether the header will ever be honoured. Used by the
// composition root to say so out loud at startup — "configured and ignored" and
// "not configured" must not look the same in a log.
// A NIL RESOLVER TRUSTS NOTHING rather than panicking. It is the zero value a
// composition root that configured no edge would hand a limiter, and the safe
// reading of "no resolver" is "no header is honoured" — the same answer an
// unconfigured one gives.
func (r *Resolver) Trusts() bool { return r != nil && r.header != "" && len(r.trusted) > 0 }

// Resolve returns the address to attribute this request to.
//
// It returns the PEER address unless the peer is trusted AND the header is
// present AND its value parses as an IP. Each of those three is a way for a
// spoofed value to become the answer, so each is checked rather than assumed.
func (r *Resolver) Resolve(req *http.Request) string {
	peer := peerIP(req.RemoteAddr)
	if !r.Trusts() || !r.trustedPeer(peer) {
		return peer
	}
	v := strings.TrimSpace(req.Header.Get(r.header))
	if v == "" {
		return peer
	}
	// Take the FIRST entry: a chain-style header (X-Forwarded-For) lists the
	// original client first and every proxy after it. Taking the last would
	// attribute the request to the nearest hop, which is the edge itself.
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	if net.ParseIP(v) == nil {
		// A trusted proxy sending garbage is a misconfiguration, not an attack,
		// but the answer still must not be the garbage.
		return peer
	}
	return v
}

func (r *Resolver) trustedPeer(peer string) bool {
	ip := net.ParseIP(peer)
	if ip == nil {
		return false
	}
	for _, n := range r.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// peerIP strips the port from a RemoteAddr, tolerating an address without one.
func peerIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}
