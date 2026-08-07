// THE POSTGRES STORE'S FILTER, AGAINST A REAL POSTGRES (#304).
//
// Until this file the audit service had NO Postgres-gated test at all: every
// test here ran against Memory. That matters more than usual for this store,
// because Filter is implemented TWICE — once as Go predicates in matches() for
// Memory, and once as pushed-down SQL in Postgres.Query. Two implementations of
// one concept is the shape CLAUDE.md warns about, and the only thing that keeps
// them honest is running the same property through both.
//
// The cursor is the reason this got written now. `AfterSeq` becomes `seq > $n`
// in SQL, and a cursor that is off by one in SQL does not raise — it returns an
// audit export that is silently missing a record, or silently repeating one.
// Neither is visible in the artifact.
//
// SCHEMA ISOLATION, deliberately (#212). audit_log is WORM by trigger, so this
// test cannot clean up with DELETE even if it wanted to. It therefore builds its
// own schema, applies the migration into it, and drops the schema afterwards —
// it never touches a shared audit_log. A test that proved paging by destroying
// the compliance log would be its own incident.
package audit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newAuditPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL (kanz/test/backing/up.sh) to run the audit Postgres tests")
	}
	ctx := context.Background()

	schema := fmt.Sprintf("audit_paging_%d", time.Now().UnixNano())
	boot, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect (bootstrap): %v", err)
	}
	defer boot.Close()
	if _, err := boot.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// search_path as a startup RuntimeParam, and WITHOUT public on it: every
	// unqualified name in the migration and in the store's SQL then resolves
	// here and nowhere else, so a missing object is an error rather than a
	// silent hit on the shared table.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		drop, err := pgxpool.New(context.Background(), url)
		if err != nil {
			t.Errorf("reconnect to drop schema %s: %v — it is now residue", schema, err)
			return
		}
		defer drop.Close()
		if _, err := drop.Exec(context.Background(),
			`DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
			t.Errorf("drop schema %s: %v — it is now residue in the shared database", schema, err)
		}
	})

	files, err := filepath.Glob(filepath.Join("../../migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (found %d)", err, len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	return pool
}

func seedPG(t *testing.T, st *Postgres, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		if _, err := st.Append(ctx, &Record{
			EventID:    fmt.Sprintf("pg-e%04d", i),
			Kind:       KindCommandOutcome,
			TenantID:   "t1",
			Domain:     "test",
			EventType:  "test.v1.Event",
			EventClass: "EVENT_CLASS_FACT",
			Source:     "postgres_paging_test",
			Summary:    fmt.Sprintf("record %d", i),
			OccurredAt: time.Unix(int64(1000+i), 0).UTC(),
			RecordedAt: time.Unix(int64(1000+i), 0).UTC(),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// THE CURSOR IS PUSHED INTO SQL AND IS EXCLUSIVE.
func TestPostgresQueryCursorIsExclusive(t *testing.T) {
	st := NewPostgres(newAuditPool(t))
	seedPG(t, st, 6)
	ctx := context.Background()

	all, err := st.Query(ctx, Filter{Tenant: "t1", Limit: 10})
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("seeded 6, read %d", len(all))
	}

	after := all[2].Seq
	rest, err := st.Query(ctx, Filter{Tenant: "t1", AfterSeq: after, Limit: 10})
	if err != nil {
		t.Fatalf("query after %d: %v", after, err)
	}
	if len(rest) != 3 {
		t.Fatalf("cursor after seq %d returned %d records, want 3 — `seq > $n` is not exclusive, so an "+
			"export either repeats the boundary record or drops it", after, len(rest))
	}
	if rest[0].Seq <= after {
		t.Fatalf("first record after the cursor has seq %d, want > %d", rest[0].Seq, after)
	}
	if rest[0].EventID != all[3].EventID {
		t.Fatalf("resumed at %q, want %q — the page boundary skipped a record", rest[0].EventID, all[3].EventID)
	}
}

// THE SAME PROPERTY THE MEMORY STORE IS HELD TO: paging to exhaustion yields
// every record exactly once, in seq order. Run here against real SQL because
// this is where the predicate actually executes in production.
func TestPostgresPagingReconstructsTheSelectionExactlyOnce(t *testing.T) {
	const total, page = 10, 3
	st := NewPostgres(newAuditPool(t))
	seedPG(t, st, total)
	ctx := context.Background()

	var seen []string
	var cursor int64
	for i := 0; ; i++ {
		if i > total {
			t.Fatal("paging did not terminate — the cursor is not advancing")
		}
		// Limit+1 is what report.Generate does to detect "more"; mirrored here so
		// this exercises the same query shape the service issues.
		recs, err := st.Query(ctx, Filter{Tenant: "t1", AfterSeq: cursor, Limit: page + 1})
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		done := len(recs) <= page
		if !done {
			recs = recs[:page]
		}
		for _, r := range recs {
			seen = append(seen, r.EventID)
		}
		if done {
			break
		}
		cursor = recs[len(recs)-1].Seq
	}

	if len(seen) != total {
		t.Fatalf("paging over Postgres yielded %d records for a %d-record log: %v", len(seen), total, seen)
	}
	for i, id := range seen {
		if want := fmt.Sprintf("pg-e%04d", i); id != want {
			t.Fatalf("record %d is %q, want %q — SQL paging did not preserve seq order", i, id, want)
		}
	}
}

// LIMIT 0 EMITS NO LIMIT CLAUSE. Pinned as a test because it is the sharp edge
// the whole issue rests on: `Limit: 0` reads as "no limit configured" to a
// reader and means "return everything" to the database. server.boundedLimit
// refuses a caller-supplied 0 for exactly this reason, and report.Generate
// refuses a template carrying it. If this behaviour ever changes to "return
// nothing", those two refusals become dead weight and should go with it.
func TestPostgresQueryWithZeroLimitIsUnbounded(t *testing.T) {
	st := NewPostgres(newAuditPool(t))
	seedPG(t, st, 5)

	recs, err := st.Query(context.Background(), Filter{Tenant: "t1", Limit: 0})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(recs) != 5 {
		t.Fatalf("Limit: 0 returned %d of 5 records. If this is now bounded, the refusals in "+
			"server.boundedLimit and report.Generate are describing behaviour that no longer exists",
			len(recs))
	}
}
