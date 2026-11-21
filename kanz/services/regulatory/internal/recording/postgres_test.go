package recording_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/audit/chain"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/filing"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/regulatory"
	regulatorypublish "github.com/eighred/kanz/internal/regulatory/publish"
	riskv1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/sustainability"
	sustainabilitypublish "github.com/eighred/kanz/internal/sustainability/publish"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	regulatorypb "github.com/eighred/kanz/kanz-schemas-go/regulatory/v1"
	sustainabilitypb "github.com/eighred/kanz/kanz-schemas-go/sustainability/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/regulatory/internal/recording"
	"github.com/eighred/kanz/services/regulatory/internal/server"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	ns "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type recordingFixture struct {
	ctx            context.Context
	dsn, schema    string
	brokerURL      string
	closePublisher func() error
	pool           *pgxpool.Pool
	stream         jetstream.Stream
	publisher      *regulatorypublish.Publisher
}

func newRecordingFixture(t *testing.T) *recordingFixture {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL required for actual filing transaction/RLS/restart proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("test database connection refused")
	}
	t.Cleanup(admin.Close)
	var super, bypass bool
	if err := admin.QueryRow(ctx, `SELECT rolsuper,rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatal("filing proof requires a NOSUPERUSER/NOBYPASSRLS role")
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	schema := "filing_" + strings.ReplaceAll(id.String(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("owned fixture schema creation refused")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("owned filing schema cleanup failed")
		}
	})
	config, err := url.Parse(dsn)
	if err != nil || (config.Scheme != "postgres" && config.Scheme != "postgresql") {
		t.Fatal("fixture needs a PostgreSQL URL")
	}
	query := config.Query()
	query.Set("search_path", schema)
	config.RawQuery = query.Encode()
	// The canonical pool retains its actual budget. The source-owned schema
	// parameter is explicit in the URL, not an assumed ConnString mutation.
	scopedDSN := config.String()
	pool, err := pg.NewTenantPool(ctx, scopedDSN, "acme")
	if err != nil {
		t.Fatal("scoped fixture pool refused")
	}
	t.Cleanup(pool.Close)
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal("fixture migration connection refused")
	}
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) != 2 {
		connection.Release()
		t.Fatal("real regulatory migration inventory changed")
	}
	sort.Strings(files)
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			connection.Release()
			t.Fatal(err)
		}
		if _, err := connection.Exec(ctx, string(body)); err != nil {
			connection.Release()
			t.Fatalf("real regulatory migration %s refused", filepath.Base(path))
		}
	}
	connection.Release()
	server, err := ns.NewServer(&ns.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, JetStream: true, StoreDir: t.TempDir(), MaxPayload: 1 << 20, MaxConn: 8, JetStreamMaxMemory: 8 << 20, JetStreamMaxStore: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	t.Cleanup(func() { server.Shutdown(); server.WaitForShutdown() })
	if !server.ReadyForConnections(3 * time.Second) {
		t.Fatal("owned broker did not become ready")
	}
	nc, err := nats.Connect(server.ClientURL())
	if err != nil {
		t.Fatal("fixture observer connection refused")
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "FILING_RECORDS", Subjects: []string{regulatorypublish.RecordedSubject, sustainabilitypublish.ObservedSubject, sustainabilitypublish.AggregatedSubject}, Storage: jetstream.MemoryStorage, MaxBytes: 4 << 20, MaxMsgs: 128, Duplicates: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: server.ClientURL(), Name: "filing-recording-proof", DisableReconnect: true, PublishTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	closePublisher := sync.OnceValue(client.Close)
	t.Cleanup(func() {
		if err := closePublisher(); err != nil {
			t.Error("owned publisher cleanup failed")
		}
	})
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "regulatory", ProducerVersion: "synthetic-proof", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := regulatorypublish.New(producer, "acme")
	if err != nil {
		t.Fatal(err)
	}
	return &recordingFixture{ctx: ctx, dsn: scopedDSN, schema: schema, pool: pool, stream: stream, publisher: publisher, brokerURL: server.ClientURL(), closePublisher: closePublisher}
}

func (f *recordingFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.closePublisher(); err != nil {
		t.Fatal("old publisher close failed")
	}
	f.pool.Close()
	pool, err := pg.NewTenantPool(f.ctx, f.dsn, "acme")
	if err != nil {
		t.Fatal("restart scoped pool refused")
	}
	t.Cleanup(pool.Close)
	f.pool = pool
	client, err := bus.DialNATS(f.ctx, bus.NATSConfig{URL: f.brokerURL, Name: "filing-restarted-proof", DisableReconnect: true, PublishTimeout: time.Second})
	if err != nil {
		t.Fatal("restart broker connection refused")
	}
	closePublisher := sync.OnceValue(client.Close)
	f.closePublisher = closePublisher
	t.Cleanup(func() {
		if err := closePublisher(); err != nil {
			t.Error("restart publisher cleanup failed")
		}
	})
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "regulatory", ProducerVersion: "synthetic-proof", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	f.publisher, err = regulatorypublish.New(producer, "acme")
	if err != nil {
		t.Fatal(err)
	}
}

func (f *recordingFixture) recorder(t *testing.T, tenant, kind string, publisher recording.Publisher) *recording.Postgres {
	t.Helper()
	pool := f.pool
	if tenant != "acme" {
		var err error
		pool, err = pg.NewTenantPool(f.ctx, f.dsn, tenant)
		if err != nil {
			t.Fatal("foreign fixture pool refused")
		}
		t.Cleanup(pool.Close)
	}
	p, err := recording.New(pool, tenant, kind, publisher, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type filingCounts struct{ Links, Artifacts, Identities, Outbox, Pending int }

func (f *recordingFixture) counts(t *testing.T) filingCounts {
	t.Helper()
	var n filingCounts
	if err := f.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM audit_chain_links),(SELECT count(*) FROM regulatory_filing_artifacts),(SELECT count(*) FROM regulatory_filing_publications),(SELECT count(*) FROM outbox),(SELECT count(*) FROM outbox WHERE published_at IS NULL)`).Scan(&n.Links, &n.Artifacts, &n.Identities, &n.Outbox, &n.Pending); err != nil {
		t.Fatal("fixture snapshot failed")
	}
	return n
}

func formPlan(t *testing.T, tenant string, calls *atomic.Int32) recording.Plan {
	t.Helper()
	manifest := []byte(`{"version":1,"origin":"filing-request/unverified","GrossAssetValue":"1000","NetAssetValue":"900","VaR":"5","GrossExposure":"1200"}`)
	digest := sha256.Sum256(manifest)
	input := hex.EncodeToString(digest[:])
	return recording.Plan{Tenant: tenant, Portfolio: "pf1", Framework: "FORM_PF", AsOf: time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC), InputDigest: input, Build: func(scope filing.PublicationScope, signer filing.Signer) (*regulatorypb.FilingRecordBody, map[string]any, error) {
		calls.Add(1)
		report, err := regulatory.FileFormPF(regulatory.FormPFInputs{GrossAssetValue: big.NewRat(1000, 1), NetAssetValue: big.NewRat(900, 1), VaR: big.NewRat(5, 1), GrossExposure: big.NewRat(1200, 1)}, scope.AsOf, signer)
		if err != nil {
			return nil, nil, err
		}
		wire, err := regulatorypublish.Report(report, scope.ReportID, &domainpb.MeasureProvenance{Method: string(riskv1.MethodPortfolioArithmetic), InputManifestDigest: scope.InputDigest})
		if err != nil {
			return nil, nil, err
		}
		return &regulatorypb.FilingRecordBody{Report: wire, TenantId: scope.Tenant, PortfolioId: scope.Portfolio, InputOrigin: sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED, InputManifest: manifest, InputManifestDigest: scope.InputDigest, ProducerVersion: "synthetic-proof", FilingCanonical: report.Canonical(), RecordedAt: timestamppb.New(scope.RecordedAt)}, report.Body(), nil
	}}
}

type acknowledgedFailure struct {
	publisher recording.Publisher
	fail      atomic.Bool
}

func (p *acknowledgedFailure) Tenant() string { return p.publisher.Tenant() }
func (p *acknowledgedFailure) Publish(ctx context.Context, event bus.Event) error {
	if err := p.publisher.Publish(ctx, event); err != nil {
		return err
	}
	if p.fail.Swap(false) {
		return errors.New("synthetic post-publication acknowledgement loss")
	}
	return nil
}

func TestPublishRegulatoryAtomicRestartAndBrokerDedup(t *testing.T) {
	f := newRecordingFixture(t)
	fault := &acknowledgedFailure{publisher: f.publisher}
	fault.fail.Store(true)
	p := f.recorder(t, "acme", "chain", fault)
	var calls atomic.Int32
	plan := formPlan(t, "acme", &calls)
	plan.AsOf = plan.AsOf.Add(123 * time.Nanosecond)
	if _, err := p.Record(f.ctx, plan); !errors.Is(err, recording.ErrUnpublished) {
		t.Fatalf("expected committed/pending response: %v", err)
	}
	if got := f.counts(t); got != (filingCounts{Links: 2, Artifacts: 1, Identities: 1, Outbox: 1, Pending: 1}) {
		t.Fatalf("partial artifact/outbox commit: %+v", got)
	}
	var original []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT response FROM regulatory_filing_artifacts`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	// Reopen both actual dependencies, not only a Go recorder over the old
	// objects. This is connection/object restart evidence, not a killed process.
	f.reopen(t)
	restarted := f.recorder(t, "acme", "chain", f.publisher)
	response, err := restarted.Record(f.ctx, plan)
	if err != nil || !bytes.Equal(response, original) || calls.Load() != 1 {
		t.Fatalf("restart recomputed/re-signed filing: calls=%d err=%v", calls.Load(), err)
	}
	if _, err := restarted.Record(f.ctx, plan); err != nil {
		t.Fatal(err)
	}
	if got := f.counts(t); got != (filingCounts{Links: 2, Artifacts: 1, Identities: 1, Outbox: 1}) {
		t.Fatalf("duplicate changed durable history: %+v", got)
	}
	info, err := f.stream.Info(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 {
		t.Fatal("acknowledgement loss duplicated the FACT")
	}
	message, err := f.stream.GetMsg(f.ctx, info.State.LastSeq)
	if err != nil {
		t.Fatal("actual regulatory FACT missing")
	}
	envelope, raw, err := bus.Unframe(message.Data)
	if err != nil || bus.Validate(envelope) != nil {
		t.Fatal("actual regulatory envelope invalid")
	}
	var observed regulatorypb.FilingRecorded
	if proto.Unmarshal(raw, &observed) != nil {
		t.Fatal("actual filing payload malformed")
	}
	if _, err := regulatorypublish.Event(&observed); err != nil {
		t.Fatal(err)
	}
	if observed.Body.SignatureKind != regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_AUDIT_CHAIN || chain.Next(observed.Body.FilingPrevHash, observed.Body.FilingCanonical) != observed.Body.Report.Signature || observed.BindingPrevHash != observed.Body.Report.Signature || envelope.EventId != observed.Body.Report.ReportId || envelope.TenantId != "acme" {
		t.Fatal("original/context chain or published identity changed")
	}
	var storedTime time.Time
	var storedArtifact []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT o.event_time,a.artifact FROM outbox o JOIN regulatory_filing_artifacts a ON a.tenant_id=o.tenant_id AND a.report_id::text=o.correlation_id WHERE a.report_id=$1 AND o.event_type=$2`, observed.Body.Report.ReportId, regulatorypublish.RecordedSubject).Scan(&storedTime, &storedArtifact); err != nil {
		t.Fatal("recording timestamp custody query failed")
	}
	var retained regulatorypb.FilingRecorded
	var served struct {
		RecordedAt time.Time `json:"recorded_at"`
	}
	if proto.Unmarshal(storedArtifact, &retained) != nil || !proto.Equal(&retained, &observed) || json.Unmarshal(response, &served) != nil {
		t.Fatal("recording timestamp representations could not be joined")
	}
	recordedAt := observed.Body.RecordedAt.AsTime()
	if recordedAt.Nanosecond()%1000 != 0 || !storedTime.Equal(recordedAt) || !envelope.EventTime.AsTime().Equal(recordedAt) || !served.RecordedAt.Equal(recordedAt) || !observed.Body.Report.AsOf.AsTime().Equal(plan.AsOf) {
		t.Fatal("artifact, actual outbox, response and envelope recording times diverged or effective time changed")
	}
	previousBinding := observed.BindingHash
	plan.AsOf = plan.AsOf.Add(24 * time.Hour)
	if _, err := restarted.Record(f.ctx, plan); err != nil {
		t.Fatal("new effective filing could not extend reopened chain")
	}
	info, err = f.stream.Info(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	message, err = f.stream.GetMsg(f.ctx, info.State.LastSeq)
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err = bus.Unframe(message.Data)
	if err != nil || proto.Unmarshal(raw, &observed) != nil {
		t.Fatal("new chain-continuity FACT invalid")
	}
	if calls.Load() != 2 || observed.Body.FilingPrevHash != previousBinding || f.counts(t) != (filingCounts{Links: 4, Artifacts: 2, Identities: 2, Outbox: 2}) {
		t.Fatal("distinct filing did not continue the reopened shared chain")
	}
}

func TestPublishRegulatoryRollbackAndConcurrentSemanticIdentity(t *testing.T) {
	f := newRecordingFixture(t)
	p := f.recorder(t, "acme", "chain", f.publisher)
	var calls atomic.Int32
	plan := formPlan(t, "acme", &calls)
	originalBuild := plan.Build
	plan.Build = func(scope filing.PublicationScope, signer filing.Signer) (*regulatorypb.FilingRecordBody, map[string]any, error) {
		_, _, err := originalBuild(scope, signer)
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("synthetic failure after original signature")
	}
	if _, err := p.Record(f.ctx, plan); err == nil {
		t.Fatal("post-signature failure committed")
	}
	if got := f.counts(t); got != (filingCounts{}) {
		t.Fatalf("rollback retained state: %+v", got)
	}
	if _, err := f.pool.Exec(f.ctx, `CREATE FUNCTION refuse_fixture_outbox() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic canonical enqueue refusal'; END $$; CREATE TRIGGER refuse_fixture_outbox BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION refuse_fixture_outbox()`); err != nil {
		t.Fatal("fixture enqueue refusal installation failed")
	}
	plan.Build = originalBuild
	if _, err := p.Record(f.ctx, plan); err == nil {
		t.Fatal("failed canonical Enqueue committed")
	}
	if got := f.counts(t); got != (filingCounts{}) {
		t.Fatalf("enqueue rollback retained state: %+v", got)
	}
	if _, err := f.pool.Exec(f.ctx, `DROP TRIGGER refuse_fixture_outbox ON outbox; DROP FUNCTION refuse_fixture_outbox()`); err != nil {
		t.Fatal("fixture refusal removal failed")
	}
	calls.Store(0)
	type result struct {
		response []byte
		err      error
	}
	results := make(chan result, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() { defer workers.Done(); response, err := p.Record(f.ctx, plan); results <- result{response, err} }()
	}
	workers.Wait()
	close(results)
	var first []byte
	for result := range results {
		if result.err != nil {
			t.Fatal("concurrent filing recording failed")
		}
		if first == nil {
			first = result.response
		}
		if !bytes.Equal(first, result.response) {
			t.Fatal("concurrent semantic request minted distinct artifacts")
		}
	}
	if calls.Load() != 1 || f.counts(t) != (filingCounts{Links: 2, Artifacts: 1, Identities: 1, Outbox: 1}) {
		t.Fatal("concurrent requests did not share one atomic artifact")
	}
}

func TestPublishRegulatoryTenantIsolationAndHashSignatureKind(t *testing.T) {
	f := newRecordingFixture(t)
	acme := f.recorder(t, "acme", "hash", f.publisher)
	var calls atomic.Int32
	wrong := formPlan(t, "foreign", &calls)
	if _, err := acme.Record(f.ctx, wrong); err == nil || calls.Load() != 0 || f.counts(t) != (filingCounts{}) {
		t.Fatal("foreign plan reached lookup/sign/record")
	}
	response, err := acme.Record(f.ctx, formPlan(t, "acme", &calls))
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		ReportID string `json:"report_id"`
	}
	if json.Unmarshal(response, &output) != nil || !filing.UUIDv7(output.ReportID) {
		t.Fatal("recorded response lost domain identity")
	}
	foreign, err := pg.NewTenantPool(f.ctx, f.dsn, "foreign")
	if err != nil {
		t.Fatal("foreign restricted fixture pool refused")
	}
	defer foreign.Close()
	if recorder, err := recording.New(foreign, "foreign", "hash", f.publisher, nil); err == nil || recorder != nil {
		t.Fatal("publisher/storage tenant mismatch accepted")
	}
	var visible int
	if err := foreign.QueryRow(f.ctx, `SELECT count(*) FROM regulatory_filing_artifacts WHERE report_id=$1`, output.ReportID).Scan(&visible); err != nil || visible != 0 {
		t.Fatal("restricted tenant pool discovered another tenant artifact")
	}
	if err := foreign.QueryRow(f.ctx, `SELECT count(*) FROM outbox`).Scan(&visible); err != nil || visible != 0 {
		t.Fatal("restricted tenant pool discovered another tenant outbox")
	}
	// Non-vacuity: another scoped instance records its OWN artifact over the
	// same real schema. This tests RLS, not broker account isolation; the latter
	// uses the separate actual-tenancy/SVID fixture in infra/nats.
	systemPool, err := pg.NewTenantPool(f.ctx, f.dsn, "__system__")
	if err != nil {
		t.Fatal("second scoped pool refused")
	}
	defer systemPool.Close()
	systemClient, err := bus.DialNATS(f.ctx, bus.NATSConfig{URL: f.brokerURL, Name: "system-filing-rls-proof", DisableReconnect: true, PublishTimeout: time.Second})
	if err != nil {
		t.Fatal("second scoped publisher connection refused")
	}
	defer func() {
		if err := systemClient.Close(); err != nil {
			t.Error("second publisher cleanup failed")
		}
	}()
	systemProducer, err := bus.NewProducer(systemClient, bus.ProducerConfig{Source: "regulatory", ProducerVersion: "synthetic-proof", Tenant: "__system__"})
	if err != nil {
		t.Fatal(err)
	}
	systemPublisher, err := regulatorypublish.New(systemProducer, "__system__")
	if err != nil {
		t.Fatal(err)
	}
	system, err := recording.New(systemPool, "__system__", "hash", systemPublisher, nil)
	if err != nil {
		t.Fatal(err)
	}
	systemResponse, err := system.Record(f.ctx, formPlan(t, "__system__", &calls))
	if err != nil {
		t.Fatal("second tenant could not record its own filing")
	}
	var systemOutput struct {
		ReportID string `json:"report_id"`
	}
	if json.Unmarshal(systemResponse, &systemOutput) != nil || systemOutput.ReportID == output.ReportID || !filing.UUIDv7(systemOutput.ReportID) {
		t.Fatal("two tenants reused a report identity")
	}
	if err := systemPool.QueryRow(f.ctx, `SELECT count(*) FROM regulatory_filing_artifacts`).Scan(&visible); err != nil || visible != 1 {
		t.Fatal("second tenant cannot see exactly its own artifact")
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM regulatory_filing_artifacts WHERE report_id=$1`, systemOutput.ReportID).Scan(&visible); err != nil || visible != 0 {
		t.Fatal("first tenant discovered the second tenant artifact")
	}
	info, err := f.stream.Info(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	message, err := f.stream.GetMsg(f.ctx, info.State.LastSeq)
	if err != nil {
		t.Fatal("hash-mode FACT missing")
	}
	_, raw, err := bus.Unframe(message.Data)
	if err != nil {
		t.Fatal(err)
	}
	var record regulatorypb.FilingRecorded
	if proto.Unmarshal(raw, &record) != nil || record.Body.SignatureKind != regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_CONTENT_HASH || record.Body.FilingPrevHash != "" || f.counts(t).Links != 2 {
		t.Fatal("plain content hash acquired a false original-chain claim")
	}
}

func TestPublishRegulatoryUnrepresentableWireProjectionRollsBack(t *testing.T) {
	f := newRecordingFixture(t)
	p := f.recorder(t, "acme", "chain", f.publisher)
	var calls atomic.Int32
	plan := formPlan(t, "acme", &calls)
	plan.Framework = "FRTB"
	manifest := []byte(`{"origin":"filing-request/unverified","FRTB_DELTA":"10000000000000000001","FRTB_VEGA":"0","FRTB_CURVATURE":"0","FRTB_DRC":"0","FRTB_RRAO":"0","FRTB_TOTAL":"10000000000000000001"}`)
	digest := sha256.Sum256(manifest)
	plan.InputDigest = hex.EncodeToString(digest[:])
	plan.Build = func(scope filing.PublicationScope, signer filing.Signer) (*regulatorypb.FilingRecordBody, map[string]any, error) {
		calls.Add(1)
		large, _ := new(big.Rat).SetString("10000000000000000001")
		values := map[string]*big.Rat{"FRTB_DELTA": large, "FRTB_VEGA": new(big.Rat), "FRTB_CURVATURE": new(big.Rat), "FRTB_DRC": new(big.Rat), "FRTB_RRAO": new(big.Rat), "FRTB_TOTAL": new(big.Rat).Set(large)}
		report, err := regulatory.BuildReport(regulatory.FRTB, scope.AsOf, values, signer)
		if err != nil {
			return nil, nil, err
		}
		wire, err := regulatorypublish.Report(report, scope.ReportID, &domainpb.MeasureProvenance{Method: string(riskv1.MethodFRTBStandardised), InputManifestDigest: scope.InputDigest})
		if err != nil {
			return nil, nil, err
		}
		return &regulatorypb.FilingRecordBody{Report: wire, TenantId: scope.Tenant, PortfolioId: scope.Portfolio, InputOrigin: sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED, InputManifest: manifest, InputManifestDigest: scope.InputDigest, ProducerVersion: "synthetic-proof", FilingCanonical: report.Canonical(), RecordedAt: timestamppb.New(scope.RecordedAt)}, report.Body(), nil
	}
	if _, err := p.Record(f.ctx, plan); !errors.Is(err, recording.ErrInvalidInput) {
		t.Fatalf("unrepresentable wire money did not refuse: %v", err)
	}
	if calls.Load() != 1 || f.counts(t) != (filingCounts{}) {
		t.Fatal("wire refusal retained a signature/artifact/outbox effect")
	}
	info, err := f.stream.Info(f.ctx)
	if err != nil || info.State.Msgs != 0 {
		t.Fatal("wire refusal published a FACT")
	}
}

func TestPublishRegulatoryRefusesAlteredPendingPayloadAndRetainedPreimage(t *testing.T) {
	f := newRecordingFixture(t)
	fault := &acknowledgedFailure{publisher: f.publisher}
	fault.fail.Store(true)
	p := f.recorder(t, "acme", "chain", fault)
	var calls atomic.Int32
	plan := formPlan(t, "acme", &calls)
	if _, err := p.Record(f.ctx, plan); !errors.Is(err, recording.ErrUnpublished) {
		t.Fatal("expected retained publication failure")
	}
	var originalPayload, originalArtifact []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT payload FROM outbox`).Scan(&originalPayload); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT artifact FROM regulatory_filing_artifacts`).Scan(&originalArtifact); err != nil {
		t.Fatal(err)
	}
	var altered regulatorypb.FilingRecorded
	if proto.Unmarshal(originalPayload, &altered) != nil {
		t.Fatal("fixture payload invalid")
	}
	altered.Body.TenantId = "foreign"
	changed, err := proto.Marshal(&altered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE outbox SET payload=$1`, changed); err != nil {
		t.Fatal("owned pending payload mutation failed")
	}
	restarted := f.recorder(t, "acme", "chain", f.publisher)
	if _, err := restarted.Record(f.ctx, plan); !errors.Is(err, recording.ErrUnpublished) {
		t.Fatal("altered pending FACT was published")
	}
	if calls.Load() != 1 || f.counts(t).Pending != 1 {
		t.Fatal("tamper refusal recomputed or marked the FACT")
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE outbox SET payload=$1`, originalPayload); err != nil {
		t.Fatal("owned pending payload restore failed")
	}
	if _, err := restarted.Record(f.ctx, plan); err != nil {
		t.Fatal("restored pending FACT did not recover")
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE regulatory_filing_artifacts SET artifact=$1`, changed); err == nil {
		t.Fatal("ordinary artifact UPDATE was not refused")
	}
	// Simulate storage corruption using ONLY this owned schema's setup role.
	// Its DDL authority is intentional; this does not certify an app role's
	// inability to ALTER/drop an immutable trigger in a deployed database.
	writeArtifact := func(value []byte) {
		t.Helper()
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(f.ctx, `ALTER TABLE regulatory_filing_artifacts DISABLE TRIGGER regulatory_filing_artifacts_immutable`); err != nil {
			t.Fatal("owned corruption setup refused")
		}
		if _, err := tx.Exec(f.ctx, `UPDATE regulatory_filing_artifacts SET artifact=$1`, value); err != nil {
			t.Fatal("owned corruption mutation refused")
		}
		if _, err := tx.Exec(f.ctx, `ALTER TABLE regulatory_filing_artifacts ENABLE TRIGGER regulatory_filing_artifacts_immutable`); err != nil {
			t.Fatal("owned corruption guard restore refused")
		}
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatal("owned corruption setup commit refused")
		}
	}
	writeArtifact(changed)
	if _, err := restarted.Record(f.ctx, plan); err == nil {
		t.Fatal("altered full context was returned on retry")
	}
	if calls.Load() != 1 {
		t.Fatal("corrupt retained artifact caused a replacement signature")
	}
	if proto.Unmarshal(originalArtifact, &altered) != nil {
		t.Fatal("original fixture artifact invalid")
	}
	altered.Body.ProducerVersion = "synthetic-rehashed-corruption"
	rehashed, err := regulatorypublish.BindingCanonical(altered.Body)
	if err != nil {
		t.Fatal(err)
	}
	altered.BindingHash = chain.Next(altered.BindingPrevHash, rehashed)
	if _, err := regulatorypublish.Event(&altered); err != nil {
		t.Fatal("rehash control failed before the stored-witness boundary")
	}
	changed, err = proto.Marshal(&altered)
	if err != nil {
		t.Fatal(err)
	}
	writeArtifact(changed)
	if _, err := restarted.Record(f.ctx, plan); err == nil {
		t.Fatal("self-consistent fabricated context bypassed the actually committed chain witness")
	}
	writeArtifact(originalArtifact)
	if _, err := restarted.Record(f.ctx, plan); err != nil {
		t.Fatal("restored retained artifact did not recover")
	}
	info, err := f.stream.Info(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 {
		t.Fatal("tamper/refusal recovery duplicated broker history")
	}
}

func TestPublishSustainabilitySharesTheArtifactTransaction(t *testing.T) {
	f := newRecordingFixture(t)
	p := f.recorder(t, "acme", "chain", f.publisher)
	var calls atomic.Int32
	plan := formPlan(t, "acme", &calls)
	plan.Framework = "TCFD"
	manifest := []byte(`{"version":1,"origin":"filing-request/unverified","holdings":[{"instrument":"synthetic","market_value":"100","scopes":[1,1,2],"revenue":"10","evic":"50","esg":[70,60,80,90]}],"scenario":{"name":"synthetic-request","carbon_price":1,"physical_severity":0.1},"glide_path":{"base_year":2020,"target_year":2050,"base_emissions":100},"year":2030,"temperature_sensitivity":2}`)
	digest := sha256.Sum256(manifest)
	plan.InputDigest = hex.EncodeToString(digest[:])
	plan.Build = func(scope filing.PublicationScope, signer filing.Signer) (*regulatorypb.FilingRecordBody, map[string]any, error) {
		calls.Add(1)
		holding := sustainability.Holding{InstrumentID: "synthetic", MarketValue: 100, ESG: sustainability.ESGScore{Overall: 70, Environmental: 60, Social: 80, Governance: 90}, Carbon: sustainability.CarbonMetrics{Scope1: 1, Scope2: 1, Scope3: 2, Revenue: 10, EVIC: 50}}
		input := sustainability.DisclosureInputs{Holdings: []sustainability.Holding{holding}, GlidePath: sustainability.GlidePath{BaseYear: 2020, TargetYear: 2050, BaseEmissions: 100}, Year: 2030, Scenario: sustainability.ClimateScenario{Name: "synthetic-request", CarbonPrice: 1, PhysicalSeverity: 0.1}, TempSensitivity: 2}
		report, err := sustainability.FileTCFD(input, scope.AsOf, signer)
		if err != nil {
			return nil, nil, err
		}
		wire, err := regulatorypublish.Report(report, scope.ReportID, &domainpb.MeasureProvenance{Method: string(riskv1.MethodClimateDisclosure), InputManifestDigest: scope.InputDigest})
		if err != nil {
			return nil, nil, err
		}
		money := func(s string) *commonpb.Decimal {
			value, ok := dec.ParseProtoExact(s)
			if !ok {
				t.Fatal("invalid literal fixture money")
			}
			return value
		}
		observations, aggregate, err := sustainabilitypublish.Build(scope, []sustainabilitypublish.Input{{Holding: holding, MarketValue: money("100"), Revenue: money("10"), Enterprise: money("50"), ESGPresent: true}}, func() (string, error) { id, err := uuid.NewV7(); return id.String(), err })
		if err != nil {
			return nil, nil, err
		}
		return &regulatorypb.FilingRecordBody{Report: wire, TenantId: scope.Tenant, PortfolioId: scope.Portfolio, InputOrigin: sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED, InputManifest: manifest, InputManifestDigest: scope.InputDigest, ProducerVersion: "synthetic-proof", FilingCanonical: report.Canonical(), RecordedAt: timestamppb.New(scope.RecordedAt), InstrumentObservations: observations, PortfolioObservation: aggregate, ClimateScenario: &sustainabilitypb.ClimateScenario{Name: input.Scenario.Name, CarbonPrice: input.Scenario.CarbonPrice, PhysicalSeverity: input.Scenario.PhysicalSeverity}}, report.Body(), nil
	}
	if _, err := p.Record(f.ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Record(f.ctx, plan); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || f.counts(t) != (filingCounts{Links: 2, Artifacts: 1, Identities: 4, Outbox: 4}) {
		t.Fatal("sustainability/report transaction or retry inventory changed")
	}
	info, err := f.stream.Info(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 4 {
		t.Fatal("deleted publish or reused type identity lost a real broker FACT")
	}
	ids := make(map[string]bool)
	subjects := make(map[string]int)
	for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq; sequence++ {
		message, err := f.stream.GetMsg(f.ctx, sequence)
		if err != nil {
			t.Fatal("mandatory filing FACT missing")
		}
		envelope, raw, err := bus.Unframe(message.Data)
		if err != nil || bus.Validate(envelope) != nil || ids[envelope.GetEventId()] || envelope.GetTenantId() != "acme" {
			t.Fatal("actual filing FACT identity/scope invalid")
		}
		ids[envelope.EventId] = true
		subjects[message.Subject]++
		if message.Subject == sustainabilitypublish.AggregatedSubject {
			var aggregate sustainabilitypb.PortfolioAggregated
			if proto.Unmarshal(raw, &aggregate) != nil || aggregate.Observation.WeightedAverageCarbonIntensity != 0.2 || aggregate.Observation.FinancedEmissions != 8 || aggregate.Observation.Esg.Overall != 70 {
				t.Fatal("actual broker aggregate differs from independent synthetic calculation")
			}
		}
	}
	if subjects[sustainabilitypublish.ObservedSubject] != 2 || subjects[sustainabilitypublish.AggregatedSubject] != 1 || subjects[regulatorypublish.RecordedSubject] != 1 {
		t.Fatal("mandatory ESG/carbon/aggregate/report publications missing")
	}
}

func fixtureHTTP(t *testing.T, f *recordingFixture, path, body, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	readiness := &server.Readiness{}
	readiness.Set(true)
	api := server.New(readiness, nil, server.WithRecorder("acme", "synthetic-http", f.recorder(t, "acme", "chain", f.publisher)))
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(f.ctx)
	if tenant != "" {
		auth.SetPrincipalHeaders(request.Header, "fixture-principal-not-input", tenant, []string{"compliance"})
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	return response
}

func TestPublishRegulatoryHTTPExactMoneyAndPreEffectRefusals(t *testing.T) {
	f := newRecordingFixture(t)
	const body = `{"portfolio_id":"pf1","as_of":"2026-06-30T00:00:00Z","GrossAssetValue":"9007199254740993","NetAssetValue":"9007199254740992","VaR":"1","GrossExposure":"9007199254740994"}`
	first := fixtureHTTP(t, f, "/v1/filings/formpf", body, "acme")
	if first.Code != http.StatusOK {
		t.Fatalf("actual filing HTTP→PG→broker request refused: status=%d", first.Code)
	}
	second := fixtureHTTP(t, f, "/v1/filings/formpf", body, "acme")
	if second.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("actual HTTP retry changed the committed artifact")
	}
	before := f.counts(t)
	if before != (filingCounts{Links: 2, Artifacts: 1, Identities: 1, Outbox: 1}) {
		t.Fatalf("unexpected real filing effects: %+v", before)
	}
	info, err := f.stream.Info(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 {
		t.Fatal("actual HTTP publication missing/duplicated")
	}
	message, err := f.stream.GetMsg(f.ctx, info.State.LastSeq)
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := bus.Unframe(message.Data)
	if err != nil {
		t.Fatal(err)
	}
	var observed regulatorypb.FilingRecorded
	if proto.Unmarshal(raw, &observed) != nil || observed.Body.Report.Framework != regulatorypb.Framework_FRAMEWORK_FORM_PF {
		t.Fatal("actual report wire shape missing")
	}
	for _, line := range observed.Body.Report.LineItems {
		if line.Code == "FORM_PF_GROSS_NAV" {
			value, ok := dec.FromProtoChecked(line.Value)
			want, _ := new(big.Rat).SetString("9007199254740993")
			if !ok || value.Cmp(want) != 0 {
				t.Fatal("source exact money rounded through HTTP/FACT bridge")
			}
		}
	}
	if bytes.Contains(observed.Body.InputManifest, []byte("fixture-principal-not-input")) || bytes.Contains(observed.Body.InputManifest, []byte("X-Kanz-Principal")) {
		t.Fatal("input manifest retained principal headers")
	}
	for _, tc := range []struct {
		name, body, tenant string
		status             int
	}{
		{"missing-principal", body, "", http.StatusUnauthorized},
		{"foreign-principal", body, "foreign", http.StatusNotFound},
		{"missing-scope", strings.Replace(body, `"portfolio_id":"pf1",`, "", 1), "acme", http.StatusBadRequest},
		{"missing-asof", strings.Replace(body, `"as_of":"2026-06-30T00:00:00Z",`, "", 1), "acme", http.StatusBadRequest},
		{"unknown-field", strings.TrimSuffix(body, "}") + `,"access_token":"public-negative-canary"}`, "acme", http.StatusBadRequest},
		{"duplicate-case", strings.TrimSuffix(body, "}") + `,"grossassetvalue":"1"}`, "acme", http.StatusBadRequest},
		{"unbounded-exponent", strings.Replace(body, `"VaR":"1"`, `"VaR":"1e999999999"`, 1), "acme", http.StatusBadRequest},
		{"nondecimal", strings.Replace(body, `"VaR":"1"`, `"VaR":"1/3"`, 1), "acme", http.StatusBadRequest},
		{"trailing", body + ` {}`, "acme", http.StatusBadRequest},
		{"numeric-null", strings.Replace(body, `"VaR":"1"`, `"VaR":null`, 1), "acme", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := fixtureHTTP(t, f, "/v1/filings/formpf", tc.body, tc.tenant)
			if response.Code != tc.status || f.counts(t) != before {
				t.Fatalf("pre-effect refusal changed: status=%d want=%d", response.Code, tc.status)
			}
		})
	}
	info, err = f.stream.Info(f.ctx)
	if err != nil || info.State.Msgs != 1 {
		t.Fatal("refused HTTP input published a FACT")
	}
}

func TestPublishSustainabilityHTTPComputedRequestObservations(t *testing.T) {
	f := newRecordingFixture(t)
	const body = `{"portfolio_id":"pf1","as_of":"2026-06-30T00:00:00Z","Holdings":[{"InstrumentID":"synthetic","MarketValue":100,"ESG":{"Overall":70,"Environmental":60,"Social":80,"Governance":90},"Carbon":{"Scope1":1,"Scope2":1,"Scope3":2,"Revenue":10,"EVIC":50}}],"GlidePath":{"BaseYear":2020,"TargetYear":2050,"BaseEmissions":100},"Year":2030,"Scenario":{"Name":"synthetic-request","CarbonPrice":1,"PhysicalSeverity":0.1},"TempSensitivity":2,"FossilFuel":{"synthetic":false}}`
	for _, framework := range []string{"tcfd", "sfdr"} {
		response := fixtureHTTP(t, f, "/v1/filings/"+framework, body, "acme")
		if response.Code != http.StatusOK {
			t.Fatalf("actual climate HTTP→PG→broker request refused: framework=%s status=%d", framework, response.Code)
		}
		retry := fixtureHTTP(t, f, "/v1/filings/"+framework, body, "acme")
		if retry.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), retry.Body.Bytes()) {
			t.Fatal("climate HTTP retry recomputed/re-identified its artifact")
		}
	}
	if f.counts(t) != (filingCounts{Links: 4, Artifacts: 2, Identities: 8, Outbox: 8}) {
		t.Fatal("climate filing did not commit every distinct ESG/carbon/aggregate/report identity")
	}
	info, err := f.stream.Info(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 8 {
		t.Fatal("actual climate FACT set incomplete")
	}
	ids := make(map[string]bool)
	subjects := make(map[string]int)
	for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq; sequence++ {
		message, err := f.stream.GetMsg(f.ctx, sequence)
		if err != nil {
			t.Fatal("actual climate FACT missing")
		}
		envelope, raw, err := bus.Unframe(message.Data)
		if err != nil || bus.Validate(envelope) != nil || ids[envelope.GetEventId()] || envelope.GetTenantId() != "acme" {
			t.Fatal("actual climate identity/scope invalid")
		}
		ids[envelope.EventId] = true
		subjects[message.Subject]++
		if message.Subject == sustainabilitypublish.AggregatedSubject {
			var aggregate sustainabilitypb.PortfolioAggregated
			if proto.Unmarshal(raw, &aggregate) != nil || aggregate.Observation.WeightedAverageCarbonIntensity != 0.2 || aggregate.Observation.FinancedEmissions != 8 || aggregate.Observation.Esg.Overall != 70 || aggregate.Observation.Origin != sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED {
				t.Fatal("actual request computation/provenance changed")
			}
		}
	}
	if subjects[sustainabilitypublish.ObservedSubject] != 4 || subjects[sustainabilitypublish.AggregatedSubject] != 2 || subjects[regulatorypublish.RecordedSubject] != 2 {
		t.Fatal("distinct climate publication types did not all reach the broker")
	}
	before := f.counts(t)
	for _, invalid := range []string{strings.Replace(body, `"Social":80,`, "", 1), strings.Replace(body, `"MarketValue":100`, `"MarketValue":null`, 1), strings.Replace(body, `"Revenue":10`, `"Revenue":1e99`, 1), strings.Replace(body, `"InstrumentID":"synthetic"`, `"InstrumentID":""`, 1)} {
		response := fixtureHTTP(t, f, "/v1/filings/tcfd", invalid, "acme")
		if response.Code != http.StatusBadRequest || f.counts(t) != before {
			t.Fatal("invalid climate input acquired signing/recording effects")
		}
	}
}
