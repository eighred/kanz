package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/server"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Capture the actual store result so that an unrelated HTTP 500 cannot
// certify the rollback fence.
type rollbackStore struct {
	*custody.Postgres
	err error
}

func (s *rollbackStore) ApplyAction(ctx context.Context, a custody.Action) (custody.ActionEvidence, error) {
	evidence, err := s.Postgres.ApplyAction(ctx, a)
	s.err = err
	return evidence, err
}

func checkPilotRollback(t *testing.T, ctx context.Context, pool *pgxpool.Pool, breakID string) {
	t.Helper()
	const tenant = "synthetic-custody-pilot"
	var protected bool
	err := pool.QueryRow(ctx, `SELECT relrowsecurity AND relforcerowsecurity
		AND NOT rolsuper AND NOT rolbypassrls AND app_current_tenant()=$1
		AND current_database()='kanz_custody_pilot'
		FROM pg_class, pg_roles WHERE pg_class.oid='custody_actions'::regclass AND rolname=current_user`, tenant).Scan(&protected)
	if err != nil || !protected {
		t.Fatalf("expected FORCE RLS on the pilot action receipt under a restricted role: %v", err)
	}

	// Compare every persisted financial/lifecycle field and the retained
	// source, plus all receipts and outbox records visible in this tenant.
	snapshot := func() string {
		t.Helper()
		var data string
		err := pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(b),
			(SELECT to_jsonb(d) FROM custody_break_detections d WHERE d.break_id=b.break_id),
			(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.request_id) FROM custody_actions a),
			(SELECT jsonb_agg(to_jsonb(o) ORDER BY o.id) FROM outbox o),
			(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.statement_id) FROM custody_batch_receipts r))::text
			FROM custody_breaks b WHERE break_id=$1`, breakID).Scan(&data)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	baseline := snapshot()
	store := custody.NewPostgres(pool)
	b, err := store.LoadBreak(ctx, breakID)
	if err != nil || b.Status != custody.BreakOpen || !b.ValuesVerified || b.PortfolioID != "PF1" || b.Revision < 1 {
		t.Fatalf("expected an open exact PF1 break: %v", err)
	}
	post := func(pool *pgxpool.Pool, owner, route, action string, revision int64) (int, error) {
		t.Helper()
		observed := &rollbackStore{Postgres: custody.NewPostgres(pool)}
		// This is the old replica's normal action-enabled composition.
		handler := server.New(&server.Readiness{}, nil, ledger.NewPostgres(pool), "USD", server.WithTenant(owner), server.WithBreakStore(observed))
		body := fmt.Sprintf(`{"request_id":"rollback-%s","action":%q,"expected_revision":"%d","review_contract":"exact-v1"`, route, action, revision)
		if action == "explain" {
			body += `,"explanation":"signed statement under review"`
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/custody/breaks/"+url.PathEscape(breakID)+"/"+route, strings.NewReader(body+"}"))
		auth.SetPrincipalHeaders(request.Header, "synthetic-pilot-reader", owner, []string{"fund"})
		if err := auth.SetPrincipalPortfolios(request.Header, []string{"PF1"}); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code, observed.err
	}
	for _, operation := range []struct{ route, action string }{{"actions", "claim"}, {"assign", "claim"}, {"explain", "explain"}} {
		code, err := post(pool, tenant, operation.route, operation.action, b.Revision)
		var refusal *pgconn.PgError
		if code != http.StatusInternalServerError || !errors.As(err, &refusal) || refusal.Code != "42501" ||
			!strings.Contains(refusal.Message, "signed_pilot_read_only_actions") {
			t.Fatalf("%s did not reach the pilot database policy: HTTP %d, %v", operation.route, code, err)
		}
		if snapshot() != baseline {
			t.Fatalf("%s changed the persisted pilot break, source, receipt or outbox", operation.route)
		}
	}

	const neighbor = "synthetic-custody-rollback-control"
	other, err := pg.NewTenantPool(ctx, os.Getenv("TEST_POSTGRES_URL"), neighbor)
	if err != nil {
		t.Fatal("other tenant fixture unavailable")
	}
	defer other.Close()
	subject := custody.Subject{PortfolioID: b.PortfolioID, CustodianID: b.CustodianID, BusinessDate: b.FirstSeenAt}
	stored, err := custody.NewPostgres(other).UpsertBreaks(ctx, subject, []custody.Break{b}, time.Now())
	if err != nil || len(stored) != 1 || stored[0].BreakID != breakID {
		t.Fatalf("other tenant same-ID control missing: %v", err)
	}
	code, err := post(other, neighbor, "actions", "claim", stored[0].Revision)
	if code != http.StatusOK || err != nil || snapshot() != baseline {
		t.Fatalf("other tenant's authorized claim did not commit in isolation: HTTP %d, %v", code, err)
	}
	var correct bool
	err = other.QueryRow(ctx, `SELECT status='assigned' AND assignee='synthetic-pilot-reader' AND revision=$2
		AND (SELECT count(*) FROM custody_actions WHERE break_id=$1)=1
		AND (SELECT count(*) FROM outbox WHERE event_type=$3)=1
		FROM custody_breaks WHERE break_id=$1`, breakID, stored[0].Revision+1, custody.SubjectActionRecorded).Scan(&correct)
	if err != nil || !correct {
		t.Fatalf("other tenant break, receipt or FACT not committed: %v", err)
	}
}
