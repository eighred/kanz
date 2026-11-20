package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

// The checked-in fragment is parsed by the actual broker. Only this disposable
// loopback test maps the unauthenticated connection to the synthetic publisher.
func TestDedicatedStatementPublisherGrant(t *testing.T) {
	grant, err := os.ReadFile("../../../../infra/nats/custody_batch_permissions.conf")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "permissions.conf"), grant, 0600); err != nil {
		t.Fatal(err)
	}
	admin, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Wipe()
	public, err := admin.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("host: 127.0.0.1\nport: -1\njetstream {store_dir: %q}\nno_auth_user: custody-source\nauthorization {\n users: [\n { user: custody-source\ninclude permissions.conf\n }\n { nkey: %q }\n ]\n}\n", filepath.ToSlash(dir), public)
	conf := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(conf, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	opts, err := server.ProcessConfigFile(conf)
	if err != nil {
		t.Fatal("invalid publisher permission fragment")
	}
	opts.NoSigs = true
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatal("fixture broker startup failed")
	}
	s.Start()
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("fixture broker failed to listen")
	}
	control, err := nats.Connect(s.ClientURL(), nats.Nkey(public, admin.Sign))
	if err != nil {
		t.Fatal("fixture broker control unavailable")
	}
	defer control.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	js, err := jetstream.New(control)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "STATEMENTS", Subjects: []string{custody.SubjectStatement, "accounting.test.blocked"}}); err != nil {
		t.Fatal(err)
	}
	denied := make(chan error, 4)
	nc, err := nats.Connect(s.ClientURL(), nats.CustomInboxPrefix(custody.BatchInbox), nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { denied <- err }))
	if err != nil {
		t.Fatal("fixture publisher unavailable")
	}
	defer nc.Close()
	client, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Publish(ctx, custody.SubjectStatement, []byte("synthetic-grant-probe")); err != nil {
		t.Fatalf("statement or acknowledgement not permitted: %v", err)
	}
	expectDenied := func() {
		t.Helper()
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-denied:
		case <-ctx.Done():
			t.Fatal("broker did not refuse unauthorized subject")
		}
	}
	if err = nc.Publish("accounting.test.blocked", []byte("synthetic-grant-probe")); err != nil {
		t.Fatal(err)
	}
	expectDenied()
	if _, err = nc.SubscribeSync(custody.SubjectStatement); err != nil {
		t.Fatal(err)
	}
	expectDenied()
	if _, err = nc.SubscribeSync("_INBOX.another_publisher.>"); err != nil {
		t.Fatal(err)
	}
	expectDenied()
}
