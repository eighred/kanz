//go:build linux && amd64

package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lake-sink/internal/config"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// One absolute parent deadline covers both owner processes and native 60-second
// redelivery. It is a disposable fixture envelope, not a production work budget.
const snapshotCrashFixtureLifetime = 240 * time.Second

type snapshotOwnerInput struct {
	Deadline                                                 time.Time
	Address, Database, NATS, Landing, Output, Policy, Worker string
	WorkerSHA                                                [32]byte
	Cert, Key, Bundle                                        []byte
	Rows                                                     []sink.Row
}

type snapshotOwnerSource struct {
	leaf   *x509svid.SVID
	bundle *x509bundle.Bundle
}

func (s snapshotOwnerSource) GetX509SVID() (*x509svid.SVID, error) { return s.leaf, nil }
func (s snapshotOwnerSource) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	if td != s.leaf.ID.TrustDomain() {
		return nil, ErrDenied
	}
	return s.bundle, nil
}

// TestSnapshotOwnerSubprocess is invoked only by the source-bound test binary.
// Synthetic identities and disposable DSN travel through bounded private stdin,
// never argv, artifact output or persisted credential files.
func TestSnapshotOwnerSubprocess(t *testing.T) {
	if os.Getenv("KANZ_LAKE_OWNER_CHILD") != "1" {
		t.Skip("owned subprocess only")
	}
	maximumDeadline := time.Now().Add(snapshotCrashFixtureLifetime)
	ctx, cancel := context.WithDeadline(context.Background(), maximumDeadline)
	defer cancel()
	reader := bufio.NewReaderSize(os.Stdin, 64<<10)
	inputLine := make(chan []byte, 1)
	inputDone := make(chan error, 1)
	go func() {
		line, e := reader.ReadSlice('\n')
		if e != nil || len(line) > 64<<10 {
			inputLine <- nil
			inputDone <- e
			cancel()
			return
		}
		inputLine <- bytes.Clone(line)
		_, e = reader.ReadByte()
		inputDone <- e
		cancel()
	}()
	defer func() { _ = os.Stdin.Close(); <-inputDone }()
	var line []byte
	select {
	case line = <-inputLine:
	case <-ctx.Done():
		t.Fatal("bounded owner input unavailable")
	}
	var in snapshotOwnerInput
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF || !in.Deadline.After(time.Now()) || in.Deadline.After(maximumDeadline) || len(in.Rows) > 3 {
		t.Fatal("invalid owner fixture input")
	}
	for _, address := range []string{in.Address} {
		host, _, e := net.SplitHostPort(address)
		if e != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			t.Fatal("owner fixture requires loopback")
		}
	}
	pgcfg, err := pgxpool.ParseConfig(in.Database)
	if err != nil || net.ParseIP(pgcfg.ConnConfig.Host) == nil || !net.ParseIP(pgcfg.ConnConfig.Host).IsLoopback() {
		t.Fatal("owner database requires loopback")
	}
	broker, err := url.Parse(in.NATS)
	if err != nil || broker.Scheme != "nats" || broker.User != nil || net.ParseIP(broker.Hostname()) == nil || !net.ParseIP(broker.Hostname()).IsLoopback() {
		t.Fatal("owner broker requires literal loopback")
	}
	for _, path := range []string{in.Landing, in.Output, in.Policy, in.Worker} {
		if !filepath.IsAbs(path) {
			t.Fatal("owner fixture requires absolute owned paths")
		}
	}
	leaf, err := x509svid.Parse(in.Cert, in.Key)
	if err != nil || leaf.ID.String() != "spiffe://kanz.internal/ns/kanz-services/sa/lake-sink" {
		t.Fatal("invalid synthetic owner identity")
	}
	bundle, err := x509bundle.Parse(leaf.ID.TrustDomain(), in.Bundle)
	if err != nil {
		t.Fatal("invalid synthetic owner trust")
	}
	ctx, stopDeadline := context.WithDeadline(ctx, in.Deadline)
	defer stopDeadline()
	landing, err := sink.NewFileSink(in.Landing, "", sink.EnableActiveRunCapture)
	if err != nil {
		t.Fatal("owner landing unavailable")
	}
	defer func() {
		if landing.Close() != nil {
			t.Error("owner landing close failed")
		}
	}()
	for _, row := range in.Rows {
		if landing.Write(ctx, row) != nil {
			t.Fatal("owner seed failed")
		}
	}
	if landing.Flush() != nil {
		t.Fatal("owner seed flush failed")
	}
	// EOF is the graceful stop command; any trailing input remains a failure.
	var once sync.Once
	ready := func(v bool) {
		if v {
			once.Do(func() {
				_ = json.NewEncoder(os.Stdout).Encode(struct {
					Status string `json:"status"`
				}{"ready"})
			})
		}
	}
	cfg := config.Snapshot{Tenant: "tenant-a", Listen: in.Address, DatabaseURL: in.Database, NATSURL: in.NATS, NATSAccount: "acme", OutputDir: in.Output, PolicyFile: in.Policy, WorkerPath: in.Worker, WorkerSHA256: in.WorkerSHA, GatewayID: spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-services/sa/api-gateway")}
	err = Run(ctx, cfg, snapshotOwnerSource{leaf, bundle}, landing, in.Landing, slog.New(slog.NewTextHandler(io.Discard, nil)), bus.NewBusMetrics(prometheus.NewRegistry()), ready)
	if err != nil {
		t.Fatal("owner runtime refused")
	}
	select {
	case e := <-inputDone:
		inputDone <- e // retain joined input result for the deferred owner
		if e != io.EOF {
			t.Fatal("owner trailing input refused")
		}
	default:
		t.Fatal("owner stopped without graceful input EOF")
	}
	if json.NewEncoder(os.Stdout).Encode(struct {
		Status string `json:"status"`
	}{"closed"}) != nil {
		t.Fatal("owner closure output failed")
	}
}

type snapshotOwnerProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	done   chan error
	states chan string
	joined bool
}

func startSnapshotOwner(t *testing.T, ctx context.Context, in snapshotOwnerInput) *snapshotOwnerProcess {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal("owner test executable unavailable")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("owner fixture requires deadline")
	}
	in.Deadline = deadline
	wire, err := json.Marshal(in)
	if err != nil || len(wire) >= 64<<10 {
		t.Fatal("owner fixture input exceeds bound")
	}
	return startSnapshotFixtureProcess(t, ctx, path, "TestSnapshotOwnerSubprocess", "KANZ_LAKE_OWNER_CHILD=1", wire)
}

func startSnapshotFixtureProcess(t *testing.T, ctx context.Context, path, name, childEnv string, wire []byte) *snapshotOwnerProcess {
	t.Helper()
	if !filepath.IsAbs(path) || len(wire) >= 64<<10 {
		t.Fatal("bounded fixture process input invalid")
	}
	p := &snapshotOwnerProcess{cmd: exec.CommandContext(ctx, path, "-test.run=^"+name+"$", "-test.count=1"), done: make(chan error, 1), states: make(chan string, 3)}
	p.cmd.Env = []string{childEnv}
	p.cmd.WaitDelay = 5 * time.Second
	p.cmd.Stderr = io.Discard
	var err error
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		t.Fatal("owner input unavailable")
	}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal("owner output unavailable")
	}
	if p.cmd.Start() != nil {
		t.Fatal("owner process start failed")
	}
	go func() {
		limited := &io.LimitedReader{R: stdout, N: (64 << 10) + 1}
		scanner := bufio.NewScanner(limited)
		scanner.Buffer(make([]byte, 1024), 64<<10)
		for scanner.Scan() {
			var v struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(scanner.Bytes(), &v) == nil && (v.Status == "ready" || v.Status == "connected" || v.Status == "closed") {
				select {
				case p.states <- v.Status:
				default:
				}
			}
		}
		close(p.states)
		err := p.cmd.Wait()
		if scanner.Err() != nil || limited.N == 0 {
			err = errors.New("bounded fixture output refused")
		}
		p.done <- err
	}()
	t.Cleanup(func() {
		if !p.joined {
			_ = p.stdin.Close()
			_ = p.cmd.Process.Kill()
			<-p.done
			p.joined = true
		}
	})
	written := make(chan error, 1)
	go func() { _, e := p.stdin.Write(append(wire, '\n')); written <- e }()
	select {
	case e := <-written:
		if e != nil {
			t.Fatal("owner input write failed")
		}
	case <-ctx.Done():
		t.Fatal("owner input deadline")
	}
	return p
}

func (p *snapshotOwnerProcess) awaitReady(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case state := <-p.states:
		if state != "ready" {
			t.Fatal("owner failed before ready")
		}
	case <-ctx.Done():
		t.Fatal("owner readiness deadline")
	}
}
func (p *snapshotOwnerProcess) kill(t *testing.T, ctx context.Context) {
	t.Helper()
	defer func() { _ = p.stdin.Close() }()
	if p.cmd.Process.Kill() != nil {
		t.Fatal("actual owner kill failed")
	}
	select {
	case err := <-p.done:
		p.joined = true
		if err == nil {
			t.Fatal("killed owner unexpectedly succeeded")
		}
	case <-ctx.Done():
		t.Fatal("killed owner did not join")
	}
}
func (p *snapshotOwnerProcess) close(t *testing.T) {
	t.Helper()
	if p.stdin.Close() != nil {
		t.Fatal("owner EOF failed")
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case err := <-p.done:
		p.joined = true
		if err != nil {
			t.Fatal("owner graceful join failed")
		}
	case <-timer.C:
		_ = p.cmd.Process.Kill()
		<-p.done
		p.joined = true
		t.Fatal("owner cleanup observation expired; killed and joined")
	}
	select {
	case state := <-p.states:
		if state != "closed" {
			t.Fatal("owner closure missing")
		}
	default:
		t.Fatal("owner closure missing")
	}
}
