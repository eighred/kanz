package storeconformance

import (
	"bytes"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
)

func (f *fixture) initial() *orderpb.OrderState {
	s := &orderpb.OrderState{OrderId: "order", PortfolioId: f.tenant + "-portfolio", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW, OrderedQuantity: &commonpb.Decimal{Coefficient: 100, Exponent: -1}, LeavesQuantity: &commonpb.Decimal{Coefficient: 10}}
	s.ProtoReflect().SetUnknown([]byte{0xf8, 0x3e, 1})
	return s
}
func (f *fixture) createRequest(s *orderpb.OrderState) map[string]any {
	f.t.Helper()
	req := f.request(s, 0, nil)
	req["mode"] = "create"
	fact, e := order.NewEmitter(nil).AcceptedFact(bus.WithTenantID(f.ctx, f.tenant), s)
	if e != nil {
		f.t.Fatal(e)
	}
	req["records"] = []map[string]any{factJSON(fact)}
	return req
}
func (f *fixture) readRequest(mode, id string) map[string]any {
	return map[string]any{"mode": mode, "schema": f.schema, "tenant": f.tenant, "order_id": id}
}
func (f *fixture) assertLoad(s *orderpb.OrderState, v int64) {
	f.t.Helper()
	r := runResult(f.t, f.readRequest("load", s.OrderId))
	if r.Status != "loaded" || r.DBVersion == nil || *r.DBVersion != v || !bytes.Equal(r.State, wire(f.t, s)) {
		f.t.Fatal("Rust single-row state/version/unknown bytes differ")
	}
	got, version, e := order.NewPostgres(f.pool).Load(f.ctx, s.OrderId)
	if e != nil || version != v || !proto.Equal(got, s) {
		f.t.Fatal("Go read compatibility differs")
	}
}
func (f *fixture) rowCounts(orders, outbox int) {
	f.t.Helper()
	for table, want := range map[string]int{"orders": orders, "outbox": outbox, "order_fills": 0, "order_executions": 0} {
		var n int
		if e := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != want {
			f.t.Fatalf("%s durable count differs", table)
		}
	}
}
func readError(t *testing.T, r fixtureResult, want string) {
	t.Helper()
	expect(t, r.Status, want)
	if r.State != nil || r.DBVersion != nil || r.Claims != nil || r.States != nil || r.Unindexed != nil {
		t.Fatal("read failure exposed a partial successful result")
	}
}

func TestRustRepositoryLifecycle(t *testing.T) {
	t.Run("read-oversized-bytes", func(t *testing.T) {
		f := newFixture(t)
		s, _ := f.seed()
		if _, e := f.pool.Exec(f.ctx, "UPDATE orders SET state=$1 WHERE order_id=$2", bytes.Repeat([]byte{0}, (1<<20)+1), s.OrderId); e != nil {
			t.Fatal(e)
		}
		readError(t, runResult(t, f.readRequest("load", s.OrderId)), "Capacity")
		// The canonical alias B-tree already refuses this single value. Preserve
		// that constraint; qualify aggregate read capacity with valid-sized rows.
		_, err := f.pool.Exec(f.ctx, `INSERT INTO order_fills (tenant_id,order_id,fill_id,raw_fill_id) VALUES (current_setting('app.tenant_id'),'row-too-large','oversized',repeat('x',1048577))`)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "54000" {
			t.Fatal("canonical alias index did not refuse oversized singleton")
		}
		for start := 1; start <= 2000; start += 500 {
			if _, e := f.pool.Exec(f.ctx, `INSERT INTO order_fills (tenant_id,order_id,fill_id,raw_fill_id) SELECT current_setting('app.tenant_id'),'aggregate','part-'||n,repeat('x',600)||n FROM generate_series($1::bigint,$2::bigint)n`, start, start+499); e != nil {
				t.Fatal(e)
			}
		}
		readError(t, runResult(t, f.readRequest("applied-fills", "aggregate")), "Capacity")
	})
	t.Run("load-concurrent-version", func(t *testing.T) {
		f := newFixture(t)
		s, v := f.seed()
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			store := order.NewPostgres(f.pool)
			for i := int64(1); i <= 100; i++ {
				next := proto.Clone(s).(*orderpb.OrderState)
				next.FilledQuantity = &commonpb.Decimal{Coefficient: i}
				if e := store.Save(f.ctx, next, v+i-1, nil, ""); e != nil {
					t.Error("concurrent Go fixture save failed")
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		for i := 0; i < 12; i++ {
			r := runResult(t, f.readRequest("load", s.OrderId))
			var observed orderpb.OrderState
			if r.Status != "loaded" || r.DBVersion == nil || proto.Unmarshal(r.State, &observed) != nil || observed.GetFilledQuantity().GetCoefficient() != *r.DBVersion-v {
				t.Fatal("state and version came from different writes")
			}
		}
		wg.Wait()
		<-done
	})
	t.Run("concurrent-create-one-fact", func(t *testing.T) {
		f := newFixture(t)
		s := f.initial()
		req := f.createRequest(s)
		var wg sync.WaitGroup
		results := make(chan string, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); results <- run(t, req) }()
		}
		wg.Wait()
		close(results)
		var actual []string
		for r := range results {
			actual = append(actual, r)
		}
		sort.Strings(actual)
		if !reflect.DeepEqual(actual, []string{"Exists", "committed"}) {
			t.Fatal("create did not have one winner")
		}
		f.assertLoad(s, 0)
		f.rowCounts(1, 1)
		different := proto.Clone(s).(*orderpb.OrderState)
		different.LeavesQuantity.Coefficient = 999
		expect(t, run(t, f.createRequest(different)), "Exists")
		f.assertLoad(s, 0)
		f.rowCounts(1, 1)
	})
	t.Run("create-restart-load-go-save", func(t *testing.T) {
		f := newFixture(t)
		s := f.initial()
		expect(t, run(t, f.createRequest(s)), "committed")
		f.pool.Close()
		f.pool = f.open(f.tenant)
		f.assertLoad(s, 0)
		next, _, e := order.Cancel(s, at)
		if e != nil {
			t.Fatal(e)
		}
		if e = order.NewPostgres(f.pool).Save(f.ctx, next, 0, nil, ""); e != nil {
			t.Fatal(e)
		}
		f.assertLoad(next, 1)
		f.rowCounts(1, 1)
		// Reverse direction: Go creates, Rust loads and saves, Go reads the same row.
		other := f.initial()
		other.OrderId = "go-created"
		if e = order.NewPostgres(f.pool).Create(f.ctx, other, nil); e != nil {
			t.Fatal(e)
		}
		f.assertLoad(other, 0)
		other.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
		expect(t, run(t, f.request(other, 0, nil)), "committed")
		f.assertLoad(other, 1)
	})
	t.Run("create-invalid-fact-rollback", func(t *testing.T) {
		f := newFixture(t)
		s := f.initial()
		req := f.createRequest(s)
		req["records"].([]map[string]any)[0]["tenant"] = ""
		expect(t, run(t, req), "InvalidInput")
		f.rowCounts(0, 0)
		readError(t, runResult(t, f.readRequest("load", s.OrderId)), "NotFound")
		expect(t, run(t, f.createRequest(s)), "committed")
		f.rowCounts(1, 1)
	})
	t.Run("create-deferred-commit-rollback", func(t *testing.T) {
		f := newFixture(t)
		s := f.initial()
		_, e := f.pool.Exec(f.ctx, `CREATE FUNCTION create_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture deferred failure'; END $$; CREATE CONSTRAINT TRIGGER create_fail_commit AFTER INSERT ON orders DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION create_fail_commit()`)
		if e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, f.createRequest(s)), "CommitUnconfirmed")
		f.rowCounts(0, 0)
		if _, e = f.pool.Exec(f.ctx, "DROP TRIGGER create_fail_commit ON orders"); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, f.createRequest(s)), "committed")
		f.assertLoad(s, 0)
		f.rowCounts(1, 1)
	})
	t.Run("create-tenant-same-id", func(t *testing.T) {
		a := newFixture(t)
		s := a.initial()
		expect(t, run(t, a.createRequest(s)), "committed")
		b := *a
		b.tenant = "tenant-b"
		b.pool = b.open(b.tenant)
		readError(t, runResult(t, b.readRequest("load", s.OrderId)), "NotFound")
		bs := b.initial()
		expect(t, run(t, b.createRequest(bs)), "committed")
		a.assertLoad(s, 0)
		b.assertLoad(bs, 0)
		a.rowCounts(1, 1)
		b.rowCounts(1, 1)
	})
	t.Run("applied-fill-raw-aliases", func(t *testing.T) {
		f := newFixture(t)
		s, v := f.seed()
		x := fill()
		n := f.candidate(s, x)
		expect(t, run(t, f.request(n, v, x)), "committed")
		_, e := f.pool.Exec(f.ctx, `INSERT INTO order_fills (tenant_id,order_id,fill_id,raw_fill_id) VALUES (current_setting('app.tenant_id'),'order','legacy',''),(current_setting('app.tenant_id'),'order','second-scoped','alias'),(current_setting('app.tenant_id'),'different','unrelated','')`)
		if e != nil {
			t.Fatal(e)
		}
		r := runResult(t, f.readRequest("applied-fills", "order"))
		expect(t, r.Status, "claims")
		if !reflect.DeepEqual(r.Claims, []string{"alias", "legacy"}) {
			t.Fatal("Rust did not collapse raw legacy aliases")
		}
		want, e := order.NewPostgres(f.pool).AppliedFills(f.ctx, "order")
		if e != nil || len(want) != len(r.Claims) {
			t.Fatal("Go claims differ")
		}
		for _, id := range r.Claims {
			if !want[id] {
				t.Fatal("Go missing returned claim")
			}
		}
		b := *f
		b.tenant = "tenant-b"
		b.pool = b.open(b.tenant)
		for _, req := range []map[string]any{b.readRequest("applied-fills", "order"), f.readRequest("applied-fills", ""), f.readRequest("applied-fills", "absent")} {
			r = runResult(t, req)
			if r.Status != "claims" || len(r.Claims) != 0 {
				t.Fatal("empty/scoped claims differ")
			}
		}
	})
	t.Run("read-notfound-unavailable-corruption", func(t *testing.T) {
		f := newFixture(t)
		readError(t, runResult(t, f.readRequest("load", "absent")), "NotFound")
		s, _ := f.seed()
		if _, e := f.pool.Exec(f.ctx, "UPDATE orders SET state=$1 WHERE order_id=$2", []byte{0xff}, s.OrderId); e != nil {
			t.Fatal(e)
		}
		readError(t, runResult(t, f.readRequest("load", s.OrderId)), "InvalidInput")
		if _, _, e := order.NewPostgres(f.pool).Load(f.ctx, s.OrderId); e == nil || errors.Is(e, order.ErrNotFound) {
			t.Fatal("Go corruption misclassified")
		}
		// Missing table is unavailable, never NotFound or an empty successful set.
		if _, e := f.pool.Exec(f.ctx, "ALTER TABLE order_fills RENAME TO unavailable_fills"); e != nil {
			t.Fatal(e)
		}
		readError(t, runResult(t, f.readRequest("load", s.OrderId)), "Database")
		readError(t, runResult(t, f.readRequest("applied-fills", s.OrderId)), "Database")
	})
	t.Run("claim-capacity-no-prefix", func(t *testing.T) {
		f := newFixture(t)
		// Migration0014 takes an advisory transaction lock for each alias. Seed
		// in committed batches, not one transaction that exceeds PostgreSQL's
		// shared lock table. Short IDs keep this below the separate byte cap.
		for start := 1; start <= 100001; start += 500 {
			end := min(start+499, 100001)
			if _, e := f.pool.Exec(f.ctx, `INSERT INTO order_fills (tenant_id,order_id,fill_id,raw_fill_id) SELECT current_setting('app.tenant_id'),'order',n::text,n::text FROM generate_series($1::bigint,$2::bigint)n`, start, end); e != nil {
				t.Fatal(e)
			}
			// Refresh empty-table estimates and invalidate cached trigger plans
			// across this deliberately large fixture. The canonical alias index,
			// trigger and original deadline remain enabled.
			if start == 1 || end%5000 == 0 {
				if _, e := f.pool.Exec(f.ctx, `ANALYZE order_fills`); e != nil {
					t.Fatal(e)
				}
				var plan []byte
				if e := f.pool.QueryRow(f.ctx, `EXPLAIN (FORMAT JSON) SELECT 1 FROM order_fills WHERE tenant_id=current_setting('app.tenant_id') AND raw_fill_id=$1 AND fill_id<>$1`, "absent").Scan(&plan); e != nil {
					t.Fatal(e)
				}
				if !bytes.Contains(plan, []byte("order_fills_alias_idx")) || bytes.Contains(plan, []byte("Seq Scan")) {
					t.Fatal("capacity fixture alias lookup did not use canonical index")
				}
			}
		}
		readError(t, runResult(t, f.readRequest("applied-fills", "order")), "Capacity")
	})
	t.Run("claim-read-failure-no-prefix", func(t *testing.T) {
		f := newFixture(t)
		_, e := f.pool.Exec(f.ctx, `INSERT INTO order_fills (tenant_id,order_id,fill_id) SELECT current_setting('app.tenant_id'),'order','claim-'||n FROM generate_series(1,9)n;
 CREATE SEQUENCE read_attempt;
 CREATE FUNCTION fail_after_prefix() RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$ BEGIN IF nextval('read_attempt')>5 THEN RAISE EXCEPTION 'fixture mid-read failure'; END IF; RETURN true; END $$;
 ALTER POLICY tenant_isolation ON order_fills USING (tenant_id=app_current_tenant() AND fail_after_prefix()) WITH CHECK (tenant_id=app_current_tenant())`)
		if e != nil {
			t.Fatal(e)
		}
		readError(t, runResult(t, f.readRequest("applied-fills", "order")), "Database")
		var attempted int64
		if e = f.pool.QueryRow(f.ctx, "SELECT last_value FROM read_attempt").Scan(&attempted); e != nil || attempted < 6 {
			t.Fatal("fixture never evaluated a successful prefix before failure")
		}
		if _, e = f.pool.Exec(f.ctx, "SELECT setval('read_attempt',1,false)"); e != nil {
			t.Fatal(e)
		}
		claims, e := order.NewPostgres(f.pool).AppliedFills(f.ctx, "order")
		if e == nil || claims != nil {
			t.Fatal("Go read failure must also discard the prefix")
		}
	})
}
