package venueconformance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"github.com/eighred/kanz/internal/testidentity"
	"google.golang.org/protobuf/proto"
	"io"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const clientID = "spiffe://kanz.internal/ns/kanz-services/sa/oms"
const serverID = "spiffe://kanz.internal/ns/kanz-services/sa/venue-fixture"

type certificates struct {
	root *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) *certificates {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "disposable venue fixture"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &certificates{root, key}
}
func (c *certificates) issue(t *testing.T, id string, expired bool) *x509svid.SVID {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	if expired {
		until = time.Now().Add(-time.Minute)
	}
	leaf := &x509.Certificate{SerialNumber: serial, URIs: []*url.URL{uri}, BasicConstraintsValid: true, IsCA: false, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: until}
	raw, err := x509.CreateCertificate(rand.Reader, leaf, c.root, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.BasicConstraintsValid || parsed.IsCA {
		t.Fatal("synthetic SVID must explicitly declare BasicConstraints CA=false")
	}
	return &x509svid.SVID{ID: spiffeid.RequireFromString(id), Certificates: []*x509.Certificate{parsed}, PrivateKey: key}
}

type staticIdentity struct {
	svid   *x509svid.SVID
	bundle *x509bundle.Bundle
}

func (s staticIdentity) GetX509SVID() (*x509svid.SVID, error) { return s.svid, nil }
func (s staticIdentity) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return s.bundle, nil
}
func identity(ca *certificates, s *x509svid.SVID) staticIdentity {
	return staticIdentity{s, x509bundle.FromX509Authorities(spiffeid.RequireTrustDomainFromString("kanz.internal"), []*x509.Certificate{ca.root})}
}

type workloadServer = testidentity.Workload

func startWorkload(t *testing.T, ca *certificates, svid *x509svid.SVID) (*workloadServer, string) {
	t.Helper()
	return testidentity.StartWorkload(t, identity(ca, svid))
}
func serverTLS(ca *certificates, svid *x509svid.SVID) *tls.Config {
	s := identity(ca, svid)
	return tlsconfig.MTLSServerConfig(s, s, tlsconfig.AuthorizeID(spiffeid.RequireFromString(clientID)))
}
func goConnection(t *testing.T, address string, ca *certificates, svid *x509svid.SVID) *grpc.ClientConn {
	t.Helper()
	s := identity(ca, svid)
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(tlsconfig.MTLSClientConfig(s, s, tlsconfig.AuthorizeID(spiffeid.RequireFromString(serverID))))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error("Go connection close failed")
		}
	})
	return conn
}

type response struct {
	Status         string   `json:"status"`
	Version        string   `json:"version"`
	Code           int      `json:"code"`
	Response       []byte   `json:"response"`
	Fills          [][]byte `json:"fills"`
	View           string   `json:"view"`
	Catalog        []byte   `json:"catalog"`
	CatalogCode    int      `json:"catalog_code"`
	SessionsClosed bool     `json:"sessions_closed"`
}
type worker struct {
	t      *testing.T
	cmd    *exec.Cmd
	input  io.WriteCloser
	lines  chan []byte
	done   chan error
	cancel context.CancelFunc
	closed bool
}

func binary(t *testing.T) string {
	t.Helper()
	path := os.Getenv("KANZ_OMS_VENUE_BIN")
	if path == "" {
		t.Skip("KANZ_OMS_VENUE_BIN unavailable: real stamped Rust venue process required")
	}
	if !filepath.IsAbs(path) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(os.Getenv("KANZ_OMS_VENUE_VERSION")) {
		t.Fatal("absolute stamped Rust venue fixture required")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatal("Rust venue fixture unavailable")
	}
	return path
}
func startWorker(t *testing.T, config map[string]any) (*worker, response) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	cmd := exec.CommandContext(ctx, binary(t))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SYSTEMROOT=" + os.Getenv("SYSTEMROOT")}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	w := &worker{t: t, cmd: cmd, input: in, lines: make(chan []byte, 1), done: make(chan error, 1), cancel: cancel}
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal("Rust process start failed")
	}
	go func() {
		scan := bufio.NewScanner(out)
		scan.Buffer(make([]byte, 4096), 4<<20)
	scanLoop:
		for scan.Scan() {
			select {
			case w.lines <- bytes.Clone(scan.Bytes()):
			case <-ctx.Done():
				break scanLoop
			}
		}
		close(w.lines)
		w.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if !w.closed {
			cancel()
			_ = in.Close()
			select {
			case err := <-w.done:
				var exit *exec.ExitError
				if err != nil && !errors.As(err, &exit) {
					t.Errorf("cancelled worker join: %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Error("Rust process did not terminate")
			}
		}
		cancel()
	})
	return w, w.call(config)
}
func (w *worker) call(request map[string]any) response {
	w.t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err = w.input.Write(append(raw, '\n')); err != nil {
		w.t.Fatal("Rust fixture request failed")
	}
	select {
	case line, ok := <-w.lines:
		if !ok {
			w.t.Fatal("Rust fixture response missing")
		}
		var result response
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&result) != nil {
			w.t.Fatal("invalid Rust fixture response")
		}
		if result.Version != os.Getenv("KANZ_OMS_VENUE_VERSION") {
			w.t.Fatal("Rust fixture source stamp differs")
		}
		return result
	case <-time.After(35 * time.Second):
		w.t.Fatal("Rust fixture response timed out")
	}
	return response{}
}
func (w *worker) close(source *workloadServer) {
	w.t.Helper()
	r := w.call(map[string]any{"method": "Close", "timeout_ms": 2000})
	if r.Status != "ok" || !r.SessionsClosed {
		w.t.Fatal("owner cleanup not confirmed")
	}
	deadline := time.Now().Add(3 * time.Second)
	for source.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if source.Active() != 0 {
		w.t.Fatal("workload stream survived Close before process exit")
	}
	if err := w.input.Close(); err != nil {
		w.t.Fatal(err)
	}
	select {
	case err := <-w.done:
		if err != nil {
			w.t.Fatal("Rust process failed after Close")
		}
	case <-time.After(5 * time.Second):
		w.t.Fatal("Rust process did not exit")
	}
	w.closed = true
	w.cancel()
}
func configuration(endpoint, socket string) map[string]any {
	return map[string]any{"endpoint": "https://" + endpoint, "workload_socket": socket, "expected_peer": serverID, "tenant": "fixture-tenant", "mic": "MOCK", "account": "fixture-account", "require_verified_account": true, "require_order_types": true, "timeout_ms": 2000}
}
func marshal(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
