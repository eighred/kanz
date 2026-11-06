package storeconformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/services/oms/internal/order"
)

type queueRow struct {
	ID            int64  `json:"id"`
	Attempts      int    `json:"attempts"`
	LastError     string `json:"last_error"`
	Tenant        string `json:"tenant"`
	Key           string `json:"partition_key"`
	Subject       string `json:"subject"`
	EventType     string `json:"event_type"`
	EventClass    int32  `json:"event_class"`
	SchemaVersion int32  `json:"schema_version"`
	Domain        string `json:"domain"`
	Schema        string `json:"payload_schema_ref"`
	Seconds       int64  `json:"event_seconds"`
	Nanos         int    `json:"event_nanos"`
	Correlation   string `json:"correlation_id"`
	Causation     string `json:"causation_id"`
	Trace         string `json:"trace_context"`
	Payload       []byte `json:"payload"`
}

func (f *fixture) queueRequest(mode string) map[string]any {
	return map[string]any{"mode": "outbox-" + mode, "schema": f.schema, "tenant": f.tenant, "order_id": "order", "limit": 256}
}
func seedQueue(t *testing.T, f *fixture) {
	t.Helper()
	s, v := f.seed()
	x := fill()
	n := f.candidate(s, x)
	expect(t, run(t, f.request(n, v, x)), "committed")
	// Unknown protobuf bytes remain opaque to the queue repository.
	if _, e := f.pool.Exec(f.ctx, `UPDATE outbox SET payload=payload||decode('f80701','hex'),correlation_id='correlation',causation_id='cause',trace_context='trace',envelope_tenant_id='envelope-other'`); e != nil {
		t.Fatal(e)
	}
}
func assertQueue(t *testing.T, f *fixture) []outbox.Pending {
	t.Helper()
	goRows, e := outbox.NewPostgres(f.pool, outbox.ServiceOMS).Pending(f.ctx, "order", 256)
	if e != nil {
		t.Fatal(e)
	}
	got := runResult(t, f.queueRequest("pending"))
	expect(t, got.Status, "pending")
	if len(got.Pending) != len(goRows) {
		t.Fatal("queue length differs")
	}
	for i, g := range goRows {
		encoded, e := json.Marshal(factJSON(g.Record))
		if e != nil {
			t.Fatal(e)
		}
		var want queueRow
		if e = json.Unmarshal(encoded, &want); e != nil {
			t.Fatal(e)
		}
		want.ID = g.ID
		want.Attempts = g.Attempts
		want.LastError = g.LastError
		if !reflect.DeepEqual(want, got.Pending[i]) {
			t.Fatal("raw record or metadata differs")
		}
	}
	return goRows
}

func TestRustOutboxQueue(t *testing.T) {
	t.Run("ordered-pages-and-schema-lock", func(t *testing.T) {
		f := newFixture(t)
		seedQueue(t, f)
		rows := assertQueue(t, f)
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		r := rows[0].Record
		r.PartitionKey = "later"
		if err = outbox.Enqueue(f.ctx, tx, outbox.ServiceOMS, r); err != nil {
			t.Fatal(err)
		}
		r.PartitionKey = "order"
		if err = outbox.Enqueue(f.ctx, tx, outbox.ServiceOMS, r); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		rows = assertQueue(t, f)
		if len(rows) != 2 || rows[0].ID >= rows[1].ID {
			t.Fatal("nonvacuous ordering")
		}
		req := f.queueRequest("keys")
		if got := runResult(t, req); !reflect.DeepEqual(got.Claims, []string{"order", "later"}) {
			t.Fatal("oldest key first")
		}
		req["limit"] = 1
		if got := runResult(t, req); !reflect.DeepEqual(got.Claims, []string{"order"}) {
			t.Fatal("intentional page")
		}
		req = f.queueRequest("pending")
		req["limit"] = 1
		if got := runResult(t, req); len(got.Pending) != 1 || got.Pending[0].ID != rows[0].ID {
			t.Fatal("oldest row page")
		}
		release, won, err := outbox.NewPostgres(f.pool, outbox.ServiceOMS).LockKey(f.ctx, "order", false)
		if err != nil || !won {
			t.Fatal("first schema lock")
		}
		defer release()
		other := newFixture(t)
		req = other.queueRequest("lock")
		req["limit"] = 1
		expect(t, run(t, req), "released")
	})
	t.Run("raw-page-mark-age-parity", func(t *testing.T) {
		f := newFixture(t)
		seedQueue(t, f)
		q := outbox.NewPostgres(f.pool, outbox.ServiceOMS)
		rows := assertQueue(t, f)
		if len(rows) != 1 {
			t.Fatal("nonvacuous queue required")
		}
		keys, e := q.PendingKeys(f.ctx, 256)
		if e != nil {
			t.Fatal(e)
		}
		got := runResult(t, f.queueRequest("keys"))
		if !reflect.DeepEqual(keys, got.Claims) {
			t.Fatal("keys differ")
		}
		req := f.queueRequest("failed")
		req["expected_version"] = rows[0].ID
		req["other"] = strings.Repeat("x", 1001)
		expect(t, run(t, req), "marked")
		rows = assertQueue(t, f)
		if rows[0].Attempts != 1 || len(rows[0].LastError) != 1000 {
			t.Fatal("failure bookkeeping")
		}
		if e = q.MarkFailed(f.ctx, rows[0].ID, errors.New("Go failure")); e != nil {
			t.Fatal(e)
		}
		assertQueue(t, f)
		now := time.Now().UTC().Truncate(time.Second)
		req = f.queueRequest("age")
		req["expected_version"] = now.Unix()
		age, present, e := q.OldestPendingAge(f.ctx, now)
		got = runResult(t, req)
		if e != nil || !present || got.AgeNS == nil || *got.AgeNS != uint64(age) {
			t.Fatal("age differs")
		}
		req = f.queueRequest("published")
		req["expected_version"] = rows[0].ID
		expect(t, run(t, req), "marked")
		var first time.Time
		if e = f.pool.QueryRow(f.ctx, "SELECT published_at FROM outbox WHERE id=$1", rows[0].ID).Scan(&first); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, req), "marked")
		req["mode"] = "outbox-failed"
		req["other"] = "late"
		expect(t, run(t, req), "marked")
		var stamp time.Time
		var attempts int
		var last string
		if e = f.pool.QueryRow(f.ctx, "SELECT published_at,attempts,last_error FROM outbox WHERE id=$1", rows[0].ID).Scan(&stamp, &attempts, &last); e != nil || !stamp.Equal(first) || attempts != 2 || last != "" {
			t.Fatal("published idempotency")
		}
		assertQueue(t, f)
		req = f.queueRequest("age")
		req["expected_version"] = now.Unix()
		if runResult(t, req).AgeNS != nil {
			t.Fatal("empty is not unknown age")
		}
	})
	t.Run("owner-tenant-and-legacy-refusal", func(t *testing.T) {
		f := newFixture(t)
		seedQueue(t, f)
		rows := assertQueue(t, f)
		for _, mode := range []string{"published", "failed"} {
			req := f.queueRequest(mode)
			req["expected_version"] = rows[0].ID
			req["tenant"] = "tenant-b"
			expect(t, run(t, req), "RecordNotOwned")
		}
		if _, e := f.pool.Exec(f.ctx, "UPDATE outbox SET producer_service='accounting'"); e != nil {
			t.Fatal(e)
		}
		req := f.queueRequest("published")
		req["expected_version"] = rows[0].ID
		expect(t, run(t, req), "RecordNotOwned")
		if _, e := f.pool.Exec(f.ctx, `ALTER TABLE outbox DROP CONSTRAINT outbox_producer_service_required; UPDATE outbox SET producer_service=NULL; ALTER TABLE outbox ADD CONSTRAINT outbox_producer_service_required CHECK(producer_service IS NOT NULL AND producer_service<>'') NOT VALID`); e != nil {
			t.Fatal(e)
		}
		for _, mode := range []string{"pending", "keys", "age"} {
			req := f.queueRequest(mode)
			req["expected_version"] = time.Now().Unix()
			expect(t, run(t, req), "UnownedOutbox")
		}
		req = f.queueRequest("keys")
		req["limit"] = 0
		expect(t, run(t, req), "keys")
	})
	t.Run("bounded-page-no-prefix", func(t *testing.T) {
		f := newFixture(t)
		seedQueue(t, f)
		req := f.queueRequest("pending")
		req["limit"] = 4097
		expect(t, run(t, req), "Capacity")
		if _, e := f.pool.Exec(f.ctx, "UPDATE outbox SET trace_context=repeat('x',1048577)"); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, f.queueRequest("pending")), "Capacity")
		// Each indexed key stays small; the requested page's combined bytes
		// exceed the cap without relying on PostgreSQL accepting huge B-tree keys.
		if _, e := f.pool.Exec(f.ctx, `INSERT INTO outbox (producer_service,envelope_tenant_id,partition_key,subject,event_type,event_class,schema_version,domain,payload_schema_ref,event_time,correlation_id,causation_id,trace_context,payload)
		SELECT 'oms','envelope',repeat(md5(i::text),12)||i::text,'fixture','fixture',1,1,'order','fixture',now(),'','','',''::bytea FROM generate_series(1,3000) AS i`); e != nil {
			t.Fatal(e)
		}
		req = f.queueRequest("keys")
		req["limit"] = 4096
		expect(t, run(t, req), "Capacity")
	})
	t.Run("mark-deferred-commit-unconfirmed", func(t *testing.T) {
		f := newFixture(t)
		seedQueue(t, f)
		rows := assertQueue(t, f)
		if _, e := f.pool.Exec(f.ctx, `CREATE FUNCTION refuse_queue_commit() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'fixture'; END$$;CREATE CONSTRAINT TRIGGER refuse_queue_commit AFTER UPDATE ON outbox DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_queue_commit()`); e != nil {
			t.Fatal(e)
		}
		for _, mode := range []string{"published", "failed"} {
			req := f.queueRequest(mode)
			req["expected_version"] = rows[0].ID
			expect(t, run(t, req), "CommitUnconfirmed")
		}
		current := assertQueue(t, f)
		if current[0].Attempts != 0 || !bytes.Equal(current[0].Record.Payload, rows[0].Record.Payload) {
			t.Fatal("known fixture rollback differs")
		}
	})
	t.Run("go-rust-lock-contention-cancel", func(t *testing.T) {
		f := newFixture(t)
		q := outbox.NewPostgres(f.pool, outbox.ServiceOMS)
		release, won, e := q.LockKey(f.ctx, "order", false)
		if e != nil || !won {
			t.Fatal("Go lock")
		}
		var once sync.Once
		defer once.Do(release)
		req := f.queueRequest("lock")
		req["limit"] = 1
		expect(t, run(t, req), "contended")
		req["mode"] = "outbox-cancel"
		req["rollback"] = true
		req["limit"] = 100
		expect(t, run(t, req), "canceled")
		var sessions int
		deadline := time.Now().Add(5 * time.Second)
		for {
			if e = f.pool.QueryRow(f.ctx, "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1", f.schema).Scan(&sessions); e != nil {
				t.Fatal(e)
			}
			if sessions == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("canceled Rust session retained")
			}
			time.Sleep(10 * time.Millisecond)
		}
		// Other tenant/service scopes are independent despite identical logical key.
		req = f.queueRequest("lock")
		req["limit"] = 1
		req["tenant"] = "tenant-b"
		expect(t, run(t, req), "released")
		once.Do(release)
		req = f.queueRequest("lock")
		req["limit"] = 1
		expect(t, run(t, req), "released")
		unlock, won, e := outbox.NewPostgres(f.pool, outbox.ServiceAccounting).LockKey(f.ctx, "order", false)
		if e != nil || !won {
			t.Fatal("accounting lock")
		}
		defer unlock()
		expect(t, run(t, req), "released")
	})
	t.Run("rust-go-lock-session-loss", func(t *testing.T) {
		f := newFixture(t)
		req := f.queueRequest("lock")
		req["limit"] = 3000
		done := make(chan string, 1)
		go func() { done <- run(t, req) }()
		deadline := time.Now().Add(5 * time.Second)
		var pid int
		for {
			e := f.pool.QueryRow(f.ctx, "SELECT a.pid FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.application_name=$1 AND l.locktype='advisory' AND l.granted", f.schema).Scan(&pid)
			if e == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Rust never acquired lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
		q := outbox.NewPostgres(f.pool, outbox.ServiceOMS)
		release, won, e := q.LockKey(f.ctx, "order", false)
		if e != nil || won {
			if release != nil {
				release()
			}
			t.Fatal("Go did not contend")
		}
		ctx, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
		defer cancel()
		release, won, e = q.LockKey(ctx, "order", true)
		if e == nil || won {
			if release != nil {
				release()
			}
			t.Fatal("Go wait failed to cancel")
		}
		if _, e = f.pool.Exec(f.ctx, "SELECT pg_terminate_backend($1)", pid); e != nil {
			t.Fatal("session loss fixture")
		}
		select {
		case status := <-done:
			expect(t, status, "Database")
		case <-time.After(25 * time.Second):
			t.Fatal("Rust session loss fixture did not finish")
		}
		release, won, e = q.LockKey(f.ctx, "order", false)
		if e != nil || !won {
			t.Fatal("lost Rust session stranded key")
		}
		release()
	})
	t.Run("cancel-transport-server-fallback", func(t *testing.T) {
		f := newFixture(t)
		release, won, e := outbox.NewPostgres(f.pool, outbox.ServiceOMS).LockKey(f.ctx, "order", false)
		if e != nil || !won {
			t.Fatal("Go lock")
		}
		defer release()
		req := f.queueRequest("cancel-fallback")
		req["rollback"] = true
		req["limit"] = 100
		start := time.Now()
		expect(t, run(t, req), "canceled")
		if time.Since(start) < 4*time.Second {
			t.Fatal("fallback did not exercise blocked cancellation transport")
		}
	})
	t.Run("rust-enqueue-connected-owner", func(t *testing.T) {
		f := newFixture(t)
		seedQueue(t, f)
		rows := assertQueue(t, f)
		if len(rows) != 1 || rows[0].Record.TenantID == f.tenant {
			t.Fatal("storage/envelope distinction")
		}
		if _, _, e := order.NewPostgres(f.pool).Load(f.ctx, "order"); e != nil {
			t.Fatal(e)
		}
	})
}
