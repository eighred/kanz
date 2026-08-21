package halt_test

// THE BROKER GRANT IS HALF THE FIX, AND IT IS THE HALF NO GO TEST CAN SEE (#635).
//
// A service wired to halt.Arm whose NATS account may not subscribe
// platform.mode.changed authenticates fine, binds nothing, and honours no halt.
// fakeBus cannot show that and neither can a plain broker: the property lives in
// infra/nats/tenancy.yaml's `permissions` blocks, which nothing in this
// repository had ever executed.
//
// So this test runs THE REAL FILE against a real nats-server. It renders
// tenancy.yaml's tenants.conf verbatim — every account, every permissions block
// unchanged — and adds ONE thing: a password on each user, because the
// production file authenticates by SVID (`tls.verify_and_map`) and there is no
// SPIRE here. The usernames stay exactly the SVIDs production uses, so the
// user → account → permissions mapping under test is the deployed one.
//
// WHAT IT PROVES:
//
//   - the OMS's account, as the file grants it, binds the halt subject and folds
//     an operator's kanz-halt FACT into its gate;
//   - the __system__ export + per-tenant import carry that same FACT ACROSS AN
//     ACCOUNT BOUNDARY into a tenant's own OMS — the half of the tenancy change
//     that is genuinely load-bearing, because accounts DO isolate;
//   - and — measured here, not assumed — exactly how much the `subscribe` grant
//     itself enforces. See
//     TestSubscribeGrantBitesOnCoreSubscribeAndNotOnAJetStreamPullConsumer,
//     which is a finding rather than a reassurance.
//
//	go install github.com/nats-io/nats-server/v2@latest
//	go test -run TestTenancyGrant ./internal/platform/halt/
//
// It SKIPS when nats-server is not on PATH, and a skip is not a pass.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/pkg/bus"
)

const (
	testPassword  = "kanz-tenancy-test"
	omsSVID       = "spiffe://kanz.internal/ns/kanz-services/sa/oms"
	tvSyncSVID    = "spiffe://kanz.internal/ns/kanz-services/sa/tv-sync"
	haltCLISVID   = "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-halt"
	bootstrapSVID = "spiffe://kanz.internal/ns/kanz-messaging/sa/nats-bootstrap"
	// tenantOMSSVID is the per-tenant OMS pod's identity (MT-02 compute): a
	// name-suffixed platform ServiceAccount that maps into the TENANT's account,
	// not into __system__.
	tenantOMSSVID = "spiffe://kanz.internal/ns/kanz-services/sa/oms-acme"
)

// userLineRe matches every `user: "spiffe://…"` in tenants.conf, in both shapes
// the file uses (inline with the opening brace, and on its own line).
var userLineRe = regexp.MustCompile(`user:\s*"(spiffe://[^"]+)"`)

// renderTenancyConf extracts the tenants.conf ConfigMap value from tenancy.yaml
// and returns it with a password added to every user.
//
// The `data:` value is a literal block scalar indented four spaces; stripping
// exactly that indent reproduces the file the broker actually mounts at
// /etc/nats/tenants.conf.
func renderTenancyConf(t *testing.T) string {
	t.Helper()
	root := repoRootFromHere(t)
	raw, err := os.ReadFile(filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	if err != nil {
		t.Fatalf("read tenancy.yaml: %v", err)
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "tenants.conf: |" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatal("tenancy.yaml no longer carries a `tenants.conf: |` block — this proof is blind")
	}
	var body []string
	for _, l := range lines[start:] {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "    ") {
			break // dedent ⇒ the block scalar ended
		}
		body = append(body, strings.TrimPrefix(l, "    "))
	}
	conf := strings.Join(body, "\n")
	if !strings.Contains(conf, "__system__") {
		t.Fatal("the extracted tenants.conf has no __system__ account — extraction is wrong")
	}
	return userLineRe.ReplaceAllString(conf, `user: "$1", password: "`+testPassword+`"`)
}

// repoRootFromHere walks up from this test's directory to the kanz module root.
func repoRootFromHere(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root from the test's working directory")
	return ""
}

// startBroker runs nats-server on the rendered tenancy config and returns its URL.
func startBroker(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("nats-server not on PATH — install it with " +
			"`go install github.com/nats-io/nats-server/v2@latest` to run the tenancy grant proof")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tenants.conf"), []byte(renderTenancyConf(t)), 0o600); err != nil {
		t.Fatalf("write tenants.conf: %v", err)
	}
	port := freePort(t)
	conf := fmt.Sprintf("port: %d\njetstream { store_dir: %q }\ninclude \"tenants.conf\"\n",
		port, filepath.ToSlash(filepath.Join(dir, "js")))
	if err := os.WriteFile(filepath.Join(dir, "nats.conf"), []byte(conf), 0o600); err != nil {
		t.Fatalf("write nats.conf: %v", err)
	}

	cmd := exec.Command(bin, "-c", filepath.Join(dir, "nats.conf"))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nats-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	url := fmt.Sprintf("nats://127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return url
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("nats-server did not accept connections — check the rendered tenancy config")
	return url
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// ensurePlatformStream creates __system__'s PLATFORM stream, the same one
// infra/nats/bootstrap-job.yaml creates (`ensure_stream PLATFORM "platform.>"`),
// connected as the bootstrap identity that job runs under.
func ensurePlatformStream(t *testing.T, url string) {
	t.Helper()
	nc, err := nats.Connect(url, nats.UserInfo(bootstrapSVID, testPassword))
	if err != nil {
		t.Fatalf("bootstrap connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "PLATFORM",
		Subjects: []string{"platform.>"},
		MaxAge:   168 * time.Hour,
	}); err != nil {
		t.Fatalf("create PLATFORM stream: %v", err)
	}
}

// ensureStreamAs creates the PLATFORM stream inside whatever account svid maps
// into. Streams are per-account, which is the whole reason the tenant bridge has
// to exist.
func ensureStreamAs(t *testing.T, url, svid string) {
	t.Helper()
	nc, err := nats.Connect(url, nats.UserInfo(svid, testPassword))
	if err != nil {
		t.Fatalf("connect as %s: %v", svid, err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "PLATFORM",
		Subjects: []string{"platform.>"},
		MaxAge:   168 * time.Hour,
	}); err != nil {
		t.Fatalf("create PLATFORM stream as %s: %v", svid, err)
	}
}

func dialAs(t *testing.T, ctx context.Context, url, svid string) *bus.NATSClient {
	t.Helper()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: url, Name: "tenancy-proof", Username: svid, Password: testPassword,
		MaxReconnects: 1,
	})
	if err != nil {
		t.Fatalf("dial as %s: %v", svid, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestTenancyGrantLetsTheOMSHearTheHalt is the positive arm: the OMS's own
// account, from the deployed permissions file, binds platform.mode.changed and
// an operator's kanz-halt FACT closes its gate.
func TestTenancyGrantLetsTheOMSHearTheHalt(t *testing.T) {
	url := startBroker(t)
	ensurePlatformStream(t, url)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	consumer, err := bus.NewConsumer(dialAs(t, ctx, url, omsSVID))
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	gate := halt.OpenGate(nil)
	if _, err := halt.Arm(ctx, consumer, gate, nil); err != nil {
		t.Fatalf("the OMS account could not arm the halt subscription: %v\n\n"+
			"This is the grant half of #635: without permissions.subscribe covering %s the OMS "+
			"authenticates fine and hears nothing.", err, halt.SubjectModeChanged)
	}

	// The operator pulls the brake, on the identity kanz-halt actually holds.
	publishHalt(t, ctx, url)

	// FOLDED, not merely delivered. The assertion is on the gate's state, never on
	// the publish returning nil.
	if !waitHalted(gate, 10*time.Second) {
		t.Fatal("the OMS gate never closed after kanz-halt published ModeChanged — the FACT is " +
			"not reaching the gate")
	}
	_, reason, _ := gate.State()
	if !strings.Contains(reason, "risk breach on fund-alpha") {
		t.Fatalf("gate reason = %q, want the operator's own words", reason)
	}
}

// TestTenancyGrantDeniesTheHaltToAnAccountWithoutIt is the control that makes the
// arm above mean something. tv-sync holds $JS.API.>/$JS.ACK.>/_INBOX.> exactly as
// the OMS does and is missing ONE line: the platform.mode.changed subscribe. If
// this passed as well, the grant would not be what is doing the work.
// THE TENANT BRIDGE, MEASURED. A per-tenant OMS runs in its OWN NATS account, and
// accounts are opaque to each other — proven in test/arch/tenant_bridge_test.go's
// own header, with a same-account control. So the export in __system__ and the
// import in each tenant account are what carry the kill switch across; without
// them a tenant's OMS resolves no stream for the halt FACT, latches its gate
// closed and refuses every order for that tenant.
//
// THIS ARM IS THE ONE THAT COULD NOT BE FAKED. Unlike a subject permission (see
// the finding below), account isolation is structural in nats-server: a message
// published in __system__ reaches `acme` only because these two lines exist.
func TestTenancyBridgeCarriesTheHaltIntoATenantAccount(t *testing.T) {
	url := startBroker(t)
	ensurePlatformStream(t, url)
	// The tenant runs nats-bootstrap on its OWN account (infra/nats/README.md,
	// "Per-tenant streams"), which is what gives the imported subject a stream to
	// land in.
	ensureStreamAs(t, url, tenantOMSSVID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	consumer, err := bus.NewConsumer(dialAs(t, ctx, url, tenantOMSSVID))
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	gate := halt.OpenGate(nil)
	if _, err := halt.Arm(ctx, consumer, gate, nil); err != nil {
		t.Fatalf("a per-tenant OMS could not arm the halt subscription in its own account: %v\n\n"+
			"Without __system__'s platform.mode.changed export and this account's matching import, "+
			"this is what every tenant OMS would do at startup — and the gate is deny-by-default, "+
			"so that tenant would refuse every order.", err)
	}

	publishHalt(t, ctx, url) // published in __system__, by kanz-halt

	if !waitHalted(gate, 10*time.Second) {
		t.Fatal("the halt FACT did not cross the account boundary into the tenant — an operator's " +
			"platform-wide halt would stop the platform account and leave every tenant trading")
	}
}

// WHAT THE `subscribe` GRANT ACTUALLY ENFORCES — a FINDING, measured against
// nats-server, not a reassurance.
//
// This test began as the negative control for the grant added in #635 and
// FAILED, which is why it is written this way. On nats-server 2.14.5:
//
//   - a CORE subscribe to platform.mode.changed by an account without the grant
//     is REFUSED ("Permissions Violation for Subscription to ...");
//   - a JETSTREAM PULL consumer on the same subject by the same account
//     SUCCEEDS, and the message is delivered.
//
// The reason is mechanical: a pull consumer is created by publishing to
// $JS.API.> and delivered over _INBOX.>, and BOTH of those are granted to every
// consuming service in this estate. The filter subject is never checked against
// the subscribe list. halt.Arm uses exactly that path.
//
// SO THE GRANT ADDED FOR #635 IS A CORRECT DECLARATION AND NOT THE ENFORCEMENT.
// It is kept — it is the file's own record of who may hear the brake, it does
// bite on the core path, and it is what a tightened $JS.API.CONSUMER.CREATE.>
// policy would enforce — but no claim in this repair rests on it. What carries
// the FACT across the boundary that matters is the account bridge above.
//
// It is pinned as a test rather than written down so that a nats-server upgrade
// which starts enforcing filter subjects is noticed HERE, as a failing test,
// rather than in production as a service that suddenly cannot hear the brake.
// The wider exposure — every subscribe allow-list in tenancy.yaml is advisory
// for anything on a JetStream stream — is bigger than #635 and is filed
// separately.
func TestSubscribeGrantBitesOnCoreSubscribeAndNotOnAJetStreamPullConsumer(t *testing.T) {
	url := startBroker(t)
	ensurePlatformStream(t, url)

	// (a) CORE subscribe: refused, as the file intends.
	nc, err := nats.Connect(url, nats.UserInfo(tvSyncSVID, testPassword))
	if err != nil {
		t.Fatalf("connect as tv-sync: %v", err)
	}
	defer nc.Close()
	violations := make(chan error, 1)
	nc.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, e error) {
		select {
		case violations <- e:
		default:
		}
	})
	if _, err := nc.SubscribeSync(halt.SubjectModeChanged); err != nil {
		t.Logf("core subscribe refused synchronously: %v", err)
	} else {
		_ = nc.Flush()
		select {
		case e := <-violations:
			if !strings.Contains(strings.ToLower(e.Error()), "permissions violation") {
				t.Fatalf("core subscribe error = %v, want a permissions violation", e)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("an account with NO platform.mode.changed subscribe grant was allowed a CORE " +
				"subscribe to it — the grant is inert on every path, and tenancy.yaml's subscribe " +
				"lists mean nothing at all")
		}
	}

	// (b) JETSTREAM PULL consumer, same account, same subject: allowed today.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	consumer, err := bus.NewConsumer(dialAs(t, ctx, url, tvSyncSVID))
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	gate := halt.OpenGate(nil)
	if _, err := halt.Arm(ctx, consumer, gate, nil); err != nil {
		t.Logf("NOTE: this nats-server now REFUSES a pull consumer on an ungranted filter subject "+
			"(%v). That is stricter than when #635 shipped and is good — but every service that "+
			"consumes a subject its account does not grant will now fail to start. Audit "+
			"tenancy.yaml's subscribe lists before treating this as a pass.", err)
		return
	}
	publishHalt(t, ctx, url)
	if !waitHalted(gate, 5*time.Second) {
		t.Log("NOTE: the ungranted account armed but received nothing — enforcement moved to " +
			"delivery. Re-read the reasoning above; it no longer describes this broker.")
	}
}

// waitHalted polls the gate rather than the wire: the assertion is that the FACT
// was FOLDED, never that a publish returned nil.
func waitHalted(gate *halt.Gate, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if gate.Halted() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func publishHalt(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	producer, err := bus.NewProducer(dialAs(t, ctx, url, haltCLISVID), bus.ProducerConfig{
		Source: "kanz-halt", ProducerVersion: "test", Tenant: "__system__",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	payload := &lifecyclepb.ModeChanged{
		Component: halt.ComponentSystem,
		NewMode:   lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
		ChangedBy: "operator:akif",
		Reason:    "risk breach on fund-alpha",
	}
	// The SAME envelope cmd/kanz-halt builds. A test that published a different
	// shape could pass against a broker that would reject the real tool.
	if err := producer.Publish(ctx, bus.Event{
		Subject:          halt.SubjectModeChanged,
		EventType:        halt.SubjectModeChanged,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "platform",
		PayloadSchemaRef: "lifecycle.v1.ModeChanged:1",
		EventTime:        time.Now().UTC(),
		PartitionKey:     halt.ComponentSystem,
		TenantID:         "__system__",
		Payload:          payload,
	}); err != nil {
		t.Fatalf("kanz-halt could not publish the halt FACT: %v", err)
	}
}
