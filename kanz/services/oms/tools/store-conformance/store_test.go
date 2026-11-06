package storeconformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4096 {
		return 0, errors.New("fixture output exceeded bound")
	}
	return b.Buffer.Write(p)
}
func fixtureBinary(t *testing.T) string {
	t.Helper()
	p := os.Getenv("KANZ_OMS_STORE_BIN")
	if p == "" {
		t.Skip("actual Rust repository fixture unavailable")
	}
	if !filepath.IsAbs(p) || os.Getenv("KANZ_OMS_STORE_VERSION") == "" {
		t.Fatal("absolute stamped fixture required")
	}
	return p
}
func run(t *testing.T, input map[string]any) string {
	t.Helper()
	bin := fixtureBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.WaitDelay = time.Second
	cmd.Env = []string{"TEST_POSTGRES_URL=" + os.Getenv("TEST_POSTGRES_URL")}
	cmd.Dir = t.TempDir()
	cmd.Stdin = bytes.NewReader(encodedJSON(t, input))
	var stdout, stderr boundedOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatal("actual Rust repository fixture failed (no connection or payload diagnostics)")
	}
	var result struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	decoder := json.NewDecoder(&stdout)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Version != os.Getenv("KANZ_OMS_STORE_VERSION") {
		t.Fatal("unbound fixture result")
	}
	return result.Status
}
func wire(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, e := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestRustExecutionIdentity(t *testing.T) {
	fixtureBinary(t)
	for _, c := range identityCases(t) {
		t.Run(c.name, func(t *testing.T) {
			if run(t, map[string]any{"mode": "identity", "fill": wire(t, c.fill)}) != c.key {
				t.Fatal("execution key differs")
			}
		})
	}
}

type fixture struct {
	t              *testing.T
	ctx            context.Context
	schema, tenant string
	pool           *pgxpool.Pool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fixtureBinary(t)
	if os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("real non-bypass PostgreSQL unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	admin, e := pgxpool.New(ctx, os.Getenv("TEST_POSTGRES_URL"))
	if e != nil {
		t.Fatal("database configuration")
	}
	t.Cleanup(admin.Close)
	var super, bypass bool
	if e = admin.QueryRow(ctx, "SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&super, &bypass); e != nil || super || bypass {
		t.Fatal("NOSUPERUSER NOBYPASSRLS required")
	}
	var nonce [12]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		t.Fatal(e)
	}
	schema := "oms_store_" + hex.EncodeToString(nonce[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+quoted); e != nil {
		t.Fatal("create schema")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, e := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); e != nil {
			t.Error("cleanup isolated schema")
		}
	})
	f := &fixture{t: t, ctx: ctx, schema: schema, tenant: "tenant-a"}
	f.pool = f.open(f.tenant)
	files, e := filepath.Glob("../../migrations/*.sql")
	if e != nil || len(files) == 0 {
		t.Fatal("missing canonical migrations")
	}
	sort.Strings(files)
	for _, file := range files {
		ddl, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.pool.Exec(ctx, string(ddl)); e != nil {
			t.Fatalf("canonical migration %s: %v", filepath.Base(file), e)
		}
	}
	var forced int
	if e = f.pool.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relname IN ('orders','order_fills','order_executions','outbox','execution_fee_revisions') AND relrowsecurity AND relforcerowsecurity").Scan(&forced); e != nil || forced != 5 {
		t.Fatal("five forced RLS tables required")
	}
	return f
}
func (f *fixture) open(tenant string) *pgxpool.Pool {
	f.t.Helper()
	cfg, e := pgxpool.ParseConfig(os.Getenv("TEST_POSTGRES_URL"))
	if e != nil {
		f.t.Fatal("database configuration")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = f.schema
	cfg.ConnConfig.RuntimeParams["app.tenant_id"] = tenant
	p, e := pgxpool.NewWithConfig(f.ctx, cfg)
	if e != nil {
		f.t.Fatal("scoped pool")
	}
	f.t.Cleanup(p.Close)
	return p
}

var at = time.Date(2026, 1, 1, 0, 0, 0, 123000, time.UTC)

func (f *fixture) seed() (*orderpb.OrderState, int64) {
	f.t.Helper()
	s := &orderpb.OrderState{OrderId: "order", PortfolioId: f.tenant + "-portfolio", Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED, OrderedQuantity: &commonpb.Decimal{Coefficient: 10}, LeavesQuantity: &commonpb.Decimal{Coefficient: 100, Exponent: -1}}
	store := order.NewPostgres(f.pool)
	if e := store.Create(f.ctx, s, nil); e != nil {
		f.t.Fatal(e)
	}
	_, v, e := store.Load(f.ctx, s.OrderId)
	if e != nil {
		f.t.Fatal(e)
	}
	return s, v
}
func fill() *orderpb.Fill {
	return &orderpb.Fill{FillId: "alias", OrderId: "order", InstrumentId: "instrument", Venue: "VENUE", VenueAccountId: "account", VenueExecutionId: "execution", Side: orderpb.Side_SIDE_BUY, Quantity: &commonpb.Decimal{Coefficient: 2}, Price: &commonpb.Decimal{Coefficient: 12500, Exponent: -2}, Fee: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1}}, ExecutedAt: timestamppb.New(at)}
}
func factJSON(r outbox.Record) map[string]any {
	return map[string]any{"tenant": r.TenantID, "partition_key": r.PartitionKey, "subject": r.Subject, "event_type": r.EventType, "event_class": int32(r.EventClass), "schema_version": r.SchemaVersion, "domain": r.Domain, "payload_schema_ref": r.PayloadSchemaRef, "event_seconds": r.EventTime.Unix(), "event_nanos": r.EventTime.Nanosecond(), "correlation_id": r.CorrelationID, "causation_id": r.CausationID, "trace_context": r.TraceContext, "payload": r.Payload}
}
func (f *fixture) request(s *orderpb.OrderState, v int64, fill *orderpb.Fill) map[string]any {
	f.t.Helper()
	records := []map[string]any{}
	id := ""
	if fill != nil {
		r, e := order.NewEmitter(nil).FillFact(bus.WithTenantID(f.ctx, f.tenant), fill, s)
		if e != nil {
			f.t.Fatal(e)
		}
		records = append(records, factJSON(r))
		id = fill.FillId
	}
	return map[string]any{"mode": "save", "schema": f.schema, "tenant": f.tenant, "state": wire(f.t, s), "expected_version": v, "fill_id": id, "records": records}
}
func (f *fixture) candidate(s *orderpb.OrderState, fill *orderpb.Fill) *orderpb.OrderState {
	f.t.Helper()
	n, e := order.ApplyFill(s, fill, at)
	if e != nil {
		f.t.Fatal(e)
	}
	return n
}
func (f *fixture) assert(s *orderpb.OrderState, v int64, n int) {
	f.t.Helper()
	got, version, e := order.NewPostgres(f.pool).Load(f.ctx, s.OrderId)
	if e != nil || v != version || !proto.Equal(got, s) {
		f.t.Fatal("independent Go durable state/version differs")
	}
	for _, table := range []string{"order_fills", "order_executions", "outbox"} {
		var count int
		if e = f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&count); e != nil || count != n {
			f.t.Fatalf("%s count=%d expected=%d", table, count, n)
		}
	}
}
func expect(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("closed result %s want %s", got, want)
	}
}

func TestRustRepositoryTransactions(t *testing.T) {
	t.Run("unowned-outbox-refusal", func(t *testing.T) {
		f := newFixture(t)
		s, v := f.seed()
		x := fill()
		n := f.candidate(s, x)
		req := f.request(n, v, x)
		// Reproduce a pre-owner-migration row, then restore the real NOT VALID
		// constraint. Do not weaken the library's RLS/owner checks.
		_, e := f.pool.Exec(f.ctx, `ALTER TABLE outbox DROP CONSTRAINT outbox_producer_service_required;
		INSERT INTO outbox (envelope_tenant_id,partition_key,subject,event_type,event_class,schema_version,domain,payload_schema_ref,event_time,correlation_id,causation_id,trace_context,payload)
		VALUES ('tenant-a','legacy','legacy','legacy',1,1,'order','legacy',now(),'','','',''::bytea);
		ALTER TABLE outbox ADD CONSTRAINT outbox_producer_service_required CHECK (producer_service IS NOT NULL AND producer_service<>'' AND producer_service=btrim(producer_service)) NOT VALID`)
		if e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, req), "UnownedOutbox")
		var claims int
		if e = f.pool.QueryRow(f.ctx, "SELECT count(*) FROM order_fills").Scan(&claims); e != nil || claims != 0 {
			t.Fatal("unowned outbox retained claim")
		}
		got, version, e := order.NewPostgres(f.pool).Load(f.ctx, s.OrderId)
		if e != nil || version != v || !proto.Equal(got, s) {
			t.Fatal("unowned outbox changed order")
		}
	})
	t.Run("unsafe-rls-and-invalid-record", func(t *testing.T) {
		f := newFixture(t)
		s, v := f.seed()
		x := fill()
		n := f.candidate(s, x)
		req := f.request(n, v, x)
		if _, e := f.pool.Exec(f.ctx, "ALTER TABLE orders NO FORCE ROW LEVEL SECURITY"); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, req), "UnsafeRole")
		if _, e := f.pool.Exec(f.ctx, "ALTER TABLE orders FORCE ROW LEVEL SECURITY"); e != nil {
			t.Fatal(e)
		}
		req["records"].([]map[string]any)[0]["tenant"] = ""
		expect(t, run(t, req), "InvalidInput")
		f.assert(s, v, 0)
	})
	t.Run("durable-restart-exact-bytes", func(t *testing.T) {
		f := newFixture(t)
		s, v := f.seed()
		x := fill()
		x.ProtoReflect().SetUnknown([]byte{0xf8, 0x3e, 1})
		n := f.candidate(s, x)
		n.ProtoReflect().SetUnknown([]byte{0xf8, 0x3e, 1})
		req := f.request(n, v, x)
		expect(t, run(t, req), "committed")
		f.pool.Close()
		f.pool = f.open(f.tenant)
		f.assert(n, v+1, 1)
		var state, payload, journal []byte
		if e := f.pool.QueryRow(f.ctx, "SELECT state FROM orders WHERE order_id='order'").Scan(&state); e != nil {
			t.Fatal(e)
		}
		if e := f.pool.QueryRow(f.ctx, "SELECT payload FROM outbox").Scan(&payload); e != nil {
			t.Fatal(e)
		}
		if e := f.pool.QueryRow(f.ctx, "SELECT fill FROM order_executions").Scan(&journal); e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(state, req["state"].([]byte)) || !bytes.Equal(payload, req["records"].([]map[string]any)[0]["payload"].([]byte)) || !bytes.Equal(journal, wire(t, x)) {
			t.Fatal("raw canonical bytes changed")
		}
		expect(t, run(t, req), "FillApplied")
		f.assert(n, v+1, 1)
	})
	t.Run("duplicate-conflict-and-alias", func(t *testing.T) {
		f := newFixture(t)
		s, v := f.seed()
		x := fill()
		n := f.candidate(s, x)
		expect(t, run(t, f.request(n, v, x)), "committed")
		alias := proto.Clone(x).(*orderpb.Fill)
		alias.FillId = "other-alias"
		expect(t, run(t, f.request(n, v, alias)), "FillApplied")
		alias.Price.Coefficient++
		expect(t, run(t, f.request(n, v, alias)), "IdentityConflict")
		f.assert(n, v+1, 1)
	})
	t.Run("concurrent-duplicate", func(t *testing.T) {
		f := newFixture(t)
		s, v := f.seed()
		x := fill()
		n := f.candidate(s, x)
		req := f.request(n, v, x)
		results := make(chan string, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); results <- run(t, req) }()
		}
		wg.Wait()
		close(results)
		var got []string
		for r := range results {
			got = append(got, r)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != "FillApplied,committed" {
			t.Fatal("not exactly one commit")
		}
		f.assert(n, v+1, 1)
	})
	for _, cancelFirst := range []bool{true, false} {
		name := "fill-wins-cancel"
		if cancelFirst {
			name = "cancel-wins-fill"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			s, v := f.seed()
			x := fill()
			n := f.candidate(s, x)
			cancelled, _, e := order.Cancel(s, at)
			if e != nil {
				t.Fatal(e)
			}
			a, b := f.request(n, v, x), f.request(cancelled, v, nil)
			want, count := n, 1
			if cancelFirst {
				a, b = b, a
				want, count = cancelled, 0
			}
			expect(t, run(t, a), "committed")
			expect(t, run(t, b), "Conflict")
			f.assert(want, v+1, count)
		})
	}
	t.Run("commit-failure-rollback-reclaim", func(t *testing.T) {
		f := newFixture(t)
		s, v := f.seed()
		x := fill()
		n := f.candidate(s, x)
		_, e := f.pool.Exec(f.ctx, `CREATE FUNCTION fixture_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture deferred failure'; END $$; CREATE CONSTRAINT TRIGGER fixture_fail_commit AFTER UPDATE ON orders DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fixture_fail_commit()`)
		if e != nil {
			t.Fatal(e)
		}
		req := f.request(n, v, x)
		expect(t, run(t, req), "CommitUnconfirmed")
		f.assert(s, v, 0)
		if _, e = f.pool.Exec(f.ctx, "DROP TRIGGER fixture_fail_commit ON orders"); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, req), "committed")
		f.assert(n, v+1, 1)
	})
	t.Run("tenant-and-missing-scope", func(t *testing.T) {
		a := newFixture(t)
		s, v := a.seed()
		x := fill()
		n := a.candidate(s, x)
		expect(t, run(t, a.request(n, v, x)), "committed")
		b := *a
		b.tenant = "tenant-b"
		b.pool = b.open(b.tenant)
		if _, _, e := order.NewPostgres(b.pool).Load(b.ctx, s.OrderId); !errors.Is(e, order.ErrNotFound) {
			t.Fatal("cross-tenant discovery")
		}
		bs, bv := b.seed()
		bn := b.candidate(bs, x)
		expect(t, run(t, b.request(bn, bv, x)), "committed")
		a.assert(n, v+1, 1)
		b.assert(bn, bv+1, 1)
		for _, f := range []*fixture{a, &b} {
			var tenant string
			if e := f.pool.QueryRow(f.ctx, "SELECT envelope_tenant_id FROM outbox").Scan(&tenant); e != nil || tenant != f.tenant {
				t.Fatal("envelope tenant mismatch")
			}
		}
		req := a.request(n, v, x)
		req["tenant"] = ""
		expect(t, run(t, req), "InvalidInput")
		a.assert(n, v+1, 1)
	})
	t.Run("legacy-alias", func(t *testing.T) {
		f := newFixture(t)
		s, v := f.seed()
		x := fill()
		if _, e := f.pool.Exec(f.ctx, "INSERT INTO order_fills (tenant_id,order_id,fill_id) VALUES (current_setting('app.tenant_id'),'order','alias')"); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, f.request(f.candidate(s, x), v, x)), "FillApplied")
		if _, e := f.pool.Exec(f.ctx, "INSERT INTO order_fills (tenant_id,order_id,fill_id) VALUES (current_setting('app.tenant_id'),'other','other-alias')"); e != nil {
			t.Fatal(e)
		}
		x.FillId = "other-alias"
		expect(t, run(t, f.request(f.candidate(s, x), v, x)), "IdentityConflict")
	})
	t.Run("fee-head-duplicate-and-corruption", func(t *testing.T) {
		f := newFixture(t)
		s, v := f.seed()
		x := fill()
		n := f.candidate(s, x)
		expect(t, run(t, f.request(n, v, x)), "committed")
		revised := proto.Clone(x).(*orderpb.Fill)
		revised.Fee.Amount.Coefficient = 2
		insert := func(id string, r *orderpb.Fill) {
			t.Helper()
			_, e := f.pool.Exec(f.ctx, "INSERT INTO execution_fee_revisions (book,execution_key,order_id,case_id,approval_digest,previous_fill,revised_fill) VALUES ('oms',$1,'order',$2,$3,$4,$5)", fillfact.ExecutionKey(x), id, strings.Repeat("a", 64), wire(t, x), wire(t, r))
			if e != nil {
				t.Fatal(e)
			}
		}
		insert("revision-1", revised)
		expect(t, run(t, f.request(n, v, revised)), "FillApplied")
		middle := proto.Clone(revised).(*orderpb.Fill)
		middle.Fee.Amount.Coefficient = 3
		insert("revision-2", middle)
		expect(t, run(t, f.request(n, v, revised)), "IdentityConflict")
		expect(t, run(t, f.request(n, v, middle)), "FillApplied")
		corrupt := proto.Clone(middle).(*orderpb.Fill)
		corrupt.Price.Coefficient++
		insert("bad-head", corrupt)
		expect(t, run(t, f.request(n, v, middle)), "FeeRevision")
		f.assert(n, v+1, 1)
	})
}
