package transport

// SEC-01e — mTLS handshake + peer-identity assertion + plaintext/unauthorized
// rejection, the integration half SEC-01b/c deferred. No external SPIRE: we
// mint a real in-memory CA + leaf SVIDs (real X.509, real URI-SAN SPIFFE IDs,
// a real TLS handshake over net.Pipe) so the go-spiffe verification + the
// AuthorizeMesh/AuthorizeServices policy are exercised end-to-end on every
// `go test` — not skipped behind a TEST_* gate (the PERS-01e anti-rot rule).

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// staticSource is a Source backed by a fixed SVID + bundle (no Workload API) —
// the test analog of *workloadapi.X509Source.
type staticSource struct {
	svid   *x509svid.SVID
	bundle *x509bundle.Bundle
}

func (s staticSource) GetX509SVID() (*x509svid.SVID, error) { return s.svid, nil }
func (s staticSource) GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return s.bundle, nil
}

// testCA is a self-signed CA standing in for a SPIRE trust domain's authority.
type testCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kanz-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key}
}

// issue mints a leaf X509-SVID for id, signed by this CA. The leaf carries the
// SPIFFE ID as its single URI SAN and the digitalSignature + client/server
// ExtKeyUsage a SPIFFE leaf needs to pass go-spiffe verification both ways.
func (ca *testCA) issue(t *testing.T, id spiffeid.ID) *x509svid.SVID {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{id.URL()},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &x509svid.SVID{ID: id, Certificates: []*x509.Certificate{leaf}, PrivateKey: key}
}

func (ca *testCA) source(t *testing.T, id spiffeid.ID) staticSource {
	return staticSource{
		svid:   ca.issue(t, id),
		bundle: x509bundle.FromX509Authorities(kanzTrustDomain, []*x509.Certificate{ca.cert}),
	}
}

func mustID(t *testing.T, ns, sa string) spiffeid.ID {
	t.Helper()
	id, err := ServiceID(ns, sa)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// handshake drives a full TLS handshake over an in-memory pipe and returns each
// side's outcome. net.Pipe is synchronous, so the two handshakes must run
// concurrently — the server side runs in a goroutine.
func handshake(serverCfg, clientCfg *tls.Config) (sConn, cConn *tls.Conn, sErr, cErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A loopback TCP listener, not net.Pipe: on a rejected handshake the server
	// writes a TLS alert, and net.Pipe (unbuffered) would block that write until
	// the ctx timeout because the peer has stopped reading. TCP's socket buffer
	// lets the alert flush so the rejecting side returns its error at once.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err, err
	}
	defer ln.Close()

	sCh := make(chan *tls.Conn, 1)
	go func() {
		raw, aerr := ln.Accept()
		if aerr != nil {
			sErr = aerr
			sCh <- nil
			return
		}
		s := tls.Server(raw, serverCfg)
		sErr = s.HandshakeContext(ctx)
		sCh <- s
	}()

	raw, derr := (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	if derr != nil {
		return nil, <-sCh, derr, derr
	}
	c := tls.Client(raw, clientCfg)
	cErr = c.HandshakeContext(ctx)
	return <-sCh, c, sErr, cErr
}

// peerID extracts the verified peer's SPIFFE ID from a completed handshake.
func peerID(t *testing.T, conn *tls.Conn) spiffeid.ID {
	t.Helper()
	chain := conn.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		t.Fatal("no peer certificates on the connection")
	}
	id, err := x509svid.IDFromCert(chain[0])
	if err != nil {
		t.Fatalf("IDFromCert: %v", err)
	}
	return id
}

// Two valid in-domain SVIDs complete the handshake AND each side reads back the
// peer's asserted SPIFFE identity — the property the mesh relies on.
func TestMTLS_HandshakeAssertsPeerIdentity(t *testing.T) {
	ca := newCA(t)
	serverID := mustID(t, "kanz-services", "risk-engine")
	clientID := mustID(t, "kanz-services", "api-gateway")

	srv := ServerTLSConfig(ca.source(t, serverID), AuthorizeMesh())
	cli := ClientTLSConfig(ca.source(t, clientID), AuthorizeMesh())

	sConn, cConn, sErr, cErr := handshake(srv, cli)
	if sErr != nil || cErr != nil {
		t.Fatalf("handshake failed: server=%v client=%v", sErr, cErr)
	}
	if got := peerID(t, sConn); got.String() != clientID.String() {
		t.Errorf("server saw peer %q, want %q", got, clientID)
	}
	if got := peerID(t, cConn); got.String() != serverID.String() {
		t.Errorf("client saw peer %q, want %q", got, serverID)
	}
}

// An unauthenticated client — a TLS client that presents NO certificate — is
// rejected by the mutual-TLS server (RequireAnyClientCert).
func TestMTLS_UnauthenticatedClientRejected(t *testing.T) {
	ca := newCA(t)
	srv := ServerTLSConfig(ca.source(t, mustID(t, "kanz-services", "risk-engine")), AuthorizeMesh())
	// No client SVID, just skip server verification — the server must still
	// reject for lack of a client cert.
	cli := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // deliberate: testing server-side rejection

	_, _, sErr, _ := handshake(srv, cli)
	if sErr == nil {
		t.Fatal("server accepted a client with no certificate; mTLS not enforced")
	}
}

// Raw plaintext (non-TLS) bytes sent to the mTLS server fail the handshake —
// the listener never downgrades to cleartext.
func TestMTLS_PlaintextClientRejected(t *testing.T) {
	ca := newCA(t)
	srv := ServerTLSConfig(ca.source(t, mustID(t, "kanz-services", "risk-engine")), AuthorizeMesh())

	sPipe, cPipe := net.Pipe()
	s := tls.Server(sPipe, srv)
	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		errCh <- s.HandshakeContext(ctx)
	}()

	// Speak cleartext at a TLS server.
	_, _ = cPipe.Write([]byte("GET / HTTP/1.1\r\nHost: risk-engine\r\n\r\n"))
	if err := <-errCh; err == nil {
		t.Fatal("server completed a handshake against plaintext input")
	}
	s.Close()
	cPipe.Close()
}

// A peer with a valid in-domain SVID that is NOT on the server's allow-list is
// rejected — authentication is necessary but not sufficient (deny-by-default
// AuthorizeServices, the seam AUTH-01 / the API gateway lean on).
func TestMTLS_UnauthorizedClientRejected(t *testing.T) {
	ca := newCA(t)
	serverID := mustID(t, "kanz-services", "risk-engine")
	allowed := mustID(t, "kanz-services", "api-gateway")
	clientID := mustID(t, "kanz-services", "rogue") // valid SVID, not allow-listed

	srv := ServerTLSConfig(ca.source(t, serverID), AuthorizeServices(allowed))
	cli := ClientTLSConfig(ca.source(t, clientID), AuthorizeMesh())

	_, _, sErr, _ := handshake(srv, cli)
	if sErr == nil {
		t.Fatal("server authorized a client outside its allow-list")
	}
}

// A peer whose SVID chains to a DIFFERENT CA than the verifier's bundle is
// rejected — a forged or foreign certificate doesn't authenticate.
func TestMTLS_UntrustedCAClientRejected(t *testing.T) {
	serverCA := newCA(t)
	foreignCA := newCA(t)
	serverID := mustID(t, "kanz-services", "risk-engine")
	clientID := mustID(t, "kanz-services", "api-gateway")

	srv := ServerTLSConfig(serverCA.source(t, serverID), AuthorizeMesh())
	// Client presents a foreign-CA-signed leaf but trusts the server's CA so the
	// failure is unambiguously the server rejecting an unverifiable client.
	cli := ClientTLSConfig(staticSource{
		svid:   foreignCA.issue(t, clientID),
		bundle: x509bundle.FromX509Authorities(kanzTrustDomain, []*x509.Certificate{serverCA.cert}),
	}, AuthorizeMesh())

	_, _, sErr, _ := handshake(srv, cli)
	if sErr == nil {
		t.Fatal("server accepted a client signed by an untrusted CA")
	}
}
