package transport

// Unit tests for the pure helpers (ID construction, authorizer wiring, config
// shape). The mTLS handshake + peer-identity-assertion + plaintext/unauthorized
// rejection integration tests live in mtls_test.go (SEC-01e). Both run on every
// `go test` so the package — and its go-spiffe linkage — can't silently rot,
// the discipline the PERS-01e split established.

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"
	"time"

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

// TestNewSourceDoesNotHangWithoutAnAgent pins EXEC-M10.
//
// NewSource used to block on the caller's context — the service's ROOT context,
// cancelled only by SIGTERM. A workload whose SPIRE agent was not there therefore
// stopped dead inside its own startup: no error, no log, no crash, no restart.
// Running, 0/1 Ready, forever. Found by deploying the OMS into a cluster with no
// SPIRE agent: it emitted one line ("oms listening") and then nothing at all.
//
// A missing agent must be a LOUD failure — an error naming the socket, a crash, a
// kubelet restart with backoff — never a silent hang. A pod that crash-loops with a
// clear reason is strictly better than a pod that hangs with none.
func TestNewSourceDoesNotHangWithoutAnAgent(t *testing.T) {
	// A socket path nothing is listening on: exactly a pod with no SPIRE agent.
	socket := "unix:///run/spiffe/definitely-not-here.sock"

	// The bound is InitialSVIDTimeout (30s), but WithTimeout honours an earlier
	// parent deadline — so a caller in a hurry gets one, and this test is fast.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := NewSource(ctx, socket)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("NewSource returned a Source with no agent listening")
		}
		if !strings.Contains(err.Error(), socket) {
			t.Errorf("the error must name the socket an operator has to go and look at; got: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("NewSource HUNG with no SPIRE agent. The service would sit Running and never Ready, " +
			"with no error and no log line, until somebody deleted the pod.")
	}
}

// ParseServiceIDs is the shared allow-list parser behind AuthorizeServices, and
// these tests ARE its enforcement (#venue-authz).
//
// Every arm below corresponds to a way an allow-list can be wrong in the safe-
// looking direction. The dangerous one is the empty result: AuthorizeServices()
// with no ids is a server nobody can call, which presents as "the caller is
// broken" and whose reflex fix is to drop back to AuthorizeMesh — admitting
// every workload in the trust domain. So an empty list must fail at STARTUP,
// naming the variable, not quietly at every handshake.
//
// The operator's control plane has carried the same six assertions since it
// shipped; consolidating the parser moved the argument here rather than making
// a second copy of it.

func TestParseServiceIDs_RefusesAnEmptyList(t *testing.T) {
	if _, err := ParseServiceIDs("VENUE_OKX_ALLOWED_CLIENTS", ""); err == nil {
		t.Fatal("an empty allow-list was accepted. AuthorizeServices with no ids admits nobody, " +
			"and the reflex repair for that is AuthorizeMesh, which admits everybody")
	} else if !strings.Contains(err.Error(), "VENUE_OKX_ALLOWED_CLIENTS") {
		t.Errorf("the error must name the variable an operator has to set; got: %v", err)
	}
}

// A list of only separators is the same defect wearing a different spelling, and
// it is why the length check lives after the loop rather than before it.
func TestParseServiceIDs_RefusesAListOfOnlySeparators(t *testing.T) {
	if _, err := ParseServiceIDs("VENUE_OKX_ALLOWED_CLIENTS", " , , "); err == nil {
		t.Fatal(`" , , " produced a non-empty allow-list`)
	}
}

// A typo must be fatal rather than skipped. Skipping it silently shrinks the
// allow-list, and the result is a server refusing the caller it was configured
// to admit — diagnosable only from the peer's side.
func TestParseServiceIDs_RejectsAMalformedID(t *testing.T) {
	_, err := ParseServiceIDs("VENUE_OKX_ALLOWED_CLIENTS",
		"spiffe://kanz.internal/ns/kanz-services/sa/oms,not-a-spiffe-id")
	if err == nil {
		t.Fatal("a malformed entry was skipped rather than refused")
	}
	if !strings.Contains(err.Error(), "not-a-spiffe-id") {
		t.Errorf("the error must quote the offending entry so it can be found in a manifest; got: %v", err)
	}
}

func TestParseServiceIDs_ParsesAndTrims(t *testing.T) {
	ids, err := ParseServiceIDs("VENUE_OKX_ALLOWED_CLIENTS",
		"  spiffe://kanz.internal/ns/kanz-services/sa/oms , spiffe://kanz.internal/ns/kanz-services/sa/api-gateway ")
	if err != nil {
		t.Fatalf("ParseServiceIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("parsed %d ids, want 2", len(ids))
	}
	if got := ids[0].String(); got != "spiffe://kanz.internal/ns/kanz-services/sa/oms" {
		t.Errorf("ids[0] = %q — surrounding whitespace in a manifest must not change the identity", got)
	}
}

// A trailing comma is a formatting slip, not a request to admit "".
func TestParseServiceIDs_IgnoresEmptyEntries(t *testing.T) {
	ids, err := ParseServiceIDs("VENUE_OKX_ALLOWED_CLIENTS",
		"spiffe://kanz.internal/ns/kanz-services/sa/oms,")
	if err != nil {
		t.Fatalf("ParseServiceIDs: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("parsed %d ids, want 1", len(ids))
	}
}

// The parsed list must actually reach an authorizer that refuses a stranger.
// Without this the tests above only prove a string was split.
func TestParseServiceIDs_FeedsAnAuthorizerThatRefusesAStranger(t *testing.T) {
	ids, err := ParseServiceIDs("VENUE_OKX_ALLOWED_CLIENTS", "spiffe://kanz.internal/ns/kanz-services/sa/oms")
	if err != nil {
		t.Fatalf("ParseServiceIDs: %v", err)
	}
	ca := newCA(t)
	serverID := mustID(t, "kanz-services", "venue-okx")
	// A VALID, CORRECTLY-SIGNED, IN-TRUST-DOMAIN SVID that is simply not the OMS.
	// Under AuthorizeMesh this peer was admitted and could submit orders to a
	// live exchange.
	rogue := mustID(t, "kanz-services", "copilot")

	srv := ServerTLSConfig(ca.source(t, serverID), AuthorizeServices(ids...))
	cli := ClientTLSConfig(ca.source(t, rogue), AuthorizeMesh())

	_, _, sErr, _ := handshake(srv, cli)
	if sErr == nil {
		t.Fatal("the venue allow-list admitted a workload that is not the OMS. Its SVID is valid " +
			"and in the trust domain — that is what every workload has — so authenticating it " +
			"proves nothing about whether it may trade")
	}
}

// And it must ADMIT the OMS, or the change is a trading outage rather than a
// control. Without this arm the test above passes against an allow-list that
// refuses everyone.
func TestParseServiceIDs_FeedsAnAuthorizerThatAdmitsTheOMS(t *testing.T) {
	ids, err := ParseServiceIDs("VENUE_OKX_ALLOWED_CLIENTS", "spiffe://kanz.internal/ns/kanz-services/sa/oms")
	if err != nil {
		t.Fatalf("ParseServiceIDs: %v", err)
	}
	ca := newCA(t)
	srv := ServerTLSConfig(ca.source(t, mustID(t, "kanz-services", "venue-okx")), AuthorizeServices(ids...))
	cli := ClientTLSConfig(ca.source(t, mustID(t, "kanz-services", "oms")), AuthorizeMesh())

	if _, _, sErr, cErr := handshake(srv, cli); sErr != nil || cErr != nil {
		t.Fatalf("the OMS was refused by its own adapter: server=%v client=%v", sErr, cErr)
	}
}
