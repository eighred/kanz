package clientip_test

// THE RATE LIMITER'S KEY IS ONLY AS HONEST AS THIS (#371).
//
// Under the zero-ingress model nothing listens publicly and cloudflared is the
// only peer that can reach the BFF, so CF-Connecting-IP is trustworthy. That is
// a DEPLOYMENT property. These tests pin the code property: the header is
// honoured only from a trusted peer, and never otherwise — so a BFF that is one
// day reachable directly does not silently become an unbounded credential-
// guessing oracle.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eighred/kanz/internal/clientip"
)

func req(t *testing.T, remoteAddr, header, value string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	r.RemoteAddr = remoteAddr
	if header != "" {
		r.Header.Set(header, value)
	}
	return r
}

func TestAnUntrustedPeerCannotChooseItsOwnAddress(t *testing.T) {
	r, err := clientip.NewResolver("CF-Connecting-IP", []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	// The request arrives DIRECTLY (not via the tunnel) claiming to be someone else.
	got := r.Resolve(req(t, "203.0.113.9:5555", "CF-Connecting-IP", "198.51.100.1"))
	if got != "203.0.113.9" {
		t.Fatalf("resolved %q, want the peer 203.0.113.9.\n\n"+
			"An untrusted caller set its own address. The login limiter keys on this, so it "+
			"could send a different value per request and guess credentials without bound — "+
			"while the metrics show a broad spread of well-behaved clients.", got)
	}
}

func TestATrustedPeersHeaderIsHonoured(t *testing.T) {
	r, _ := clientip.NewResolver("CF-Connecting-IP", []string{"127.0.0.1"})
	got := r.Resolve(req(t, "127.0.0.1:44321", "CF-Connecting-IP", "198.51.100.1"))
	if got != "198.51.100.1" {
		t.Fatalf("resolved %q, want 198.51.100.1 — behind the tunnel EVERY request has the "+
			"same peer, so without the header one user's failures throttle everybody", got)
	}
}

// NO CONFIGURATION MEANS NO TRUST — the safe default, not an open one.
func TestWithNoTrustConfiguredTheHeaderIsIgnored(t *testing.T) {
	for name, mk := range map[string]func() (*clientip.Resolver, error){
		"no header":  func() (*clientip.Resolver, error) { return clientip.NewResolver("", []string{"127.0.0.1"}) },
		"no cidrs":   func() (*clientip.Resolver, error) { return clientip.NewResolver("CF-Connecting-IP", nil) },
		"empty cidr": func() (*clientip.Resolver, error) { return clientip.NewResolver("CF-Connecting-IP", []string{" "}) },
	} {
		res, err := mk()
		if err != nil {
			t.Fatalf("%s: NewResolver: %v", name, err)
		}
		if res.Trusts() {
			t.Errorf("%s: Trusts() = true with nothing configured", name)
		}
		if got := res.Resolve(req(t, "127.0.0.1:1", "CF-Connecting-IP", "198.51.100.1")); got != "127.0.0.1" {
			t.Errorf("%s: resolved %q, want the peer — an unconfigured resolver must not honour "+
				"a header, or forgetting to configure it opens the oracle", name, got)
		}
	}
}

// A CHAIN HEADER ATTRIBUTES THE ORIGINAL CLIENT, not the nearest hop.
func TestAChainHeaderTakesTheFirstEntry(t *testing.T) {
	r, _ := clientip.NewResolver("X-Forwarded-For", []string{"10.0.0.0/8"})
	got := r.Resolve(req(t, "10.1.2.3:9", "X-Forwarded-For", "198.51.100.1, 10.1.2.3"))
	if got != "198.51.100.1" {
		t.Fatalf("resolved %q, want the first entry — taking the last attributes every request "+
			"to the edge itself, and the limiter becomes global", got)
	}
}

// A TRUSTED PROXY SENDING NONSENSE still must not produce nonsense.
func TestAnUnparseableHeaderFallsBackToThePeer(t *testing.T) {
	r, _ := clientip.NewResolver("CF-Connecting-IP", []string{"127.0.0.1"})
	for _, v := range []string{"not-an-ip", "", "   ", "1.2.3.4.5"} {
		if got := r.Resolve(req(t, "127.0.0.1:1", "CF-Connecting-IP", v)); got != "127.0.0.1" {
			t.Errorf("value %q resolved to %q, want the peer", v, got)
		}
	}
}

func TestABareAddressIsAcceptedAsATrustedProxy(t *testing.T) {
	if _, err := clientip.NewResolver("CF-Connecting-IP", []string{"::1"}); err != nil {
		t.Fatalf("a bare IPv6 address was rejected: %v — an operator naming one sidecar should "+
			"not have to know CIDR notation", err)
	}
	if _, err := clientip.NewResolver("CF-Connecting-IP", []string{"nonsense"}); err == nil {
		t.Fatal("an unparseable trusted proxy was accepted — it would silently trust nothing, " +
			"which reads as configured")
	}
}
