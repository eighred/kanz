package consume

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Every connection uses the real migrations in a disposable schema. Refuse a
// privileged role rather than passing an isolation assertion through RLS bypass.
func requiredInputPostgres(t *testing.T) func(string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires TEST_POSTGRES_URL with NOSUPERUSER/NOBYPASSRLS")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var super, bypass bool
	if err := admin.QueryRow(t.Context(), `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatal("required-input proof refuses a superuser/BYPASSRLS role")
	}
	schema := pgx.Identifier{fmt.Sprintf("alt_required_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	poolFor := func(tenant string) *pgxpool.Pool {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id',$1,false)`, tenant)
			return err
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	pool := poolFor("migration")
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) != 2 {
		t.Fatalf("expected both actual alternatives migrations: %d %v", len(files), err)
	}
	sort.Strings(files)
	for _, path := range files {
		ddl, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), string(ddl)); err != nil {
			t.Fatalf("migration %s: %v", path, err)
		}
	}
	pool.Close()
	return poolFor
}

func requiredJournalSnapshot(t *testing.T, pool *pgxpool.Pool) []byte {
	t.Helper()
	var data []byte
	if err := pool.QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY event_id),'[]'::jsonb) FROM fund_events e`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPostgresRequiredInputsNoClaimRestartAndRLS(t *testing.T) {
	poolFor := requiredInputPostgres(t)
	const tenant = "required-input-owner"
	pool := poolFor(tenant)
	store := fund.NewPostgres(pool)
	for _, kind := range requiredInputKinds() {
		for _, fault := range requiredInputFaults() {
			t.Run(kind.name+"_"+fault.name, func(t *testing.T) {
				assertRequiredInputNoClaim(t, store, tenant, kind, fault)
			})
		}
	}
	before := requiredJournalSnapshot(t, pool)
	pool.Close()
	restartedPool := poolFor(tenant)
	restarted := fund.NewPostgres(restartedPool)
	t.Run("restart_preserves_accepted_history", func(t *testing.T) {
		if !bytes.Equal(before, requiredJournalSnapshot(t, restartedPool)) {
			t.Fatal("restart changed accepted source history")
		}
		for _, kind := range requiredInputKinds() {
			for _, fault := range requiredInputFaults() {
				id := kind.name + "_" + fault.name
				book := kind.commitment(id, "book_"+id)
				events, err := restarted.Journal(t.Context(), book)
				if err != nil || len(events) != 1 || events[0].Amount.RatString() != "405/4" ||
					!events[0].Date.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
					t.Fatalf("restart lost corrected same-ID input: %+v %v", events, err)
				}
			}
		}
	})
	t.Run("rls_and_envelope_scope_preserve_tenant_identity", func(t *testing.T) {
		otherPool := poolFor("required-input-other")
		other := fund.NewPostgres(otherPool)
		kind := requiredInputKinds()[1]
		const id = "called_missing_amount"
		const book = "book_called_missing_amount"
		rows, err := other.Journal(t.Context(), book)
		if err != nil || len(rows) != 0 {
			t.Fatalf("foreign tenant discovered accepted history: %+v %v", rows, err)
		}
		folder, _ := NewFolder("required-input-other", other, DecodeProto(kind.subject))
		msg := marshalRequiredInput(t, kind.message(id, book, &commonpb.Decimal{Coefficient: 9}, timestamppb.New(t0())))
		if err := folder.Handle(t.Context(), &envelopepb.Envelope{TenantId: tenant, EventType: kind.subject}, msg); err == nil {
			t.Fatal("foreign envelope entered another tenant's fund")
		}
		if got := requiredJournalSnapshot(t, otherPool); string(got) != "[]" {
			t.Fatalf("foreign envelope claimed a row: %s", got)
		}
		if err := folder.Handle(t.Context(), &envelopepb.Envelope{TenantId: "required-input-other", EventType: kind.subject}, msg); err != nil {
			t.Fatalf("same domain ID in own tenant was incorrectly deduplicated: %v", err)
		}
		rows, err = other.Journal(t.Context(), book)
		if err != nil || len(rows) != 1 || rows[0].Amount.RatString() != "9" || !bytes.Equal(before, requiredJournalSnapshot(t, restartedPool)) {
			t.Fatalf("same-ID tenant scopes merged or original history changed: %+v %v", rows, err)
		}
	})
}

func TestPostgresRequiredInputCompatibility(t *testing.T) {
	poolFor := requiredInputPostgres(t)
	store := fund.NewPostgres(poolFor("required-compatibility"))
	for _, kind := range requiredInputKinds() {
		for _, tc := range []struct {
			name string
			date time.Time
		}{
			{"epoch", time.Unix(0, 0).UTC()},
			{"year_one", time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)},
			{"postgres_microsecond_precision", time.Date(2026, 6, 1, 0, 0, 0, 123456789, time.UTC)},
		} {
			t.Run(kind.name+"_"+tc.name, func(t *testing.T) {
				id := kind.name + "_" + tc.name
				book := kind.commitment(id, "book_"+id)
				folder, _ := NewFolder("required-compatibility", store, DecodeProto(kind.subject))
				body := marshalRequiredInput(t, kind.message(id, book, &commonpb.Decimal{Coefficient: -125, Exponent: -2}, timestamppb.New(tc.date)))
				if err := folder.Handle(t.Context(), &envelopepb.Envelope{TenantId: "required-compatibility", EventType: kind.subject}, body); err != nil {
					t.Fatal(err)
				}
				events, err := store.Journal(t.Context(), book)
				// pgx/PostgreSQL stores microseconds; do not claim nanosecond
				// persistence from the decoder's exact nanosecond compatibility.
				if err != nil || len(events) != 1 || events[0].Amount.RatString() != "-5/4" || !events[0].Date.Equal(tc.date.Truncate(time.Microsecond)) {
					t.Fatalf("valid timestamp/sign/precision compatibility changed: %+v %v", events, err)
				}
			})
		}
	}
}
