package storeconformance

import (
	"bytes"
	"fmt"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
	"time"
)

func queryStates(t *testing.T, r fixtureResult, want []*orderpb.OrderState) {
	t.Helper()
	if r.Status != "orders" || len(r.States) != len(want) {
		t.Fatal("query result status/count differs")
	}
	for i, s := range want {
		if !bytes.Equal(r.States[i], wire(t, s)) {
			t.Fatalf("canonical bytes differ at %d", i)
		}
	}
}
func (f *fixture) query(mode string, extra map[string]any) fixtureResult {
	req := f.readRequest(mode, "")
	for k, v := range extra {
		req[k] = v
	}
	return runResult(f.t, req)
}
func TestRustRepositoryQueries(t *testing.T) {
	t.Run("list-status-parent-exact", func(t *testing.T) {
		f := newFixture(t)
		store := order.NewPostgres(f.pool)
		for i := 4; i >= 0; i-- {
			s := f.initial()
			s.OrderId = fmt.Sprintf("order-%d", i)
			s.Status = orderpb.OrderStatus(i + 1)
			if i == 4 {
				s.Status = orderpb.OrderStatus(999)
			}
			if i%2 == 0 {
				s.ParentOrderId = "parent"
			}
			if i%2 == 0 {
				if e := store.Create(f.ctx, s, nil); e != nil {
					t.Fatal(e)
				}
			} else {
				expect(t, run(t, f.createRequest(s)), "committed")
			}
		}
		all, e := store.List(f.ctx)
		if e != nil {
			t.Fatal(e)
		}
		queryStates(t, f.query("list", nil), all)
		for _, statuses := range [][]orderpb.OrderStatus{nil, {1, 3}, {999}, {2, 2}} {
			want, e := store.ListByStatus(f.ctx, statuses...)
			if e != nil {
				t.Fatal(e)
			}
			queryStates(t, f.query("list-by-status", map[string]any{"statuses": statuses}), want)
		}
		for _, parent := range []string{"parent", "absent", ""} {
			want, e := store.ListByParent(f.ctx, parent)
			if e != nil {
				t.Fatal(e)
			}
			queryStates(t, f.query("list-by-parent", map[string]any{"parent_id": parent}), want)
		}
		// Selection follows denormalized columns, without rewriting canonical bytes.
		if _, e := f.pool.Exec(f.ctx, "UPDATE orders SET status=777 WHERE order_id='order-0'"); e != nil {
			t.Fatal(e)
		}
		want, e := store.ListByStatus(f.ctx, 777)
		if e != nil {
			t.Fatal(e)
		}
		queryStates(t, f.query("list-by-status", map[string]any{"statuses": []int{777}}), want)
	})
	t.Run("portfolio-page-count-backfill", func(t *testing.T) {
		f := newFixture(t)
		store := order.NewPostgres(f.pool)
		for i := 0; i < 205; i++ {
			s := f.initial()
			s.OrderId = fmt.Sprintf("order-%03d", i)
			if e := store.Create(f.ctx, s, nil); e != nil {
				t.Fatal(e)
			}
			if _, e := f.pool.Exec(f.ctx, "UPDATE orders SET created_at=$1 WHERE order_id=$2", at.Add(time.Duration(i)*time.Second), s.OrderId); e != nil {
				t.Fatal(e)
			}
		}
		if _, e := f.pool.Exec(f.ctx, "UPDATE orders SET portfolio_id='' WHERE order_id='order-000'"); e != nil {
			t.Fatal(e)
		}
		for _, limit := range []int{-1, 0, 1, 17, 200, 201} {
			want, n, e := store.ListByPortfolio(f.ctx, f.tenant+"-portfolio", limit)
			if e != nil {
				t.Fatal(e)
			}
			r := f.query("list-by-portfolio", map[string]any{"portfolio_id": f.tenant + "-portfolio", "limit": limit})
			queryStates(t, r, want)
			if r.Unindexed == nil || *r.Unindexed != n || n != 1 {
				t.Fatal("unindexed count differs")
			}
		}
		r := f.query("list-by-portfolio", map[string]any{"portfolio_id": ""})
		queryStates(t, r, nil)
		if r.Unindexed == nil || *r.Unindexed != 0 {
			t.Fatal("empty portfolio is not a real selector")
		}
		s, v, e := store.Load(f.ctx, "order-000")
		if e != nil {
			t.Fatal(e)
		}
		if e = store.Save(f.ctx, s, v, nil, ""); e != nil {
			t.Fatal(e)
		}
		r = f.query("list-by-portfolio", map[string]any{"portfolio_id": f.tenant + "-portfolio"})
		if r.Unindexed == nil || *r.Unindexed != 0 {
			t.Fatal("canonical save backfill not observed")
		}
	})
	t.Run("tenant-same-ids", func(t *testing.T) {
		f := newFixture(t)
		a := f.initial()
		expect(t, run(t, f.createRequest(a)), "committed")
		b := *f
		b.tenant = "tenant-b"
		b.pool = f.open(b.tenant)
		s := b.initial()
		s.ParentOrderId = "b-parent"
		expect(t, run(t, b.createRequest(s)), "committed")
		queryStates(t, f.query("list", nil), []*orderpb.OrderState{a})
		queryStates(t, b.query("list", nil), []*orderpb.OrderState{s})
		queryStates(t, f.query("list-by-parent", map[string]any{"parent_id": "b-parent"}), nil)
		r := f.query("list-by-portfolio", map[string]any{"portfolio_id": s.PortfolioId})
		queryStates(t, r, nil)
	})
	t.Run("corruption-capacity-no-prefix", func(t *testing.T) {
		f := newFixture(t)
		s, _ := f.seed()
		other := proto.Clone(s).(*orderpb.OrderState)
		other.OrderId = "zzz"
		if e := order.NewPostgres(f.pool).Create(f.ctx, other, nil); e != nil {
			t.Fatal(e)
		}
		if _, e := f.pool.Exec(f.ctx, "UPDATE orders SET parent_order_id='parent'"); e != nil {
			t.Fatal(e)
		}
		for _, tc := range []struct {
			raw    []byte
			status string
		}{{[]byte{0xff}, "InvalidInput"}, {bytes.Repeat([]byte{0}, (1<<20)+1), "Capacity"}} {
			if _, e := f.pool.Exec(f.ctx, "UPDATE orders SET state=$1 WHERE order_id='zzz'", tc.raw); e != nil {
				t.Fatal(e)
			}
			for _, mode := range []string{"list", "list-by-status", "list-by-parent", "list-by-portfolio"} {
				readError(t, f.query(mode, mapForQuery(mode, s)), tc.status)
			}
		}
		// Two individually bounded canonical states exceed only the whole-result cap.
		big := proto.Clone(s).(*orderpb.OrderState)
		big.OrderId = strings.Repeat("x", 600000)
		if _, e := f.pool.Exec(f.ctx, "UPDATE orders SET state=$1", wire(t, big)); e != nil {
			t.Fatal(e)
		}
		readError(t, f.query("list", nil), "Capacity")
		var withheld bool
		if e := f.pool.QueryRow(f.ctx, `SELECT bool_and(projected IS NULL) FROM (SELECT CASE WHEN sum(octet_length(state)) OVER ()<=$1 THEN state END AS projected FROM (SELECT order_id,state FROM orders ORDER BY order_id LIMIT 100001) selected) bounded`, int64(1<<20)).Scan(&withheld); e != nil || !withheld {
			t.Fatal("aggregate overflow bytes were not withheld inside SQL")
		}
	})
	t.Run("row-cap-no-prefix", func(t *testing.T) {
		f := newFixture(t)
		raw := []byte{} // Empty canonical message isolates row overhead from the byte cap.
		if _, e := f.pool.Exec(f.ctx, `INSERT INTO orders(tenant_id,order_id,status,state,portfolio_id,parent_order_id) SELECT current_setting('app.tenant_id'),'row-'||n,1,$1,'','' FROM generate_series(1,100001)n`, raw); e != nil {
			t.Fatal(e)
		}
		readError(t, f.query("list", nil), "Capacity")
	})
	t.Run("canonical-index-plans", func(t *testing.T) {
		f := newFixture(t)
		if _, e := f.pool.Exec(f.ctx, `INSERT INTO orders(tenant_id,order_id,status,state,portfolio_id,parent_order_id) SELECT current_setting('app.tenant_id'),'bulk-'||n,1,$1,'bulk','' FROM generate_series(1,20000)n`, wire(t, f.initial())); e != nil {
			t.Fatal(e)
		}
		s := f.initial()
		s.OrderId = "selected"
		s.Status = 777
		s.ParentOrderId = "selected-parent"
		s.PortfolioId = "selected-portfolio"
		expect(t, run(t, f.createRequest(s)), "committed")
		if _, e := f.pool.Exec(f.ctx, "ANALYZE orders"); e != nil {
			t.Fatal(e)
		}
		for _, tc := range []struct {
			sql, index string
			args       []any
		}{
			{`SELECT CASE WHEN sum(octet_length(state)) OVER ()<=$2 THEN state END FROM (SELECT order_id,state FROM orders WHERE status=ANY($1) ORDER BY order_id LIMIT 100001) selected ORDER BY order_id`, "orders_status_idx", []any{[]int32{777}, int64(1 << 20)}},
			{`SELECT CASE WHEN sum(octet_length(state)) OVER ()<=$2 THEN state END FROM (SELECT order_id,state FROM orders WHERE parent_order_id=$1 AND parent_order_id<>'' ORDER BY order_id LIMIT 100001) selected ORDER BY order_id`, "orders_parent_idx", []any{"selected-parent", int64(1 << 20)}},
			{`SELECT CASE WHEN sum(octet_length(state)) OVER ()<=$3 THEN state END FROM (SELECT created_at,state FROM orders WHERE portfolio_id=$1 ORDER BY created_at DESC LIMIT $2) selected ORDER BY created_at DESC`, "orders_portfolio_idx", []any{"selected-portfolio", int64(200), int64(1 << 20)}},
		} {
			var plan []byte
			if e := f.pool.QueryRow(f.ctx, "EXPLAIN (FORMAT JSON) "+tc.sql, tc.args...).Scan(&plan); e != nil {
				t.Fatal(e)
			}
			if !bytes.Contains(plan, []byte(tc.index)) || bytes.Contains(plan, []byte("Seq Scan")) {
				t.Fatalf("query did not use canonical index %s", tc.index)
			}
		}
		queryStates(t, f.query("list-by-status", map[string]any{"statuses": []int{777}}), []*orderpb.OrderState{s})
		queryStates(t, f.query("list-by-parent", map[string]any{"parent_id": "selected-parent"}), []*orderpb.OrderState{s})
		queryStates(t, f.query("list-by-portfolio", map[string]any{"portfolio_id": "selected-portfolio"}), []*orderpb.OrderState{s})
	})
	t.Run("sql-failure-no-prefix", func(t *testing.T) {
		f := newFixture(t)
		if _, e := f.pool.Exec(f.ctx, `INSERT INTO orders(tenant_id,order_id,state) SELECT current_setting('app.tenant_id'),'row-'||n,$1 FROM generate_series(1,9)n`, wire(t, f.initial())); e != nil {
			t.Fatal(e)
		}
		if _, e := f.pool.Exec(f.ctx, `CREATE SEQUENCE query_attempt;
 CREATE FUNCTION query_failure() RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$ BEGIN IF nextval('query_attempt')>5 THEN RAISE EXCEPTION 'fixture mid-read failure'; END IF; RETURN true; END $$;
 ALTER POLICY tenant_isolation ON orders USING (tenant_id=current_setting('app.tenant_id',true) AND query_failure()) WITH CHECK (tenant_id=current_setting('app.tenant_id',true))`); e != nil {
			t.Fatal(e)
		}
		readError(t, f.query("list", nil), "Database")
		var attempted int64
		if e := f.pool.QueryRow(f.ctx, "SELECT last_value FROM query_attempt").Scan(&attempted); e != nil || attempted < 6 {
			t.Fatal("read never evaluated a prefix before failure")
		}
	})
	t.Run("scope-selector-refusal", func(t *testing.T) {
		f := newFixture(t)
		f.seed()
		for _, mode := range []string{"list", "list-by-status", "list-by-parent", "list-by-portfolio"} {
			req := f.readRequest(mode, "")
			req["tenant"] = ""
			readError(t, runResult(t, req), "InvalidInput")
		}
		readError(t, f.query("list-by-status", map[string]any{"statuses": make([]int32, 129)}), "Capacity")
	})
	t.Run("unavailable-no-result", func(t *testing.T) {
		f := newFixture(t)
		f.seed()
		if _, e := f.pool.Exec(f.ctx, "ALTER TABLE orders RENAME COLUMN state TO unavailable_state"); e != nil {
			t.Fatal(e)
		}
		readError(t, f.query("list", nil), "Database")
	})
}
func mapForQuery(mode string, s *orderpb.OrderState) map[string]any {
	switch mode {
	case "list-by-parent":
		return map[string]any{"parent_id": "parent"}
	case "list-by-status":
		return map[string]any{"statuses": []int32{int32(s.Status)}}
	case "list-by-portfolio":
		return map[string]any{"portfolio_id": s.PortfolioId}
	}
	return nil
}
