package foldshadow

// Test-only qualification: a fully bound Rust candidate enters the unchanged Go
// transaction. No runtime path uses a worker result to mutate financial state.
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/shadowprocess"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	omspb "github.com/eighred/kanz/kanz-schemas-go/oms/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type transactionFixture struct {
	t      *testing.T
	ctx    context.Context
	schema string
	tenant string
	pool   *pgxpool.Pool
	store  *order.Postgres
	config Config
}

func newTransactionFixture(t *testing.T) *transactionFixture {
	t.Helper()
	bin := os.Getenv("KANZ_OMS_FOLD_BIN")
	if bin == "" {
		t.Skip("actual Rust binary unavailable; set KANZ_OMS_FOLD_BIN")
	}
	c := Config{Enabled: true, Executable: bin, ExpectedVersion: os.Getenv("KANZ_OMS_FOLD_VERSION"), Timeout: 10 * time.Second}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("set TEST_POSTGRES_URL for real transaction qualification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, os.Getenv("TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatal("database configuration unavailable")
	}
	t.Cleanup(admin.Close)
	var super, bypass bool
	if err = admin.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal("cannot inspect database role")
	}
	if super || bypass {
		t.Fatal("qualification requires NOSUPERUSER NOBYPASSRLS")
	}
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "oms_rust_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("create isolated schema")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("remove isolated schema")
		}
	})
	f := &transactionFixture{t: t, ctx: ctx, schema: schema, tenant: "rust-tenant-a", config: c}
	f.pool = f.open(f.tenant)
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatal("missing canonical migrations")
	}
	sort.Strings(files)
	for _, file := range files {
		ddl, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("migration %s failed: %v", filepath.Base(file), err)
		}
	}
	var forced int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relname IN ('orders','order_fills','order_executions','outbox') AND relrowsecurity AND relforcerowsecurity`).Scan(&forced); err != nil || forced != 4 {
		t.Fatal("all four durable owners require FORCE RLS")
	}
	f.store = order.NewPostgres(f.pool)
	return f
}

func (f *transactionFixture) open(tenant string) *pgxpool.Pool {
	f.t.Helper()
	c, err := pgxpool.ParseConfig(os.Getenv("TEST_POSTGRES_URL"))
	if err != nil {
		f.t.Fatal("database configuration unavailable")
	}
	c.ConnConfig.RuntimeParams["search_path"] = f.schema
	c.ConnConfig.RuntimeParams["app.tenant_id"] = tenant
	p, err := pgxpool.NewWithConfig(f.ctx, c)
	if err != nil {
		f.t.Fatal("open scoped pool")
	}
	f.t.Cleanup(p.Close)
	return p
}

func (f *transactionFixture) seed() (*orderpb.OrderState, int64) {
	f.t.Helper()
	s := &orderpb.OrderState{OrderId: "rust-order", PortfolioId: f.tenant + "-portfolio", Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED, VenueAccountId: "fixture-account", OrderedQuantity: &commonpb.Decimal{Coefficient: 10}, LeavesQuantity: &commonpb.Decimal{Coefficient: 10}}
	if err := f.store.Create(f.ctx, s, nil); err != nil {
		f.t.Fatal(err)
	}
	_, version, err := f.store.Load(f.ctx, s.OrderId)
	if err != nil {
		f.t.Fatal(err)
	}
	// A routed order has already advanced beyond initial admission. The worker
	// explicitly refuses missing/zero expected versions.
	if err = f.store.Save(f.ctx, s, version, nil, ""); err != nil {
		f.t.Fatal(err)
	}
	return s, version + 1
}

func transactionFill() *orderpb.Fill {
	return &orderpb.Fill{FillId: "fixture-execution", OrderId: "rust-order", Venue: "BINANCE", VenueAccountId: "fixture-account", InstrumentId: "BTC-USD", VenueExecutionId: "fixture-execution", Quantity: &commonpb.Decimal{Coefficient: 2}, Price: &commonpb.Decimal{Coefficient: 125}, ExecutedAt: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 123, time.UTC))}
}

// Accept only complete, recursively known, fully bound OK results with exact Go
// state parity. A matched refusal must never become a writable candidate.
func transactionCandidate(j *omspb.FillFoldRequest, capture order.FillFoldCapture, r *omspb.FillFoldResult, version string) (*orderpb.OrderState, error) {
	if r == nil || r.Status != omspb.FillFoldStatus_FILL_FOLD_STATUS_OK || compare(j, capture, r, version) != "matched" {
		return nil, errors.New("candidate refused")
	}
	return proto.Clone(r.Candidate).(*orderpb.OrderState), nil
}

func (f *transactionFixture) candidate(s *orderpb.OrderState, version int64, fill *orderpb.Fill) *orderpb.OrderState {
	f.t.Helper()
	return f.fold(s, version, fill).candidate
}

type transactionObservation struct {
	candidate *orderpb.OrderState
	request   *omspb.FillFoldRequest
	capture   order.FillFoldCapture
	response  *omspb.FillFoldResult
}

func (f *transactionFixture) fold(s *orderpb.OrderState, version int64, fill *orderpb.Fill) transactionObservation {
	f.t.Helper()
	at := fill.ExecutedAt.AsTime()
	expected, err := order.ApplyFill(s, fill, at)
	if err != nil {
		f.t.Fatal(err)
	}
	capture := order.FillFoldCapture{Tenant: f.tenant, Path: "live", ExpectedVersion: version, EventTime: at, CapturedAt: at.Add(time.Second), State: s, Fill: fill, Candidate: expected}
	j, err := job(capture, capture.Tenant, f.config.ExpectedVersion)
	if err != nil {
		f.t.Fatal(err)
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(j)
	if err != nil {
		f.t.Fatal(err)
	}
	raw, status := shadowprocess.Run(f.ctx, shadowprocess.Config{Executable: f.config.Executable, Timeout: f.config.Timeout}, wire)
	if status != "" {
		f.t.Fatalf("actual worker: %s", status)
	}
	r := new(omspb.FillFoldResult)
	if err = proto.Unmarshal(raw, r); err != nil {
		f.t.Fatal("invalid worker protobuf")
	}
	candidate, err := transactionCandidate(j, capture, r, f.config.ExpectedVersion)
	if err != nil {
		f.t.Fatal(err)
	}
	// Independent analytic fixture: 0 + 2 units at 125 leaves 8 of 10, at
	// exactly the execution timestamp. The wider corpus verifies rounding.
	if dec.Cmp(candidate.FilledQuantity, &commonpb.Decimal{Coefficient: 2}) != 0 || dec.Cmp(candidate.LeavesQuantity, &commonpb.Decimal{Coefficient: 8}) != 0 || dec.Cmp(candidate.AverageFillPrice, &commonpb.Decimal{Coefficient: 125}) != 0 || candidate.Status != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED || !proto.Equal(candidate.AsOf, fill.ExecutedAt) {
		f.t.Fatal("independent transaction fixture arithmetic/time mismatch")
	}
	return transactionObservation{candidate: candidate, request: j, capture: capture, response: r}
}

func (f *transactionFixture) facts(s *orderpb.OrderState, fill *orderpb.Fill) []outbox.Record {
	f.t.Helper()
	r, err := order.NewEmitter(nil).FillFact(bus.WithTenantID(f.ctx, f.tenant), fill, s)
	if err != nil {
		f.t.Fatal(err)
	}
	return []outbox.Record{r}
}

func (f *transactionFixture) assertDurable(pool *pgxpool.Pool, want *orderpb.OrderState, version int64, count int) {
	f.t.Helper()
	got, v, err := order.NewPostgres(pool).Load(f.ctx, want.OrderId)
	if err != nil || v != version || !proto.Equal(got, want) {
		f.t.Fatalf("durable state/version mismatch: version=%d error=%v", v, err)
	}
	for _, table := range []string{"order_fills", "order_executions", "outbox"} {
		column := "order_id"
		if table == "outbox" {
			column = "partition_key"
		}
		var n int
		if err = pool.QueryRow(f.ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()+" WHERE "+column+"=$1", want.OrderId).Scan(&n); err != nil || n != count {
			f.t.Fatalf("%s count=%d want=%d error=%v", table, n, count, err)
		}
	}
}

func TestRustOMSPostgresTransactions(t *testing.T) {
	t.Run("durable-restart-duplicate-conflict", func(t *testing.T) {
		f := newTransactionFixture(t)
		s, v := f.seed()
		fill := transactionFill()
		candidate := f.candidate(s, v, fill)
		facts := f.facts(candidate, fill)
		if err := f.store.Save(f.ctx, candidate, v, facts, fill.FillId); err != nil {
			t.Fatal(err)
		}
		// Drop every original connection; a new store/pool must reconstruct all
		// durable owners, rather than merely reading a retained in-process value.
		f.pool.Close()
		fresh := f.open("rust-tenant-a")
		store := order.NewPostgres(fresh)
		f.assertDurable(fresh, candidate, v+1, 1)
		if err := store.Save(f.ctx, candidate, v+1, facts, fill.FillId); !errors.Is(err, order.ErrFillApplied) {
			t.Fatalf("duplicate: %v", err)
		}
		changed := proto.Clone(fill).(*orderpb.Fill)
		changed.Price.Coefficient++
		if err := store.Save(f.ctx, candidate, v+1, f.facts(candidate, changed), changed.FillId); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) {
			t.Fatalf("conflicting execution: %v", err)
		}
		f.assertDurable(fresh, candidate, v+1, 1)
		var raw []byte
		if err := fresh.QueryRow(f.ctx, `SELECT fill FROM order_executions WHERE order_id=$1`, s.OrderId).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		persisted := new(orderpb.Fill)
		if proto.Unmarshal(raw, persisted) != nil || !proto.Equal(persisted, fill) {
			t.Fatal("journal changed execution")
		}
		if err := fresh.QueryRow(f.ctx, `SELECT payload FROM outbox WHERE partition_key=$1`, s.OrderId).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		fact := new(orderpb.OrderPartiallyFilled)
		if proto.Unmarshal(raw, fact) != nil || !proto.Equal(fact.Fill, fill) || !proto.Equal(fact.State, candidate) {
			t.Fatal("outbox fact differs from committed candidate/execution")
		}
	})
	t.Run("concurrent-duplicate", func(t *testing.T) {
		f := newTransactionFixture(t)
		s, v := f.seed()
		fill := transactionFill()
		candidate := f.candidate(s, v, fill)
		facts := f.facts(candidate, fill)
		stores := []*order.Postgres{f.store, order.NewPostgres(f.open("rust-tenant-a"))}
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, store := range stores {
			wg.Go(func() { <-start; results <- store.Save(f.ctx, candidate, v, facts, fill.FillId) })
		}
		close(start)
		wg.Wait()
		close(results)
		wins, duplicates := 0, 0
		for err := range results {
			if err == nil {
				wins++
			} else if errors.Is(err, order.ErrFillApplied) {
				duplicates++
			} else {
				t.Fatalf("unexpected race refusal: %v", err)
			}
		}
		if wins != 1 || duplicates != 1 {
			t.Fatalf("wins=%d duplicates=%d", wins, duplicates)
		}
		f.assertDurable(f.pool, candidate, v+1, 1)
	})
	t.Run("cancel-wins-stale-fill", func(t *testing.T) {
		f := newTransactionFixture(t)
		s, v := f.seed()
		fill := transactionFill()
		candidate := f.candidate(s, v, fill)
		cancelled, _, err := order.Cancel(s, fill.ExecutedAt.AsTime().Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := order.NewPostgres(f.open("rust-tenant-a")).Save(f.ctx, cancelled, v, nil, ""); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Save(f.ctx, candidate, v, f.facts(candidate, fill), fill.FillId); !errors.Is(err, order.ErrConflict) {
			t.Fatalf("stale fill: %v", err)
		}
		f.assertDurable(f.pool, cancelled, v+1, 0)
	})
	t.Run("fill-wins-stale-cancel", func(t *testing.T) {
		f := newTransactionFixture(t)
		s, v := f.seed()
		fill := transactionFill()
		candidate := f.candidate(s, v, fill)
		if err := f.store.Save(f.ctx, candidate, v, f.facts(candidate, fill), fill.FillId); err != nil {
			t.Fatal(err)
		}
		cancelled, _, err := order.Cancel(s, fill.ExecutedAt.AsTime().Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := order.NewPostgres(f.open("rust-tenant-a")).Save(f.ctx, cancelled, v, nil, ""); !errors.Is(err, order.ErrConflict) {
			t.Fatalf("stale cancel: %v", err)
		}
		f.assertDurable(f.pool, candidate, v+1, 1)
	})
	t.Run("commit-failure-rolls-back-all-owners", func(t *testing.T) {
		f := newTransactionFixture(t)
		s, v := f.seed()
		fill := transactionFill()
		candidate := f.candidate(s, v, fill)
		_, err := f.pool.Exec(f.ctx, `CREATE FUNCTION rust_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rust fixture deferred failure'; END $$; CREATE CONSTRAINT TRIGGER rust_fail_commit AFTER UPDATE ON orders DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION rust_fail_commit()`)
		if err != nil {
			t.Fatal(err)
		}
		err = f.store.Save(f.ctx, candidate, v, f.facts(candidate, fill), fill.FillId)
		if err == nil || !strings.Contains(err.Error(), "rust fixture deferred failure") {
			t.Fatalf("did not reach deferred commit failure: %v", err)
		}
		f.assertDurable(f.pool, s, v, 0)
		if _, err = f.pool.Exec(f.ctx, `DROP TRIGGER rust_fail_commit ON orders`); err != nil {
			t.Fatal(err)
		}
		if err = f.store.Save(f.ctx, candidate, v, f.facts(candidate, fill), fill.FillId); err != nil {
			t.Fatal("rollback left fill unclaimable")
		}
		f.assertDurable(f.pool, candidate, v+1, 1)
	})
	t.Run("tenant-isolation", func(t *testing.T) {
		f := newTransactionFixture(t)
		s, v := f.seed()
		fill := transactionFill()
		afold := f.fold(s, v, fill)
		candidate := afold.candidate
		if err := f.store.Save(f.ctx, candidate, v, f.facts(candidate, fill), fill.FillId); err != nil {
			t.Fatal(err)
		}
		other := f.open("rust-tenant-b")
		otherStore := order.NewPostgres(other)
		if _, _, err := otherStore.Load(f.ctx, s.OrderId); !errors.Is(err, order.ErrNotFound) {
			t.Fatalf("cross-tenant discovery: %v", err)
		}
		for _, table := range []string{"orders", "order_fills", "order_executions", "outbox"} {
			var n int
			if err := other.QueryRow(f.ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil || n != 0 {
				t.Fatalf("cross-tenant %s disclosure", table)
			}
		}
		// Positive control: the second tenant must be able to use the identical
		// order/execution IDs, with its own fully bound Rust result and FACT.
		b := *f
		b.tenant = "rust-tenant-b"
		b.pool = other
		b.store = otherStore
		bs, bv := b.seed()
		bfold := b.fold(bs, bv, fill)
		bcandidate := bfold.candidate
		// Save is a trusted tenant-local upsert, not an identity authority. Its
		// envelope tenant can differ from its storage tenant (#223/#97). Refuse
		// the actual A response at the binding boundary before calling SQL.
		if got, err := transactionCandidate(bfold.request, bfold.capture, afold.response, b.config.ExpectedVersion); err == nil || got != nil {
			t.Fatal("actual A response accepted for B")
		}
		crossed := proto.Clone(bfold.response).(*omspb.FillFoldResult)
		crossed.Candidate = proto.Clone(candidate).(*orderpb.OrderState)
		if got, err := transactionCandidate(bfold.request, bfold.capture, crossed, b.config.ExpectedVersion); err == nil || got != nil {
			t.Fatal("A payload accepted with B response identity")
		}
		b.assertDurable(other, bs, bv, 0)
		f.assertDurable(f.pool, candidate, v+1, 1)
		if err := b.store.Save(b.ctx, bcandidate, bv, b.facts(bcandidate, fill), fill.FillId); err != nil {
			t.Fatal("tenant B positive control failed", err)
		}
		f.assertDurable(f.pool, candidate, v+1, 1)
		b.assertDurable(other, bcandidate, bv+1, 1)
		if proto.Equal(candidate, bcandidate) {
			t.Fatal("tenant controls must retain distinct portfolio attribution")
		}
		for _, fixture := range []*transactionFixture{f, &b} {
			var envelopeTenant string
			var raw []byte
			if err := fixture.pool.QueryRow(f.ctx, `SELECT envelope_tenant_id,payload FROM outbox WHERE partition_key=$1`, s.OrderId).Scan(&envelopeTenant, &raw); err != nil {
				t.Fatal(err)
			}
			fact := new(orderpb.OrderPartiallyFilled)
			if envelopeTenant != fixture.tenant || proto.Unmarshal(raw, fact) != nil || fact.GetState().GetPortfolioId() != fixture.tenant+"-portfolio" {
				t.Fatal("crossed tenant outbox envelope/payload")
			}
		}
	})
}

func TestRustTransactionCandidateBinding(t *testing.T) {
	c := fixture(t)
	j, err := job(c, "tenant", "source")
	if err != nil {
		t.Fatal(err)
	}
	r := response(j, c)
	got, err := transactionCandidate(j, c, r, "stamp")
	if err != nil || !proto.Equal(got, c.Candidate) {
		t.Fatal("valid candidate refused")
	}
	got.OrderId = "detached"
	if r.Candidate.OrderId == "detached" {
		t.Fatal("candidate aliases raw worker response")
	}
	mutations := map[string]func(*omspb.FillFoldResult){
		"capture":        func(r *omspb.FillFoldResult) { r.CaptureId = "other" },
		"digest":         func(r *omspb.FillFoldResult) { r.CaptureDigest = nil },
		"tenant":         func(r *omspb.FillFoldResult) { r.TenantId = "other" },
		"producer":       func(r *omspb.FillFoldResult) { r.ProducerVersion = "other" },
		"version":        func(r *omspb.FillFoldResult) { r.ExpectedVersion = proto.Int64(9) },
		"absent-version": func(r *omspb.FillFoldResult) { r.ExpectedVersion = nil },
		"state":          func(r *omspb.FillFoldResult) { r.Candidate.FilledQuantity.Coefficient++ },
		"nested-unknown": func(r *omspb.FillFoldResult) { r.Candidate.FilledQuantity.ProtoReflect().SetUnknown([]byte{0x78, 1}) },
		"unknown":        func(r *omspb.FillFoldResult) { r.ProtoReflect().SetUnknown([]byte{0x78, 1}) },
		"refusal": func(r *omspb.FillFoldResult) {
			r.Status = omspb.FillFoldStatus_FILL_FOLD_STATUS_REJECTED
			r.Candidate = nil
			r.RejectionCode = "OVERFILL"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := response(j, c)
			mutate(r)
			if got, err := transactionCandidate(j, c, r, "stamp"); err == nil || got != nil {
				t.Fatal("unbound candidate escaped")
			}
		})
	}
	refused := c
	refused.Candidate = nil
	refused.RejectionCode = "OVERFILL"
	r = response(j, c)
	r.Status = omspb.FillFoldStatus_FILL_FOLD_STATUS_REJECTED
	r.Candidate = nil
	r.RejectionCode = "OVERFILL"
	if compare(j, refused, r, "stamp") != "matched" {
		t.Fatal("refusal control is not matched")
	}
	if got, err := transactionCandidate(j, refused, r, "stamp"); err == nil || got != nil {
		t.Fatal("matched refusal became writable")
	}
}
