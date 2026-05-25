package transport

// Unit tests for the pure helpers (ID construction, authorizer wiring, config
// shape). The mTLS handshake + peer-identity-assertion + plaintext/unauthorized
// rejection integration tests live in mtls_test.go (SEC-01e). Both run on every
// `go test` so the package — and its go-spiffe linkage — can't silently rot,
// the discipline the PERS-01e split established.

import (
	"crypto/tls"
	"errors"
	"testing"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// stubSource satisfies Source without a live Workload API. The config builders
// store it in callbacks and never invoke it at build time, so the error
// returns are never hit by these tests.
type stubSource struct{}

func (stubSource) GetX509SVID() (*x509svid.SVID, error) {
	return nil, errors.New("stub: no svid")
}
func (stubSource) GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return nil, errors.New("stub: no bundle")
}

func TestServiceID_MatchesRegistrationTemplate(t *testing.T) {
	id, err := ServiceID("kanz-services", "risk-engine")
	if err != nil {
		t.Fatalf("ServiceID: %v", err)
	}
	if got, want := id.String(), "spiffe://kanz.internal/ns/kanz-services/sa/risk-engine"; got != want {
		t.Errorf("ServiceID = %q want %q", got, want)
	}
	if id.TrustDomain().String() != TrustDomain {
		t.Errorf("trust domain = %q want %q", id.TrustDomain(), TrustDomain)
	}
}

func TestServiceID_RejectsEmpty(t *testing.T) {
	for _, tc := range []struct{ ns, sa string }{{"", "x"}, {"x", ""}, {"", ""}} {
		if _, err := ServiceID(tc.ns, tc.sa); err == nil {
			t.Errorf("ServiceID(%q,%q) = nil err, want error", tc.ns, tc.sa)
		}
	}
}

// Authorizers must be constructible without panicking and produce distinct,
// non-nil values (AuthorizeServices over our own ID scheme is the deny-by-
// default path AUTH-01 / the gateway lean on).
func TestAuthorizers_Construct(t *testing.T) {
	if AuthorizeMesh() == nil {
		t.Fatal("AuthorizeMesh returned nil")
	}
	id, err := ServiceID("kanz-services", "api-gateway")
	if err != nil {
		t.Fatalf("ServiceID: %v", err)
	}
	if AuthorizeServices(id) == nil {
		t.Fatal("AuthorizeServices returned nil")
	}
}

// ServerTLSConfig requires + verifies the client cert (mutual TLS), TLS 1.2 floor.
func TestServerTLSConfig_RequiresAndVerifiesClient(t *testing.T) {
	cfg := ServerTLSConfig(stubSource{}, AuthorizeMesh())
	if cfg.ClientAuth != tls.RequireAnyClientCert {
		t.Errorf("ClientAuth = %v want RequireAnyClientCert", cfg.ClientAuth)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate not set — peer SVID would go unverified")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x want >= TLS1.2", cfg.MinVersion)
	}
}

// ClientTLSConfig skips DNS hostname checks (SPIFFE identities, not DNS) but
// still verifies the chain via VerifyPeerCertificate.
func TestClientTLSConfig_VerifiesViaSPIFFE(t *testing.T) {
	cfg := ClientTLSConfig(stubSource{}, AuthorizeMesh())
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify=false — go-spiffe needs it true to skip DNS checks")
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate not set — server SVID would go unverified")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x want >= TLS1.2", cfg.MinVersion)
	}
}

// The gRPC convenience wrappers must produce usable, non-nil values.
func TestGRPCWrappers_NonNil(t *testing.T) {
	src := stubSource{}
	if ServerCredentials(src, AuthorizeMesh()) == nil {
		t.Error("ServerCredentials nil")
	}
	if ClientCredentials(src, AuthorizeMesh()) == nil {
		t.Error("ClientCredentials nil")
	}
	if ServerOption(src, AuthorizeMesh()) == nil {
		t.Error("ServerOption nil")
	}
	if ClientDialOption(src, AuthorizeMesh()) == nil {
		t.Error("ClientDialOption nil")
	}
}
