package snapshot

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	natsconfig "github.com/eighred/kanz/infra/nats"
	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	"github.com/eighred/kanz/internal/testidentity"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"gopkg.in/yaml.v3"
)

// The dedicated proof supplies the pinned external server. Production imports
// only its conf parser; no embedded broker, credentials or live endpoint is used.
type snapshotBroker struct {
	t                                                 *testing.T
	binary, root, address                             string
	source, adminA, adminB, server, gateway, archiver *testidentity.Source
	permissions, archiverPermissions                  map[string]any
	process                                           *exec.Cmd
	done                                              chan error
}

func newSnapshotBroker(t *testing.T) *snapshotBroker {
	t.Helper()
	binary := os.Getenv("TEST_LAKE_NATS_SERVER")
	expectedVersion := os.Getenv("TEST_LAKE_NATS_VERSION")
	if binary == "" && expectedVersion == "" {
		t.Skip("dedicated pinned NATS executable unavailable")
	}
	if (expectedVersion != "2.12.6" && expectedVersion != "2.14.5") || !filepath.IsAbs(binary) {
		t.Fatal("pinned broker path must be absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil || len(version) > 128 || strings.TrimSpace(string(version)) != "nats-server: v"+expectedVersion {
		t.Fatal("unexpected actual broker version")
	}
	t.Logf("actual snapshot broker version: %s", expectedVersion)
	b := &snapshotBroker{t: t, binary: binary, root: t.TempDir()}
	ca, err := testidentity.NewAuthority(spiffeid.RequireTrustDomainFromString("kanz.internal"))
	if err != nil {
		t.Fatal("synthetic issuer unavailable")
	}
	issue := func(id string) *testidentity.Source {
		s, err := ca.Issue(spiffeid.RequireFromString(id), time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal("synthetic leaf unavailable")
		}
		return s
	}
	b.source = issue(natsconfig.SnapshotIdentity)
	b.gateway = issue("spiffe://kanz.internal/ns/kanz-services/sa/api-gateway")
	b.archiver = issue("spiffe://kanz.internal/ns/kanz-services/sa/archiver-acme")
	b.adminA = issue("spiffe://kanz.internal/fixture/admin-a")
	b.adminB = issue("spiffe://kanz.internal/fixture/admin-b")
	b.server = issue("spiffe://kanz.internal/ns/kanz-messaging/sa/nats")
	svid, _ := b.server.GetX509SVID()
	key, err := x509.MarshalPKCS8PrivateKey(svid.PrivateKey)
	if err != nil {
		t.Fatal("synthetic key encoding failed")
	}
	bundle, _ := b.server.GetX509BundleForTrustDomain(svid.ID.TrustDomain())
	for name, body := range map[string][]byte{
		"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: svid.Certificates[0].Raw}),
		"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		"ca.pem":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.X509Authorities()[0].Raw}),
	} {
		if os.WriteFile(filepath.Join(b.root, name), body, 0600) != nil {
			t.Fatal("synthetic TLS fixture write failed")
		}
	}
	candidate, err := natsconfig.SnapshotCandidate("acme")
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err != nil || yaml.Unmarshal(candidate, &cm) != nil {
		t.Fatal("canonical candidate unavailable")
	}
	var full map[string]any
	if json.Unmarshal([]byte(cm.Data["tenants.conf"]), &full) != nil {
		t.Fatal("candidate is not exact native-roundtripped JSON")
	}
	accounts := full["accounts"].(map[string]any)
	users := accounts["acme"].(map[string]any)["users"].([]any)
	for _, value := range users {
		user := value.(map[string]any)
		if user["user"] == natsconfig.SnapshotIdentity {
			b.permissions = user
		}
		if user["user"] == "spiffe://kanz.internal/ns/kanz-services/sa/archiver-acme" {
			b.archiverPermissions = user
		}
	}
	if b.permissions == nil || b.archiverPermissions == nil {
		t.Fatal("canonical snapshot principal missing")
	}
	// Native -t checks the complete offline account candidate, including all
	// preserved accounts/imports/exports. Runtime isolation below uses two owned
	// accounts and exactly this extracted workload permission object.
	b.writeConfig(full, "candidate.conf")
	checkCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	check := exec.CommandContext(checkCtx, binary, "-t", "-c", filepath.Join(b.root, "candidate.conf"))
	check.Stdout, check.Stderr = io.Discard, io.Discard
	if check.Run() != nil {
		t.Fatal("pinned broker rejected full candidate")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture address unavailable")
	}
	b.address = l.Addr().String()
	_ = l.Close()
	t.Cleanup(b.stop)
	b.start("acme")
	return b
}

func (b *snapshotBroker) writeConfig(doc map[string]any, name string) {
	b.t.Helper()
	doc["listen"] = "127.0.0.1:-1"
	if b.address != "" {
		doc["listen"] = b.address
	}
	doc["jetstream"] = map[string]any{"store_dir": filepath.Join(b.root, "store"), "max_memory_store": 16 << 20, "max_file_store": 256 << 20}
	doc["tls"] = map[string]any{"cert_file": filepath.Join(b.root, "cert.pem"), "key_file": filepath.Join(b.root, "key.pem"), "ca_file": filepath.Join(b.root, "ca.pem"), "verify": true, "verify_and_map": true, "timeout": 2}
	var wire bytes.Buffer
	encoder := json.NewEncoder(&wire)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(doc) != nil || wire.Len() > 512<<10 || os.WriteFile(filepath.Join(b.root, name), wire.Bytes(), 0600) != nil {
		b.t.Fatal("bounded fixture config write failed")
	}
}

func (b *snapshotBroker) start(mapped string) {
	b.t.Helper()
	admin := func(s *testidentity.Source) map[string]any {
		id, _ := s.GetX509SVID()
		return map[string]any{"user": id.ID.String()}
	}
	aUsers, bUsers := []any{admin(b.adminA)}, []any{admin(b.adminB)}
	aUsers = append(aUsers, b.archiverPermissions)
	if mapped == "acme" {
		aUsers = append(aUsers, b.permissions)
	} else if mapped == "beta" {
		bUsers = append(bUsers, b.permissions)
	} else {
		b.t.Fatal("invalid fixture account")
	}
	b.writeConfig(map[string]any{"system_account": "SYS", "accounts": map[string]any{
		"SYS":  map[string]any{"users": []any{map[string]any{"user": "spiffe://kanz.internal/fixture/system"}}},
		"acme": map[string]any{"jetstream": "enabled", "users": aUsers},
		"beta": map[string]any{"jetstream": "enabled", "users": bUsers},
	}}, "server.conf")
	b.process = exec.Command(b.binary, "-c", filepath.Join(b.root, "server.conf"))
	b.process.Stdout, b.process.Stderr = io.Discard, io.Discard
	if b.process.Start() != nil {
		b.t.Fatal("owned broker start failed")
	}
	b.done = make(chan error, 1)
	go func() { b.done <- b.process.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		conn, err := b.connect(b.adminA)
		if err == nil {
			conn.Close()
			return
		}
		select {
		case err := <-b.done:
			b.process = nil
			b.t.Fatalf("owned broker exited before readiness: %v", err)
		case <-ctx.Done():
			b.t.Fatal("authenticated broker readiness unavailable")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (b *snapshotBroker) connect(source *testidentity.Source) (*nats.Conn, error) {
	return nats.Connect("nats://"+b.address, nats.Secure(transport.ClientTLSConfig(source, transport.AuthorizeServices(spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-messaging/sa/nats")))), nats.NoReconnect(), nats.Timeout(250*time.Millisecond))
}

func (b *snapshotBroker) stop() {
	if b.process == nil {
		return
	}
	_ = b.process.Process.Kill()
	select {
	case err := <-b.done:
		b.process = nil
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			b.t.Errorf("owned broker join: %v", err)
		}
	case <-time.After(5 * time.Second):
		b.t.Fatal("owned broker join unconfirmed")
	}
}

func (b *snapshotBroker) client(ctx context.Context, disconnected func(error)) *bus.NATSClient {
	b.t.Helper()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: "nats://" + b.address, TLSConfig: transport.ClientTLSConfig(b.source, transport.AuthorizeServices(spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-messaging/sa/nats"))), DisableReconnect: true, OnDisconnect: disconnected, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		b.t.Fatal("actual workload connection failed")
	}
	b.t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestSnapshotActualBrokerAccountProfileAndRestartRefusal(t *testing.T) {
	b := newSnapshotBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admins := []*testidentity.Source{b.adminA, b.adminB}
	for _, source := range admins {
		conn, err := b.connect(source)
		if err != nil {
			t.Fatal("fixture admin unavailable")
		}
		defer conn.Close()
		js, err := jetstream.New(conn)
		if err != nil {
			t.Fatal("JetStream unavailable")
		}
		if _, err := js.CreateStream(ctx, snapshotprofile.Stream()); err != nil {
			t.Fatal("exact stream provisioning failed")
		}
	}
	disconnected := make(chan struct{}, 1)
	client := b.client(ctx, func(error) {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	})
	if client.RequireAccountIdentity(ctx, "acme", natsconfig.SnapshotIdentity) != nil || client.RequireDurableRetention(ctx, snapshotprofile.Stream()) != nil {
		t.Fatal("actual exact account/profile refused")
	}
	if client.RequireAccountIdentity(ctx, "beta", natsconfig.SnapshotIdentity) == nil {
		t.Fatal("identical foreign profile substituted for account identity")
	}
	b.stop()
	select {
	case <-disconnected:
	case <-ctx.Done():
		t.Fatal("disconnect epoch was not observed")
	}
	b.start("beta")
	if client.RequireAccountIdentity(ctx, "acme", natsconfig.SnapshotIdentity) == nil {
		t.Fatal("old validated epoch reconnected after remapping")
	}
	remapped := b.client(ctx, nil)
	if remapped.RequireDurableRetention(ctx, snapshotprofile.Stream()) != nil || remapped.RequireAccountIdentity(ctx, "beta", natsconfig.SnapshotIdentity) != nil {
		t.Fatal("remapped identical-profile positive control unavailable")
	}
	if remapped.RequireAccountIdentity(ctx, "acme", natsconfig.SnapshotIdentity) == nil {
		t.Fatal("configured tenant/account mismatch admitted")
	}
}

func TestSnapshotActualBrokerProfileCapacityAndAdministrativeRefusal(t *testing.T) {
	b := newSnapshotBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := b.connect(b.adminA)
	if err != nil {
		t.Fatal("admin connection unavailable")
	}
	defer admin.Close()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal("admin JetStream unavailable")
	}
	profile := snapshotprofile.Stream()
	stream, err := js.CreateStream(ctx, profile)
	if err != nil {
		t.Fatal("exact profile provisioning failed")
	}
	client := b.client(ctx, nil)
	if client.RequireAccountIdentity(ctx, "acme", natsconfig.SnapshotIdentity) != nil || client.RequireDurableRetention(ctx, profile) != nil {
		t.Fatal("positive profile control unavailable")
	}
	owner, err := b.connect(b.source)
	if err != nil {
		t.Fatal("workload connection unavailable")
	}
	defer owner.Close()
	ownerJS, err := jetstream.New(owner)
	if err != nil {
		t.Fatal("workload JetStream unavailable")
	}
	// The workload cannot change its own retained-request policy. Each refusal
	// is bracketed by administrator readback of the still-existing exact stream.
	for _, action := range []string{"create", "update", "purge", "delete"} {
		t.Run(action, func(t *testing.T) {
			operation, stop := context.WithTimeout(ctx, 500*time.Millisecond)
			defer stop()
			var denied error
			switch action {
			case "create":
				other := profile
				other.Name = "FORBIDDEN"
				other.Subjects = []string{"forbidden.>"}
				_, denied = ownerJS.CreateStream(operation, other)
			case "update":
				changed := profile
				changed.MaxAge = time.Hour
				_, denied = ownerJS.UpdateStream(operation, changed)
			case "purge":
				owned, e := ownerJS.Stream(operation, profile.Name)
				if e != nil {
					t.Fatal("allowed stream read failed")
				}
				denied = owned.Purge(operation)
			case "delete":
				denied = ownerJS.DeleteStream(operation, profile.Name)
			}
			if denied == nil {
				t.Fatal("workload performed administrative operation")
			}
			if client.RequireDurableRetention(ctx, profile) != nil {
				t.Fatal("refused administration changed stream")
			}
		})
	}
	changed := profile
	changed.MaxAge = time.Hour
	if _, err := js.UpdateStream(ctx, changed); err != nil {
		t.Fatal("mismatch fixture unavailable")
	}
	if client.RequireDurableRetention(ctx, profile) == nil {
		t.Fatal("age eviction admitted")
	}
	if _, err := js.UpdateStream(ctx, profile); err != nil || client.RequireDurableRetention(ctx, profile) != nil {
		t.Fatal("profile restoration failed")
	}
	// A smaller explicit fixture capacity exercises the same discard-new
	// semantics without generating unbounded test artifacts. Production still
	// requires its exact configured 10000-message profile, not this test profile.
	limited := profile
	limited.MaxMsgs = 2
	if _, err := js.UpdateStream(ctx, limited); err != nil || client.RequireDurableRetention(ctx, limited) != nil {
		t.Fatal("bounded capacity control unavailable")
	}
	if _, err := ownerJS.Publish(ctx, SubjectRequested, []byte("retained-first")); err != nil {
		t.Fatal("first accepted publication refused")
	}
	if _, err := ownerJS.Publish(ctx, SubjectRequested, []byte("retained-second")); err != nil {
		t.Fatal("second accepted publication refused")
	}
	if _, err := ownerJS.Publish(ctx, SubjectRequested, []byte("must-not-evict")); err == nil {
		t.Fatal("capacity silently evicted pending work")
	}
	first, err := stream.GetMsg(ctx, 1)
	if err != nil || string(first.Data) != "retained-first" {
		t.Fatal("old unprocessed Requested was lost")
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs != 2 || client.RequireDurableRetention(ctx, limited) == nil {
		t.Fatal("full capacity reported healthy")
	}
}

// Administrative fixture provisioning only. This proves a registered delivery
// contract, not a running archiver or Kafka durability.
func provisionSnapshotRegisteredDependencies(t *testing.T, ctx context.Context, js jetstream.JetStream) {
	t.Helper()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "DLQ", Subjects: []string{"dlq.>"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage, MaxAge: 720 * time.Hour, MaxMsgSize: -1, MaxMsgs: -1, MaxBytes: -1, MaxMsgsPerSubject: -1, Replicas: 1}); err != nil {
		t.Fatal("canonical parking fixture unavailable")
	}
	if _, err := js.CreateConsumer(ctx, snapshotprofile.Name, jetstream.ConsumerConfig{Durable: "archiver-lake_snapshot__", FilterSubject: "lake.snapshot.>", AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverAllPolicy, AckWait: bus.WorkAckWait, MaxDeliver: 64, MaxAckPending: 32}); err != nil {
		t.Fatal("registered archiver fixture unavailable")
	}
}

func TestSnapshotActualParkingAndRegisteredArchiverPreflight(t *testing.T) {
	b := newSnapshotBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := b.connect(b.adminA)
	if err != nil {
		t.Fatal("fixture admin unavailable")
	}
	defer admin.Close()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal("fixture JetStream unavailable")
	}
	if _, err = js.CreateStream(ctx, snapshotprofile.Stream()); err != nil {
		t.Fatal("lifecycle stream unavailable")
	}
	owner := b.client(ctx, func(error) {})
	if owner.RequireCanonicalParking(ctx, SubjectRequested, 65536) == nil || owner.RequireRegisteredDefaultDurable(ctx, snapshotprofile.Name, "lake.snapshot.>", "archiver") == nil {
		t.Fatal("missing required route accepted")
	}
	provisionSnapshotRegisteredDependencies(t, ctx, js)
	if owner.RequireCanonicalParking(ctx, SubjectRequested, 65536) != nil || owner.RequireRegisteredDefaultDurable(ctx, snapshotprofile.Name, "lake.snapshot.>", "archiver") != nil {
		t.Fatal("actual registered route positive control refused")
	}
	parking, err := js.Stream(ctx, "DLQ")
	if err != nil {
		t.Fatal("parking readback unavailable")
	}
	info, err := parking.Info(ctx)
	if err != nil {
		t.Fatal("parking info unavailable")
	}
	changed := info.Config
	changed.MaxAge = time.Hour
	if _, err = js.UpdateStream(ctx, changed); err != nil {
		t.Fatal("parking mutation unavailable")
	}
	if owner.RequireCanonicalParking(ctx, SubjectRequested, 65536) == nil {
		t.Fatal("changed retention accepted")
	}
	if _, err = js.UpdateStream(ctx, info.Config); err != nil {
		t.Fatal("parking restoration unavailable")
	}
	consumer, err := js.Consumer(ctx, snapshotprofile.Name, "archiver-lake_snapshot__")
	if err != nil {
		t.Fatal("registered consumer unavailable")
	}
	ci, err := consumer.Info(ctx)
	if err != nil {
		t.Fatal("registered consumer info unavailable")
	}
	stale := ci.Config
	stale.MaxDeliver = -1
	if _, err = js.UpdateConsumer(ctx, snapshotprofile.Name, stale); err != nil {
		t.Fatal("consumer mutation unavailable")
	}
	if owner.RequireRegisteredDefaultDurable(ctx, snapshotprofile.Name, "lake.snapshot.>", "archiver") == nil {
		t.Fatal("snapshot override substituted for archiver contract")
	}
	if _, err = js.UpdateConsumer(ctx, snapshotprofile.Name, ci.Config); err != nil {
		t.Fatal("consumer restoration unavailable")
	}
	if owner.RequireCanonicalParking(ctx, SubjectRequested, 65536) != nil || owner.RequireRegisteredDefaultDurable(ctx, snapshotprofile.Name, "lake.snapshot.>", "archiver") != nil {
		t.Fatal("restored routes refused")
	}
}
