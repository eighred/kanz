package main

// These are component/account-bridge tests on the existing disposable mTLS
// fixture. They do not run the whole service main, admit parked lineage, certify
// deployed NetworkPolicy, or qualify #1753/POS. Canonical live FACT IDs are
// globally distinct UUIDv7; equal-ID attacks are separate storage/HTTP tests.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/lineage/internal/config"
	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/harvest"
	"github.com/eighred/kanz/services/lineage/internal/openlineage"
	"github.com/eighred/kanz/services/lineage/internal/query"
	"github.com/eighred/kanz/services/lineage/internal/server"
)

type lineageFixtureSource struct {
	svid   *x509svid.SVID
	bundle *x509bundle.Bundle
}

func (s lineageFixtureSource) GetX509SVID() (*x509svid.SVID, error) { return s.svid, nil }
func (s lineageFixtureSource) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	if td.String() != "kanz.internal" {
		return nil, errors.New("unexpected fixture trust domain")
	}
	return s.bundle, nil
}

type lineageAccount struct {
	client *bus.NATSClient
	js     jetstream.JetStream
	stored nats.JetStreamContext
}

type lineageRetainedStream struct {
	jetstream.Stream
	stored nats.JetStreamContext
	name   string
}

func lineageBrokerEnv(t *testing.T) (string, string) {
	t.Helper()
	url, dir := os.Getenv("TEST_NATS_MTLS_URL"), os.Getenv("TEST_NATS_MTLS_CERT_DIR")
	if url == "" && dir == "" {
		t.Skip("TEST_NATS_MTLS_URL and TEST_NATS_MTLS_CERT_DIR are required for the canonical account fixture")
	}
	if url == "" || dir == "" {
		t.Fatal("partially configured canonical mTLS fixture")
	}
	return url, dir
}
func lineageAccountAs(t *testing.T, ctx context.Context, url, dir, role string) *lineageAccount {
	t.Helper()
	var expected string
	switch role {
	case "systemoms":
		expected = "spiffe://kanz.internal/ns/kanz-services/sa/oms"
	case "tenantoms":
		expected = "spiffe://kanz.internal/ns/kanz-services/sa/oms-acme"
	case "audit":
		expected = "spiffe://kanz.internal/ns/kanz-services/sa/audit"
	case "webbff":
		expected = "spiffe://kanz.internal/ns/kanz-services/sa/web-bff"
	default:
		t.Fatal("unknown fixture role")
	}
	svid, err := x509svid.Load(filepath.Join(dir, role+".pem"), filepath.Join(dir, role+".key"))
	if err != nil || svid == nil || svid.ID.String() != expected {
		t.Fatal("owned dummy SVID identity did not match its existing canonical role")
	}
	td, err := spiffeid.TrustDomainFromString("kanz.internal")
	if err != nil {
		t.Fatal("fixture trust domain invalid")
	}
	bundle, err := x509bundle.Load(td, filepath.Join(dir, "bundle.pem"))
	if err != nil {
		t.Fatal("owned dummy trust bundle unavailable")
	}
	natsID, err := transport.ServiceID("kanz-messaging", "nats")
	if err != nil {
		t.Fatal("canonical server identity invalid")
	}
	tls := transport.ClientTLSConfig(lineageFixtureSource{svid: svid, bundle: bundle}, transport.AuthorizeServices(natsID))
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "lineage-owned-" + role, TLSConfig: tls})
	if err != nil {
		t.Fatal("canonical authenticated bus client unavailable")
	}
	nc, err := nats.Connect(url, nats.Secure(tls), nats.Name("lineage-readback-"+role))
	if err != nil {
		_ = client.Close()
		t.Fatal("same-role canonical readback client unavailable")
	}
	t.Cleanup(func() {
		nc.Close()
		if err := client.Close(); err != nil {
			t.Error("owned bus client did not close")
		}
	})
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal("canonical JetStream unavailable")
	}
	stored, err := nc.JetStream()
	if err != nil {
		t.Fatal("same-role stored-message reader unavailable")
	}
	return &lineageAccount{client: client, js: js, stored: stored}
}
func lineageCanonicalStream(t *testing.T, ctx context.Context, account *lineageAccount, subject, want string, age time.Duration, subjects []string) *lineageRetainedStream {
	t.Helper()
	name, err := account.js.StreamNameBySubject(ctx, subject)
	if err != nil || name != want {
		t.Fatal("required canonical account/subject stream mapping missing")
	}
	stream, err := account.js.Stream(ctx, name)
	if err != nil {
		t.Fatal("required canonical stream unavailable")
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal("canonical stream readback unavailable")
	}
	got := append([]string(nil), info.Config.Subjects...)
	expected := append([]string(nil), subjects...)
	sort.Strings(got)
	sort.Strings(expected)
	if info.Config.Name != want || strings.Join(got, "\x00") != strings.Join(expected, "\x00") || info.Config.Storage != jetstream.FileStorage ||
		info.Config.Retention != jetstream.LimitsPolicy || info.Config.MaxAge != age || info.Config.Replicas != 1 ||
		info.Config.Duplicates != 2*time.Minute || info.Config.Discard != jetstream.DiscardOld {
		t.Fatal("actual canonical stream configuration differs from the reviewed fixture contract")
	}
	return &lineageRetainedStream{Stream: stream, stored: account.stored, name: name}
}

var lineageExecutionSubjects = []string{"execution.>", "strategy.>", "order.>", "optimization.>"}

func lineageUUID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal("owned event identity allocation failed")
	}
	return id.String()
}
func lineageGraph(t *testing.T, datasets int) *graph.Memory {
	t.Helper()
	g, err := graph.NewMemory(graph.Limits{EventIndexMax: 32, DatasetMax: datasets, EdgeMax: 32, StringBytesMax: 65536})
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func lineageProducer(t *testing.T, a *lineageAccount, source string) *bus.Producer {
	t.Helper()
	p, err := bus.NewProducer(a.client, bus.ProducerConfig{Source: source, ProducerVersion: "lineage-owned-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func lineagePublish(t *testing.T, ctx context.Context, p *bus.Producer, owner, id, subject string, at time.Time) {
	t.Helper()
	event := bus.Event{EventID: id, Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1, Domain: "order", EventTime: at, TenantID: owner, CorrelationID: id, PartitionKey: "owned-" + id}
	switch subject {
	case "order.order.filled":
		event.Payload = &orderpb.OrderFilled{OrderId: "owned-" + id}
		event.PayloadSchemaRef = "order.v1.OrderFilled:1"
	case "order.order.accepted":
		event.Payload = &orderpb.OrderAccepted{OrderId: "owned-" + id}
		event.PayloadSchemaRef = "order.v1.OrderAccepted:1"
	default:
		t.Fatal("fixture cannot publish an unreviewed subject")
	}
	if err := p.Publish(ctx, event); err != nil {
		t.Fatal("canonical owned FACT publish failed")
	}
}
func lineageLastSequence(t *testing.T, ctx context.Context, stream jetstream.Stream) uint64 {
	t.Helper()
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal("owned stream snapshot unavailable")
	}
	return info.State.LastSeq
}

type lineageRetained struct {
	message  bus.Message
	env      *envelopepb.Envelope
	sequence uint64
}

func lineageFindRetained(t *testing.T, ctx context.Context, stream *lineageRetainedStream, after uint64, id string) (lineageRetained, bool) {
	t.Helper()
	last := lineageLastSequence(t, ctx, stream)
	if last < after || last-after > 128 {
		t.Fatal("owned readback interval changed or exceeded its bound")
	}
	for seq := after; seq < last; {
		seq++
		// Direct GetMsg adds retrieval metadata to Header. Republish only the
		// original stored headers: the same authenticated connection uses the
		// fixed stored-message API, without DirectGet or a fallback read route.
		readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		raw, err := stream.stored.GetMsg(stream.name, seq, nats.Context(readCtx))
		stop()
		if errors.Is(err, nats.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			t.Fatal("owned retained frame readback failed")
		}
		if raw.Sequence != seq {
			t.Fatal("owned retained sequence disagrees with its request")
		}
		env, _, err := bus.Unframe(raw.Data)
		if err != nil || env.GetEventId() != id {
			continue
		}
		if bus.Validate(env) != nil {
			t.Fatal("owned retained envelope failed canonical validation")
		}
		headers := map[string]string{}
		for key, values := range raw.Header {
			if len(values) != 1 {
				t.Fatal("owned retained header multiplicity unresolved")
			}
			headers[key] = values[0]
		}
		// NATSClient already carries this key on the wire. Verify it before
		// projecting the same typed key into canonical PlanRedrive.
		if raw.Header.Get("Kanz-Partition-Key") != env.GetPartitionKey() {
			t.Fatal("owned retained partition key disagrees with the envelope")
		}
		return lineageRetained{message: bus.Message{Subject: raw.Subject, Key: []byte(env.GetPartitionKey()), Body: append([]byte(nil), raw.Data...), Headers: headers}, env: env, sequence: seq}, true
	}
	return lineageRetained{}, false
}
func lineageWait(t *testing.T, ctx context.Context, done <-chan struct{}, condition func() bool) {
	t.Helper()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("owned component observation deadline exceeded")
		case <-done:
			t.Fatal("owned production subscription seam stopped before its observation")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type lineageOwnedConsumer struct {
	stream, name, subject string
	created               time.Time
}

func lineageConsumerInventory(t *testing.T, ctx context.Context, account *lineageAccount, streams []string, group string, allowed map[string]bool) []lineageOwnedConsumer {
	t.Helper()
	var owned []lineageOwnedConsumer
	seen := map[string]bool{}
	for _, name := range streams {
		if seen[name] {
			continue
		}
		seen[name] = true
		stream, err := account.js.Stream(ctx, name)
		if err != nil {
			t.Fatal("owned consumer stream unavailable")
		}
		list := stream.ListConsumers(ctx)
		count := 0
		for info := range list.Info() {
			count++
			if count > 256 {
				t.Fatal("consumer inventory exceeded the fixed fixture bound")
			}
			if !strings.HasPrefix(info.Config.Durable, group+"-") {
				continue
			}
			if info.Name != info.Config.Durable || !allowed[info.Config.FilterSubject] || info.Created.IsZero() {
				t.Fatal("owned consumer custody contradiction")
			}
			owned = append(owned, lineageOwnedConsumer{stream: name, name: info.Name, subject: info.Config.FilterSubject, created: info.Created})
		}
		if list.Err() != nil {
			t.Fatal("owned consumer inventory incomplete")
		}
	}
	return owned
}

type lineageRunning struct {
	cancel    context.CancelFunc
	done      chan struct{}
	err       error
	readiness *server.Readiness
	owned     []lineageOwnedConsumer
}

func lineageSameConsumers(a, b []lineageOwnedConsumer) bool {
	if len(a) != len(b) {
		return false
	}
	byName := map[string]lineageOwnedConsumer{}
	for _, entry := range a {
		byName[entry.stream+"\x00"+entry.name] = entry
	}
	for _, entry := range b {
		old, ok := byName[entry.stream+"\x00"+entry.name]
		if !ok || old.subject != entry.subject || !old.created.Equal(entry.created) {
			return false
		}
	}
	return true
}
func lineageStart(t *testing.T, parent context.Context, account *lineageAccount, subjects, streams []string, group string, consumer *bus.Consumer, handler bus.EventHandler, prior []lineageOwnedConsumer) *lineageRunning {
	t.Helper()
	allowed := map[string]bool{}
	for _, s := range subjects {
		allowed[s] = true
	}
	before := lineageConsumerInventory(t, parent, account, streams, group, allowed)
	if prior == nil {
		if len(before) != 0 {
			t.Fatal("fresh owned consumer group already exists")
		}
	} else if !lineageSameConsumers(before, prior) {
		t.Fatal("joined restart did not retain the exact owned consumer identities")
	}
	ctx, cancel := context.WithCancel(parent)
	run := &lineageRunning{cancel: cancel, done: make(chan struct{}), readiness: &server.Readiness{}, owned: append([]lineageOwnedConsumer(nil), prior...)}
	go func() {
		defer close(run.done)
		run.err = runHarvestSubscriptions(ctx, subjects, group, consumer.SubscribeReady, handler, run.readiness)
	}()
	t.Cleanup(func() {
		run.cancel()
		select {
		case <-run.done:
		case <-time.After(10 * time.Second):
			t.Error("owned subscription workers did not join")
			return
		}
		if run.readiness.Ready() {
			t.Error("owned readiness remained true after join")
		}
		cleanCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		current := lineageConsumerInventory(t, cleanCtx, account, streams, group, allowed)
		// A failed initial registration may leave only a subset. Its fresh
		// group was observed absent before the only owned create calls. Once
		// actual identities were captured, replacement is never adopted.
		if run.owned == nil {
			run.owned = append([]lineageOwnedConsumer(nil), current...)
		}
		for _, entry := range current {
			found := false
			for _, owned := range run.owned {
				if lineageSameConsumers([]lineageOwnedConsumer{entry}, []lineageOwnedConsumer{owned}) {
					found = true
					break
				}
			}
			if !found {
				t.Error("consumer replacement refused cleanup adoption")
				return
			}
		}
		for _, entry := range run.owned {
			consumer, err := account.js.Consumer(cleanCtx, entry.stream, entry.name)
			if errors.Is(err, jetstream.ErrConsumerNotFound) {
				continue
			}
			if err != nil {
				t.Error("owned consumer identity could not be re-observed")
				continue
			}
			info, err := consumer.Info(cleanCtx)
			if err != nil || info.Name != entry.name || info.Config.FilterSubject != entry.subject || !info.Created.Equal(entry.created) {
				t.Error("owned consumer identity changed before retirement")
				continue
			}
			if err := account.js.DeleteConsumer(cleanCtx, entry.stream, entry.name); err != nil {
				t.Error("owned consumer deletion failed")
			}
		}
		if len(lineageConsumerInventory(t, cleanCtx, account, streams, group, allowed)) != 0 {
			t.Error("owned consumers remained after retirement")
		}
	})
	lineageWait(t, parent, run.done, run.readiness.Ready)
	owned := lineageConsumerInventory(t, parent, account, streams, group, allowed)
	if len(owned) != len(subjects) {
		t.Fatal("actual ready consumers did not match every owned subject")
	}
	if prior != nil && !lineageSameConsumers(owned, prior) {
		t.Fatal("restart recreated/replaced an owned durable")
	}
	run.owned = owned
	return run
}
func lineageStop(t *testing.T, run *lineageRunning) {
	t.Helper()
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		t.Fatal("owned subscriptions did not join before replacement")
	}
	if run.err != nil || run.readiness.Ready() {
		t.Fatal("owned stop was not a clean joined boundary")
	}
}
func lineageHTTP(t *testing.T, g *graph.Memory, ready *server.Readiness) *httptest.Server {
	t.Helper()
	gov, err := buildGovernor(config.Config{}, discardLogger(), auth.NewSlogRecorder(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if ready == nil {
		t.Fatal("actual component readiness is required")
	}
	s := httptest.NewServer(server.New(ready, discardLogger(), server.WithLineage(query.NewService(g, gov))))
	t.Cleanup(s.Close)
	return s
}
func lineageRead(t *testing.T, ctx context.Context, s *httptest.Server, owner, path string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth.SetPrincipalHeaders(request.Header, "equal-role-user", owner, []string{"viewer"})
	response, err := s.Client().Do(request)
	if err != nil {
		t.Fatal("owned governed HTTP read failed")
	}
	return response
}

func TestCanonicalTenantFactsReachOwnedLineageHTTP(t *testing.T) {
	url, dir := lineageBrokerEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	system := lineageAccountAs(t, ctx, url, dir, "systemoms")
	tenant := lineageAccountAs(t, ctx, url, dir, "tenantoms")
	audit := lineageAccountAs(t, ctx, url, dir, "audit")
	sysStream := lineageCanonicalStream(t, ctx, system, "order.order.filled", "EXECUTION", 24*time.Hour, lineageExecutionSubjects)
	tenantStream := lineageCanonicalStream(t, ctx, tenant, "order.order.filled", "EXECUTION", 24*time.Hour, lineageExecutionSubjects)
	importStream := lineageCanonicalStream(t, ctx, audit, "tfact.acme.order.order.filled", "TENANT_FACT", 168*time.Hour, []string{"tfact.*.>"})
	lineageCanonicalStream(t, ctx, audit, "order.order.filled", "EXECUTION", 24*time.Hour, lineageExecutionSubjects)
	sysBefore, tenantBefore, importBefore := lineageLastSequence(t, ctx, sysStream), lineageLastSequence(t, ctx, tenantStream), lineageLastSequence(t, ctx, importStream)
	systemID, tenantID := lineageUUID(t), lineageUUID(t)
	if systemID == tenantID {
		t.Fatal("canonical FACT identities collided")
	}
	expected := map[string]string{systemID: bus.SystemTenant, tenantID: "acme"}
	g := lineageGraph(t, 8)
	h := harvest.New(g, openlineage.NewLogEmitter(discardLogger()))
	var admitted atomic.Int32
	consumer, err := bus.NewConsumer(audit.client, bus.WithDLQ(audit.client))
	if err != nil {
		t.Fatal(err)
	}
	handler := func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
		owner, ours := expected[env.GetEventId()]
		if !ours {
			return nil
		}
		if env.GetTenantId() != owner || env.GetPayloadSchemaRef() != "order.v1.OrderFilled:1" {
			return errors.New("owned canonical envelope identity mismatch")
		}
		if err := h.Handle(ctx, env, payload); err != nil {
			return err
		}
		admitted.Add(1)
		return nil
	}
	run := lineageStart(t, ctx, audit, []string{"order.order.filled", "tfact.acme.order.order.filled"}, []string{"EXECUTION", "TENANT_FACT"}, "lineage-owned-"+lineageUUID(t), consumer, handler, nil)
	systemAt := time.Now().UTC()
	tenantAt := systemAt.Add(time.Nanosecond)
	lineagePublish(t, ctx, lineageProducer(t, system, "oms"), bus.SystemTenant, systemID, "order.order.filled", systemAt)
	lineagePublish(t, ctx, lineageProducer(t, tenant, "oms-acme"), "acme", tenantID, "order.order.filled", tenantAt)
	lineageWait(t, ctx, run.done, func() bool { return admitted.Load() == 2 })
	retainedViews := make([]lineageRetained, 0, 3)
	for _, check := range []struct {
		stream    *lineageRetainedStream
		before    uint64
		id, owner string
	}{
		{sysStream, sysBefore, systemID, bus.SystemTenant}, {tenantStream, tenantBefore, tenantID, "acme"}, {importStream, importBefore, tenantID, "acme"},
	} {
		frame, found := lineageFindRetained(t, ctx, check.stream, check.before, check.id)
		if !found || frame.env.GetTenantId() != check.owner {
			t.Fatal("owned canonical frame was not retained on its actual account/import stream")
		}
		retainedViews = append(retainedViews, frame)
	}
	if !bytes.Equal(retainedViews[1].message.Body, retainedViews[2].message.Body) ||
		!bytes.Equal(retainedViews[1].message.Key, retainedViews[2].message.Key) {
		t.Fatal("tenant account export changed the actual retained frame or key")
	}
	// Positive same-named streams alone cannot identify an account. Require
	// the other producer's owned ID to be absent from each contrary view.
	for _, check := range []struct {
		stream *lineageRetainedStream
		after  uint64
		id     string
	}{
		{sysStream, sysBefore, tenantID},
		{tenantStream, tenantBefore, systemID},
		{importStream, importBefore, systemID},
	} {
		if _, found := lineageFindRetained(t, ctx, check.stream, check.after, check.id); found {
			t.Fatal("owned FACT reached a contradictory account/import retention view")
		}
	}
	s := lineageHTTP(t, g, run.readiness)
	for _, check := range []struct {
		owner, id, foreign string
		at                 time.Time
	}{{bus.SystemTenant, systemID, tenantID, systemAt}, {"acme", tenantID, systemID, tenantAt}} {
		response := lineageRead(t, ctx, s, check.owner, "/v1/lineage/event/"+check.id)
		var provenance query.Provenance
		decodeErr := json.NewDecoder(response.Body).Decode(&provenance)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || decodeErr != nil || provenance.Target.Dataset != (graph.DatasetID{Namespace: "kanz.order", Name: "OrderFilled"}) || provenance.Coverage.Retained != 1 {
			t.Fatal("real tenant FACT did not reach its governed owned provenance")
		}
		response = lineageRead(t, ctx, s, check.owner, "/v1/catalog/datasets")
		var catalog query.Catalog
		decodeErr = json.NewDecoder(response.Body).Decode(&catalog)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || decodeErr != nil || catalog.Count != 1 || len(catalog.Datasets) != 1 || catalog.Datasets[0].Events != 1 || !catalog.Datasets[0].LastSeen.Equal(check.at) {
			t.Fatal("canonical same-URN catalog collapsed owners")
		}
		foreign := lineageRead(t, ctx, s, check.owner, "/v1/lineage/event/"+check.foreign)
		foreignBody, _ := io.ReadAll(io.LimitReader(foreign.Body, 65537))
		_ = foreign.Body.Close()
		absent := lineageRead(t, ctx, s, check.owner, "/v1/lineage/event/"+lineageUUID(t))
		absentBody, _ := io.ReadAll(io.LimitReader(absent.Body, 65537))
		_ = absent.Body.Close()
		if foreign.StatusCode != http.StatusGone || absent.StatusCode != http.StatusGone || len(foreignBody) > 65536 || !bytes.Equal(foreignBody, absentBody) {
			t.Fatal("real governed boundary disclosed foreign existence/coverage")
		}
	}
	lineageStop(t, run)
	response := lineageRead(t, ctx, s, "acme", "/readyz")
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("joined component still served ready")
	}
	t.Log("LINEAGE_CANONICAL_OWNER_COMPONENT owners=2 producer_roles=2 subject_scopes=2 retained_account_views=3 helper_joined=1")
}

type lineageFailingDLQ struct {
	refusing, permitted bus.Publisher
	id                  string
	allow               atomic.Bool
	failures            atomic.Int32
	unexpectedSuccess   atomic.Bool
}

func (p *lineageFailingDLQ) Publish(ctx context.Context, msg bus.Message) error {
	env, _, err := bus.Unframe(msg.Body)
	if err == nil && env.GetEventId() == p.id && !p.allow.Load() {
		// This is a REAL Publish dependency call under the existing restricted
		// webbff SVID, not an injected error. Only its observed error return is
		// classified; no precise timeout/permission/network cause is inferred.
		err := p.refusing.Publish(ctx, msg)
		if err != nil {
			p.failures.Add(1)
		} else {
			p.unexpectedSuccess.Store(true)
		}
		return err
	}
	return p.permitted.Publish(ctx, msg)
}
func lineageAllowedPublisherControl(t *testing.T, ctx context.Context, p *bus.Producer, reader *lineageAccount) {
	t.Helper()
	name, err := reader.js.StreamNameBySubject(ctx, "audit.authority.decision")
	if err != nil {
		t.Fatal("canonical authority evidence stream unavailable")
	}
	base, err := reader.js.Stream(ctx, name)
	if err != nil {
		t.Fatal("authority evidence readback unavailable")
	}
	stream := &lineageRetainedStream{Stream: base, stored: reader.stored, name: name}
	before := lineageLastSequence(t, ctx, stream)
	id := lineageUUID(t)
	// A nonce-owned, typed envelope is only a fixture permission/transport
	// control. It does not assert a real user or capital authorization decision.
	if err := p.Publish(ctx, bus.Event{EventID: id, Subject: "audit.authority.decision", EventType: auth.AuthzDecisionEventType,
		EventClass: envelopepb.EventClass_EVENT_CLASS_OBSERVATION, SchemaVersion: 1, Domain: auth.AuthzDecisionDomain, EventTime: time.Now().UTC(),
		TenantID: bus.SystemTenant, CorrelationID: id, PartitionKey: "owned-" + id, PayloadSchemaRef: "observation.v1.DecisionLog:1",
		Payload: &observationpb.DecisionLog{Decider: "fixture:lineage-permission", Summary: "synthetic transport permission control"}}); err != nil {
		t.Fatal("same restricted client lost its allowed-subject PubAck control")
	}
	if _, found := lineageFindRetained(t, ctx, stream, before, id); !found {
		t.Fatal("allowed-subject PubAck was not bound to an actual retained owned frame")
	}
}
func lineageOwnedAckFloor(t *testing.T, ctx context.Context, account *lineageAccount, owned []lineageOwnedConsumer, subject string) uint64 {
	t.Helper()
	for _, entry := range owned {
		if entry.subject == subject {
			consumer, err := account.js.Consumer(ctx, entry.stream, entry.name)
			if err != nil {
				t.Fatal("owned durable readback unavailable")
			}
			info, err := consumer.Info(ctx)
			if err != nil || info.Name != entry.name || info.Config.FilterSubject != entry.subject || !info.Created.Equal(entry.created) {
				t.Fatal("owned durable cursor identity changed")
			}
			return info.AckFloor.Stream
		}
	}
	t.Fatal("owned subject cursor unresolved")
	return 0
}
func TestLineageCapacityDLQFailureAndCanonicalAgedRedrive(t *testing.T) {
	url, dir := lineageBrokerEnv(t)
	// This duration is the real canonical age precondition plus bounded
	// observation/join slack. No fake clock, shortened TTL or MinAge override.
	ctx, cancel := context.WithTimeout(context.Background(), bus.DefaultMinAge+90*time.Second)
	defer cancel()
	system := lineageAccountAs(t, ctx, url, dir, "systemoms")
	audit := lineageAccountAs(t, ctx, url, dir, "audit")
	restricted := lineageAccountAs(t, ctx, url, dir, "webbff")
	stream := lineageCanonicalStream(t, ctx, system, "order.order.filled", "EXECUTION", 24*time.Hour, lineageExecutionSubjects)
	before := lineageLastSequence(t, ctx, stream)
	dlqStream := lineageCanonicalStream(t, ctx, audit, "dlq.order.order.filled", "DLQ", 720*time.Hour, []string{"dlq.>"})
	dlqBefore := lineageLastSequence(t, ctx, dlqStream)
	seedID, failedID := lineageUUID(t), lineageUUID(t)
	g := lineageGraph(t, 1)
	h := harvest.New(g, openlineage.NewLogEmitter(discardLogger()))
	restrictedProducer := lineageProducer(t, restricted, "web-bff")
	lineageAllowedPublisherControl(t, ctx, restrictedProducer, audit)
	gated := &lineageFailingDLQ{refusing: restricted.client, permitted: audit.client, id: failedID}
	consumer, err := bus.NewConsumer(audit.client, bus.WithDLQ(gated))
	if err != nil {
		t.Fatal(err)
	}
	var seedDone, attempts atomic.Int32
	handler := func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
		if env.GetEventId() != seedID && env.GetEventId() != failedID {
			return nil
		}
		if env.GetEventId() == failedID {
			attempts.Add(1)
		}
		err := h.Handle(ctx, env, payload)
		if err == nil && env.GetEventId() == seedID {
			seedDone.Add(1)
		}
		return err
	}
	group := "lineage-redrive-" + lineageUUID(t)
	subjects := []string{"order.order.accepted", "order.order.filled"}
	run := lineageStart(t, ctx, audit, subjects, []string{"EXECUTION"}, group, consumer, handler, nil)
	producer := lineageProducer(t, system, "oms")
	lineagePublish(t, ctx, producer, bus.SystemTenant, seedID, "order.order.accepted", time.Now().UTC())
	lineageWait(t, ctx, run.done, func() bool { return seedDone.Load() == 1 })
	seed, found := lineageFindRetained(t, ctx, stream, before, seedID)
	if !found {
		t.Fatal("owned rebaseline source frame unavailable")
	}
	retainedBefore := g.Stats()
	lineagePublish(t, ctx, producer, bus.SystemTenant, failedID, "order.order.filled", time.Now().UTC())
	lineageWait(t, ctx, run.done, func() bool {
		if gated.unexpectedSuccess.Load() {
			t.Fatal("restricted DLQ publisher unexpectedly succeeded")
		}
		return attempts.Load() >= 2 && gated.failures.Load() >= 2
	})
	failed, found := lineageFindRetained(t, ctx, stream, before, failedID)
	if !found {
		t.Fatal("owned failed live frame unavailable")
	}
	if g.Stats() != retainedBefore {
		t.Fatal("capacity refusal partially mutated the graph")
	}
	if lineageOwnedAckFloor(t, ctx, audit, run.owned, "order.order.filled") >= failed.sequence {
		t.Fatal("failed real DLQ Publish advanced the owned ACK floor")
	}
	lineageAllowedPublisherControl(t, ctx, restrictedProducer, audit)
	gated.allow.Store(true)
	var parked lineageRetained
	lineageWait(t, ctx, run.done, func() bool {
		var ok bool
		parked, ok = lineageFindRetained(t, ctx, dlqStream, dlqBefore, failedID)
		return ok
	})
	if parked.message.Headers[bus.HeaderDLQClass] != bus.ClassTransient || parked.message.Headers[bus.HeaderDLQAttempts] != "1" ||
		parked.message.Headers[bus.HeaderDLQOriginalSubject] != "order.order.filled" || !bytes.Equal(parked.message.Body, failed.message.Body) ||
		!bytes.Equal(parked.message.Key, failed.message.Key) {
		t.Fatal("real transient parking changed its original frame/key/class")
	}
	parkedAt, err := time.Parse(time.RFC3339Nano, parked.message.Headers[bus.HeaderDLQParkedAt])
	if err != nil || parkedAt.After(time.Now()) {
		t.Fatal("actual parked age cannot be established")
	}
	lineageWait(t, ctx, run.done, func() bool {
		return lineageOwnedAckFloor(t, ctx, audit, run.owned, "order.order.filled") >= failed.sequence
	})
	options := bus.RedriveOptions{MinAge: bus.DefaultMinAge}
	if _, err := bus.PlanRedrive(parked.message, options, time.Now()); !errors.Is(err, bus.ErrRedriveRefused) {
		t.Fatal("young parked message bypassed canonical age refusal")
	}
	timer := time.NewTimer(time.Until(parkedAt.Add(bus.DefaultMinAge)) + time.Millisecond)
	select {
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
		t.Fatal("actual canonical redrive age was not reached")
	}
	// Explicit component rebaseline/restart: close readiness and join the old
	// workers before constructing replacement state/consumer. It is not an
	// implementation or certification of a production replay/cutover procedure.
	lineageStop(t, run)
	rebuilt := lineageGraph(t, 2)
	next := harvest.New(rebuilt, openlineage.NewLogEmitter(discardLogger()))
	seedEnv, seedPayload, err := bus.Unframe(seed.message.Body)
	if err != nil || bus.Validate(seedEnv) != nil {
		t.Fatal("owned retained rebaseline source invalid")
	}
	if err := next.Handle(ctx, seedEnv, seedPayload); err != nil {
		t.Fatal("owned rebaseline admission failed")
	}
	replacement, err := bus.NewConsumer(audit.client, bus.WithDLQ(audit.client))
	if err != nil {
		t.Fatal(err)
	}
	var redriven atomic.Int32
	nextHandler := func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
		if env.GetEventId() != failedID {
			return nil
		}
		err := next.Handle(ctx, env, payload)
		if err == nil {
			redriven.Add(1)
		}
		return err
	}
	nextRun := lineageStart(t, ctx, audit, subjects, []string{"EXECUTION"}, group, replacement, nextHandler, run.owned)
	// Resume the SAME Created/name/filter identities and post-park ACK floor.
	// Replaying the old failed live frame must not stand in for a redrive.
	if lineageOwnedAckFloor(t, ctx, audit, nextRun.owned, "order.order.filled") < failed.sequence || redriven.Load() != 0 {
		t.Fatal("old retained input was credited as fresh redrive")
	}
	if _, found := rebuilt.Get(graph.Owner(bus.SystemTenant), graph.DatasetID{Namespace: "kanz.order", Name: "OrderFilled"}); found {
		t.Fatal("old live input entered rebuilt state before redrive")
	}
	// The restored graph is intentionally incomplete and only owns admitted data.
	planned, err := bus.PlanRedrive(parked.message, options, time.Now())
	if err != nil {
		t.Fatal("aged canonical redrive planning refused")
	}
	if planned.Subject != "order.order.filled" || !bytes.Equal(planned.Body, parked.message.Body) || !bytes.Equal(planned.Key, parked.message.Key) ||
		planned.Headers[bus.HeaderDLQRedrives] != "1" {
		t.Fatal("canonical redrive changed frame/key/route")
	}
	redriveBefore := lineageLastSequence(t, ctx, stream)
	if err := system.client.Publish(ctx, planned); err != nil {
		t.Fatal("existing OMS destination authority refused canonical redrive")
	}
	lineageWait(t, ctx, nextRun.done, func() bool { return redriven.Load() >= 1 })
	actualRedrive, found := lineageFindRetained(t, ctx, stream, redriveBefore, failedID)
	if !found || actualRedrive.sequence <= failed.sequence || actualRedrive.message.Headers[bus.HeaderDLQRedrives] != "1" ||
		!bytes.Equal(actualRedrive.message.Body, planned.Body) || !bytes.Equal(actualRedrive.message.Key, planned.Key) {
		t.Fatal("redrive dispatch was not bound to the new retained canonical frame/key/count")
	}
	lineageWait(t, ctx, nextRun.done, func() bool {
		return lineageOwnedAckFloor(t, ctx, audit, nextRun.owned, "order.order.filled") >= actualRedrive.sequence
	})
	info, ok := rebuilt.Get(graph.Owner(bus.SystemTenant), graph.DatasetID{Namespace: "kanz.order", Name: "OrderFilled"})
	if !ok || info.Events != 1 || rebuilt.Coverage(graph.Owner(bus.SystemTenant)).Complete {
		t.Fatal("redriven admission did not retain honest rebuilt state")
	}
	governed := lineageHTTP(t, rebuilt, nextRun.readiness)
	response := lineageRead(t, ctx, governed, bus.SystemTenant, "/v1/lineage/event/"+failedID)
	var provenance query.Provenance
	decodeErr := json.NewDecoder(response.Body).Decode(&provenance)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || provenance.Target.Dataset != (graph.DatasetID{Namespace: "kanz.order", Name: "OrderFilled"}) ||
		provenance.Coverage.Complete || provenance.Coverage.Retained != 2 {
		t.Fatal("redriven FACT did not reach governed HTTP with honest rebuilt coverage")
	}
	foreign := lineageRead(t, ctx, governed, "acme", "/v1/lineage/event/"+failedID)
	foreignBody, _ := io.ReadAll(io.LimitReader(foreign.Body, 65537))
	_ = foreign.Body.Close()
	absent := lineageRead(t, ctx, governed, "acme", "/v1/lineage/event/"+lineageUUID(t))
	absentBody, _ := io.ReadAll(io.LimitReader(absent.Body, 65537))
	_ = absent.Body.Close()
	if foreign.StatusCode != http.StatusGone || absent.StatusCode != http.StatusGone || len(foreignBody) > 65536 || !bytes.Equal(foreignBody, absentBody) {
		t.Fatal("redrive rebuilt HTTP boundary disclosed foreign existence")
	}
	lineageStop(t, nextRun)
	t.Log("LINEAGE_CAPACITY_REDRIVE_COMPONENT same_consumer_redeliveries_at_least=2 real_publish_error_observed=1 allowed_client_controls=2 unacked_observed=1 transient_parked=1 default_age_reached=1 old_workers_joined=1 same_durable_resumed=1 rebuilt_admitted=1")
}
