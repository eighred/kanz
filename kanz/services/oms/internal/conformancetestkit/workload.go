// Package conformancetestkit provides disposable transport fixtures, never runtime identity.
package conformancetestkit

import (
	"crypto/x509"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	workload "github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type Identity interface {
	GetX509SVID() (*x509svid.SVID, error)
	GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error)
}

type Workload struct {
	workload.UnimplementedSpiffeWorkloadAPIServer
	mu           sync.Mutex
	value        *workload.X509SVIDResponse
	changed      chan struct{}
	active       atomic.Int32
	started      atomic.Int32
	initialDelay time.Duration
}

func (s *Workload) Set(t *testing.T, source Identity) {
	t.Helper()
	id, err := source.GetX509SVID()
	if err != nil {
		t.Fatal("synthetic SVID unavailable")
	}
	bundle, err := source.GetX509BundleForTrustDomain(id.ID.TrustDomain())
	if err != nil {
		t.Fatal("synthetic bundle unavailable")
	}
	var roots []byte
	for _, root := range bundle.X509Authorities() {
		roots = append(roots, root.Raw...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(id.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	var chain []byte
	for _, cert := range id.Certificates {
		chain = append(chain, cert.Raw...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = &workload.X509SVIDResponse{Svids: []*workload.X509SVID{{SpiffeId: id.ID.String(), X509Svid: chain, X509SvidKey: key, Bundle: roots}}}
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}
func (s *Workload) FetchX509SVID(_ *workload.X509SVIDRequest, stream grpc.ServerStreamingServer[workload.X509SVIDResponse]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	if values := md.Get("workload.spiffe.io"); len(values) != 1 || values[0] != "true" {
		return status.Error(codes.InvalidArgument, "workload metadata missing")
	}
	s.active.Add(1)
	s.started.Add(1)
	defer s.active.Add(-1)
	s.mu.Lock()
	delay := s.initialDelay
	s.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
	for {
		s.mu.Lock()
		value := proto.Clone(s.value).(*workload.X509SVIDResponse)
		changed := s.changed
		s.mu.Unlock()
		if err := stream.Send(value); err != nil {
			return err
		}
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-changed:
		}
	}
}
func StartWorkload(t *testing.T, source Identity) (*Workload, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workload.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Workload{}
	s.Set(t, source)
	server := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(server, s)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("workload server did not close")
		}
	})
	return s, "unix://" + path
}

func (s *Workload) Active() int32  { return s.active.Load() }
func (s *Workload) Started() int32 { return s.started.Load() }

// SetInitialDelay delays only the synthetic first WAPI response, before any client connects.
func (s *Workload) SetInitialDelay(t *testing.T, delay time.Duration) {
	t.Helper()
	if delay < 0 || delay > 2*time.Second || s.Started() != 0 {
		t.Fatal("invalid synthetic initial-response delay")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialDelay = delay
}
