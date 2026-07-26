package bus_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/transport"
)

// SEC-M3a: the production broker contract.
//
// infra/nats/nats.yaml configures the client listener with `tls { verify: true,
// verify_and_map: true }` — every client must present an SVID chaining to the
// trust bundle, and its SPIFFE URI SAN is mapped to a NATS user (and therefore
// an account) by infra/nats/tenancy.yaml. Nothing in this repository did that:
// every bus.DialNATS call site passed {URL, Name} and left NATSConfig.TLSConfig
// nil, which makes a PLAINTEXT client. So the platform could not connect to its
// own spine as deployed, and no test could see it — CI ran `nats:2 -js` with no
// TLS at all, a broker configured unlike production in the one dimension that
// decides whether a connection is possible.
//
// These tests run against the REAL nats.conf + tenants.conf, extracted from the
// ConfigMaps production applies (the same stance as the bootstrap-script
// extraction in kanz-ci.yml). They gate on BOTH preconditions — the URL and the
// cert directory — because a test that gates on the first alone does not skip
// when the second is missing, it FAILS, and an under-gated test blocks the very
// infrastructure it needs.
//
// The pair is what makes either meaningful: the positive test alone would pass
// against a broker with no TLS at all, proving nothing about production. The
// refusal test is the non-vacuous half — it proves the broker under test really
// does enforce what production enforces.

// mtlsEnv returns the broker URL and cert dir, skipping unless BOTH are present.
func mtlsEnv(t *testing.T) (url, certDir string) {
	t.Helper()
	url = os.Getenv("TEST_NATS_MTLS_URL")
	certDir = os.Getenv("TEST_NATS_MTLS_CERT_DIR")
	if url == "" || certDir == "" {
		t.Skip("TEST_NATS_MTLS_URL and TEST_NATS_MTLS_CERT_DIR must both be set")
	}
	return url, certDir
}

// staticSource is a transport.Source over PEMs already on disk. Production uses
// the auto-rotating workloadapi source (transport.NewSource); here the SVID is
// minted by the CI step that also minted the broker's, so both chain to one CA.
type staticSource struct {
	svid   *x509svid.SVID
	bundle *x509bundle.Bundle
}

func (s staticSource) GetX509SVID() (*x509svid.SVID, error) { return s.svid, nil }
func (s staticSource) GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return s.bundle, nil
}

// sourceFromDir loads one SVID + the trust bundle the CI step wrote. name selects
// which identity: "client" is risk-engine (the producer) and "consumer" is
// archiver, both minted by test/mtls/up.sh into the same directory.
func sourceFromDir(t *testing.T, dir, name string) transport.Source {
	t.Helper()
	svid, err := x509svid.Load(filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key"))
	if err != nil {
		t.Fatalf("load %s SVID from %s: %v", name, dir, err)
	}
	td, err := spiffeid.TrustDomainFromString("kanz.internal")
	if err != nil {
		t.Fatalf("trust domain: %v", err)
	}
	bundle, err := x509bundle.Load(td, filepath.Join(dir, "bundle.pem"))
	if err != nil {
		t.Fatalf("load trust bundle from %s: %v", dir, err)
	}
	return staticSource{svid: svid, bundle: bundle}
}

// TestNATSMTLS_SPIFFEClientConnectsPublishesConsumes is the pattern every
// composition root inherits: transport.NewSource -> transport.ClientTLSConfig ->
// bus.NATSConfig.TLSConfig. It proves a client wired that way completes the mTLS
// handshake against the production broker config, maps to a tenants.conf user,
// and can carry a real round-trip.
func TestNATSMTLS_SPIFFEClientConnectsPublishesConsumes(t *testing.T) {
	url, certDir := mtlsEnv(t)

	ctx := context.Background()

	// TWO identities, because the production permission model REQUIRES two.
	// tenants.conf grants risk-engine publish on its own output FACTs and
	// subscribe on its three inputs — an EMPTY intersection, deliberately: a
	// service emits what it produces and consumes what it needs, and never
	// round-trips its own traffic. So a single-identity publish-then-consume is
	// not merely awkward here, it is FORBIDDEN by the contract this test exists
	// to prove, and an earlier version of this test failed for exactly that
	// reason while looking like a broker timeout (see below).
	//
	// producer = risk-engine, consumer = archiver. That pairing is a real
	// service relationship — archiver holds subscribe on risk.portfolio.> and
	// archives the engine's output — not a fixture invented for the test.
	producerTLS := transport.ClientTLSConfig(sourceFromDir(t, certDir, "client"), transport.AuthorizeMesh())
	consumerTLS := transport.ClientTLSConfig(sourceFromDir(t, certDir, "consumer"), transport.AuthorizeMesh())

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// A REAL production subject on the REAL bootstrap-provisioned stream, and
	// no stream is created here. infra/nats/bootstrap-job.yaml binds
	// risk.portfolio.> to the RISK stream, and NATS refuses a second stream
	// overlapping a bound subject — so a throwaway stream is not available. That
	// is a better test anyway: it exercises the topology production runs.
	//
	// WHY THE OLD SUBJECT FAILED, recorded so it is not reintroduced: this used
	// to publish to test.mtls.<ts>, which appears in no account's publish
	// allow-list. $JS.API.> IS allowed, so stream creation succeeded and the
	// publish did not — and because a NATS permission denial on a request
	// subject returns NO REPLY, a JetStream publish awaiting its PubAck
	// presented as "context deadline exceeded" rather than as an authorization
	// error. The fix is to speak subjects the identities actually hold, never to
	// widen tenants.conf so a test can pass.
	const subject = "risk.portfolio.measures_computed"

	// Consumer first, so its durable exists before anything is published.
	consumerClient, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "bus-mtls-consumer", TLSConfig: consumerTLS})
	if err != nil {
		t.Fatalf("DialNATS as archiver: %v — the production broker refused a consumer that should be authorized", err)
	}
	t.Cleanup(func() { _ = consumerClient.Close() })

	// Buffered well above 1: the RISK stream is SHARED production topology, so
	// unrelated traffic can arrive and must not wedge the handler or fail the run.
	received := make(chan bus.Message, 64)
	subCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		_ = consumerClient.Subscribe(subCtx, subject, "mtls-test-"+suffix, func(_ context.Context, m bus.Message) error {
			select {
			case received <- m:
			default:
			}
			return nil
		})
	}()
	// PERMITTED sleep, and only this one: a quiet period to let the durable bind.
	// There is no bind-completed event on this seam to wait for, and publishing
	// into an unbound durable would prove nothing about delivery.
	time.Sleep(500 * time.Millisecond)

	producerClient, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "bus-mtls-producer", TLSConfig: producerTLS})
	if err != nil {
		t.Fatalf("DialNATS as risk-engine: %v — the production broker refused a producer that should be authorized", err)
	}
	t.Cleanup(func() { _ = producerClient.Close() })

	body := []byte("mtls-round-trip-" + suffix)
	if err := producerClient.Publish(ctx, bus.Message{Subject: subject, Body: body}); err != nil {
		t.Fatalf("publish over mTLS as risk-engine: %v", err)
	}

	// Drain until OUR message arrives. Matching on the unique body rather than
	// taking the first delivery is what makes this safe on a shared stream.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case m := <-received:
			if string(m.Body) == string(body) {
				return
			}
		case <-deadline:
			t.Fatalf("published %q as risk-engine but archiver never received it within 15s — "+
				"mTLS and authorization succeeded, so this is a DELIVERY failure", body)
		}
	}
}

// TestNATSMTLS_PlaintextClientRefused is the non-vacuous half. It pins the exact
// defect SEC-M3 found: a DialNATS that leaves TLSConfig nil — which is what
// EVERY call site in the repository did — must be REFUSED by the production
// broker config. If this test ever passes a plaintext client, the broker under
// test is not configured like production and the positive test above is proving
// nothing.
func TestNATSMTLS_PlaintextClientRefused(t *testing.T) {
	url, _ := mtlsEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "bus-mtls-plaintext-probe"})
	if err == nil {
		_ = client.Close()
		t.Fatal("a plaintext DialNATS CONNECTED to the broker — the mTLS gate is not enforced, " +
			"so this suite cannot prove the production contract")
	}
}
