package bus_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

// sourceFromDir loads the client SVID + trust bundle the CI step wrote.
func sourceFromDir(t *testing.T, dir string) transport.Source {
	t.Helper()
	svid, err := x509svid.Load(filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key"))
	if err != nil {
		t.Fatalf("load client SVID from %s: %v", dir, err)
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
	src := sourceFromDir(t, certDir)
	tlsCfg := transport.ClientTLSConfig(src, transport.AuthorizeMesh())

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	streamName := "TEST_MTLS_" + suffix
	subject := "test.mtls." + suffix

	// Out-of-band stream provisioning, over the SAME mTLS the client uses: a
	// plaintext setup connection could not reach this broker either.
	setupConn, err := nats.Connect(url, nats.Secure(tlsCfg))
	if err != nil {
		t.Fatalf("setup connect over mTLS: %v", err)
	}
	// Registered BEFORE the stream cleanup so it runs LAST (cleanups run
	// last-registered-first) — a deferred close here would tear the connection
	// down before DeleteStream could use it.
	t.Cleanup(setupConn.Close)
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	if _, err := setupJS.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subject},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		t.Fatalf("create stream over mTLS: %v", err)
	}
	t.Cleanup(func() { _ = setupJS.DeleteStream(ctx, streamName) })

	// The seam under test: DialNATS with a SPIFFE-derived TLSConfig.
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "bus-mtls-test", TLSConfig: tlsCfg})
	if err != nil {
		t.Fatalf("DialNATS with SPIFFE TLSConfig: %v — the production broker refused a client that should be authorized", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	received := make(chan bus.Message, 1)
	subCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		_ = client.Subscribe(subCtx, subject, "mtls-test-group", func(_ context.Context, m bus.Message) error {
			select {
			case received <- m:
			default:
			}
			return nil
		})
	}()
	// Let the durable bind before publishing.
	time.Sleep(500 * time.Millisecond)

	body := []byte("mtls-round-trip-" + suffix)
	if err := client.Publish(ctx, bus.Message{Subject: subject, Body: body}); err != nil {
		t.Fatalf("publish over mTLS: %v", err)
	}

	select {
	case m := <-received:
		if string(m.Body) != string(body) {
			t.Fatalf("body = %q, want %q", m.Body, body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no message received over mTLS within 10s")
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
