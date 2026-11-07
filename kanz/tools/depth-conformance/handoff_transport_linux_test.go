//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/testidentity"
	ns "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
)

const feedBrokerID = "spiffe://kanz.internal/ns/kanz-messaging/sa/nats"
const feedGoID = "spiffe://kanz.internal/ns/kanz-services/sa/market-ingest"
const feedObserverID = "spiffe://kanz.internal/ns/kanz-services/sa/feed-fixture-observer"

type handoffBroker struct {
	t         *testing.T
	server    *ns.Server
	authority *testidentity.Authority
	observer  *nats.Conn
	js        nats.JetStreamContext
	url       string
	denied    *handoffDenials
}

// Only a count of the pinned server's actual publish-violation event survives.
// No client address, message, subject argument or credential is formatted/logged.
type handoffDenials struct{ publish atomic.Uint64 }

func (*handoffDenials) Noticef(string, ...any) {}
func (*handoffDenials) Warnf(string, ...any)   {}
func (*handoffDenials) Fatalf(string, ...any)  {}
func (*handoffDenials) Debugf(string, ...any)  {}
func (*handoffDenials) Tracef(string, ...any)  {}
func (d *handoffDenials) Errorf(format string, _ ...any) {
	if strings.HasSuffix(format, "Publish Violation - Subject %q") {
		d.publish.Add(1)
	}
}

func handoffIdentity(t *testing.T, authority *testidentity.Authority, id string) *testidentity.Source {
	t.Helper()
	source, err := authority.Issue(spiffeid.RequireFromString(id), time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal("synthetic identity issuance failed")
	}
	return source
}

func startHandoffBroker(t *testing.T, rustID string, denyRustPublish bool) *handoffBroker {
	t.Helper()
	authority, err := testidentity.NewAuthority(spiffeid.RequireTrustDomainFromString("kanz.internal"))
	if err != nil {
		t.Fatal("synthetic authority failed")
	}
	source := handoffIdentity(t, authority, feedBrokerID)
	cfg := tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeMemberOf(spiffeid.RequireTrustDomainFromString("kanz.internal")))
	cfg.MinVersion = tls.VersionTLS13
	permissions := func(subjects ...string) *ns.Permissions {
		return &ns.Permissions{Publish: &ns.SubjectPermission{Allow: subjects}, Subscribe: &ns.SubjectPermission{Allow: []string{"_INBOX.>"}}}
	}
	rustPermissions := permissions("market.book.snapshot", "market.crypto.quote")
	if denyRustPublish {
		rustPermissions.Publish = &ns.SubjectPermission{Deny: []string{">"}}
	}
	server, err := ns.NewServer(&ns.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, JetStream: true, StoreDir: t.TempDir(), MaxPayload: 1 << 20, MaxConn: 16, TLSConfig: cfg, TLSMap: true,
		Users: []*ns.User{
			{Username: feedGoID, Permissions: permissions("market.book.snapshot", "market.crypto.quote", "market.crypto.bar", "market.crypto.ingestion_coverage", "market.crypto.volume_profile")},
			{Username: rustID, Permissions: rustPermissions},
			{Username: feedObserverID, Permissions: permissions("$JS.API.>", "$JS.ACK.>")},
		}})
	if err != nil {
		t.Fatal("disposable broker configuration refused")
	}
	denied := new(handoffDenials)
	server.SetLogger(denied, false, false)
	server.Start()
	t.Cleanup(func() {
		server.Shutdown()
		done := make(chan struct{})
		go func() { server.WaitForShutdown(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("broker shutdown unconfirmed")
		}
	})
	if !server.ReadyForConnections(5 * time.Second) {
		t.Fatal("disposable broker unavailable")
	}
	observer := handoffIdentity(t, authority, feedObserverID)
	url := "tls://" + server.Addr().String()
	nc, err := nats.Connect(url, nats.Secure(tlsconfig.MTLSClientConfig(observer, observer, tlsconfig.AuthorizeID(spiffeid.RequireFromString(feedBrokerID)))), nats.Timeout(5*time.Second), nats.Name("handoff-observer"), nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
	if err != nil {
		t.Fatal("authenticated observer refused")
	}
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal("JetStream unavailable")
	}
	if _, err = js.AddStream(&nats.StreamConfig{Name: "HANDOFF", Subjects: []string{"market.>"}, Storage: nats.MemoryStorage, MaxBytes: 8 << 20, MaxMsgs: 10000, MaxAge: time.Hour}); err != nil {
		t.Fatal("stream setup failed")
	}
	// The observer administers only this disposable broker and cannot forge
	// feed output. Require an actual server-side refusal, not only config text.
	if nc.Publish("market.book.snapshot", []byte("observer-must-not-publish")) != nil || nc.FlushTimeout(time.Second) != nil {
		t.Fatal("observer refusal control transport failed")
	}
	handoffWait(t, 5*time.Second, "observer publish permission unexpectedly granted", func() bool { return denied.publish.Load() > 0 })
	info, err := js.StreamInfo("HANDOFF")
	if err != nil || info.State.Msgs != 0 || info.State.LastSeq != 0 {
		t.Fatal("observer forged accepted market facts")
	}
	return &handoffBroker{t, server, authority, nc, js, url, denied}
}

func (b *handoffBroker) connections(id string) []uint64 {
	b.t.Helper()
	state, err := b.server.Connz(&ns.ConnzOptions{Username: true, Limit: 32})
	if err != nil || state.Total > 32 {
		b.t.Fatal("bounded broker principal inventory unavailable")
	}
	var ids []uint64
	for _, c := range state.Conns {
		if c.AuthorizedUser == id {
			ids = append(ids, c.Cid)
		}
	}
	return ids
}

func handoffWait(t *testing.T, limit time.Duration, label string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(label)
}

func handoffBinary(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Skip("actual source-stamped handoff binaries unavailable")
	}
	info, err := os.Stat(value)
	if err != nil || !info.Mode().IsRegular() || !filepath.IsAbs(value) {
		t.Fatal("invalid handoff executable")
	}
	return value
}

type handoffProcess struct {
	t         *testing.T
	cmd       *exec.Cmd
	done      chan error
	joined    bool
	source    *testidentity.Workload
	broker    *handoffBroker
	principal string
}

func startHandoffProcess(t *testing.T, b *handoffBroker, binary, principal string, env []string, identity bool) *handoffProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	source, socket := testidentity.StartWorkload(t, handoffIdentity(t, b.authority, principal))
	cmd := exec.CommandContext(ctx, binary)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.Env = append(cmd.Env, env...)
	if identity {
		cmd.Env = append(cmd.Env, "SPIFFE_ENDPOINT_SOCKET="+socket)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal("handoff child start failed")
	}
	p := &handoffProcess{t: t, cmd: cmd, done: make(chan error, 1), source: source, broker: b, principal: principal}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		defer cancel()
		if !p.joined {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-p.done:
				p.joined = true
			case <-time.After(10 * time.Second):
				t.Error("handoff child join unconfirmed")
			}
		}
	})
	return p
}

func (p *handoffProcess) stop() {
	p.t.Helper()
	if p.joined {
		p.t.Fatal("duplicate handoff stop")
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM); err != nil {
		p.t.Fatal("handoff stop failed")
	}
	select {
	case err := <-p.done:
		p.joined = true
		if err != nil {
			p.t.Fatal("handoff owner did not exit cleanly")
		}
	case <-time.After(35 * time.Second):
		p.t.Fatal("handoff owner drain unconfirmed")
	}
	handoffWait(p.t, 5*time.Second, "old owner transport still active", func() bool { return p.source.Active() == 0 && len(p.broker.connections(p.principal)) == 0 })
}

func (p *handoffProcess) refused() {
	p.t.Helper()
	select {
	case err := <-p.done:
		p.joined = true
		if err == nil {
			p.t.Fatal("refused owner exited successfully")
		}
	case <-time.After(35 * time.Second):
		p.t.Fatal("owner did not refuse unavailable authority")
	}
	handoffWait(p.t, 5*time.Second, "refused owner transport still active", func() bool { return p.source.Active() == 0 && len(p.broker.connections(p.principal)) == 0 })
}

func handoffAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func handoffVersion(t *testing.T) string {
	t.Helper()
	v := os.Getenv("KANZ_FEED_HANDOFF_VERSION")
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(v) {
		t.Fatal("exact source stamp required")
	}
	return v
}
