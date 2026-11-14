package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const scopeTenant = "__system__"

func scopePool(t *testing.T, original *pgxpool.Pool, tenant string) *pgxpool.Pool {
	t.Helper()
	cfg := original.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id',$1,false)`, tenant)
		return err
	}
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func scopeRestricted(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	var privileged bool
	if err := p.QueryRow(t.Context(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil || privileged {
		t.Fatalf("NOSUPERUSER NOBYPASSRLS required: privileged=%v err=%v", privileged, err)
	}
}

func scopeSeedStored(t *testing.T, p *pgxpool.Pool) (*custody.Postgres, map[string]string) {
	t.Helper()
	scopeRestricted(t, p)
	st := custody.NewPostgres(p)
	ids := map[string]string{"PF-A": "opaque/A?cash=1", "PF-B": "PF-A|C|quantity|foreign"}
	for i, pf := range []string{"PF-A", "PF-B"} {
		s := custody.Subject{PortfolioID: pf, CustodianID: "C", BusinessDate: breakT0}
		b := custody.FromRecon(s, []recon.Break{{Kind: recon.BreakQuantity, Key: "asset", IBOR: big.NewRat(int64(100+i*800), 1), Custodian: big.NewRat(90, 1), Diff: big.NewRat(int64(10+i*800), 1)}}, breakT0)
		b[0].BreakID = ids[pf]
		if _, err := st.UpsertBreaks(t.Context(), s, b, breakT0); err != nil {
			t.Fatal(err)
		}
	}
	return st, ids
}

func scopeSnapshot(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"custody_breaks", "custody_actions", "custody_runs", "custody_statements", "outbox"} {
		var snapshot string
		if err := p.QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(snapshot_row ORDER BY snapshot_row::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(t) AS snapshot_row FROM `+table+` t) snapshot`).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, snapshot)
	}
	return strings.Join(parts, "\n")
}

func scopeActionHTTP(t *testing.T, server *httptest.Server, tenant, id, requestID, revision string, scopes ...string) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"request_id": requestID, "expected_revision": revision, "action": "claim"})
	if err != nil {
		t.Fatal(err)
	}
	return scopedHTTP(t, server.Client(), server.URL, "POST", "/v1/custody/breaks/"+url.PathEscape(id)+"/actions", tenant, string(body), scopes...)
}

func TestPostgresAccountingPortfolioAuthorization(t *testing.T) {
	pool := newServerPool(t)
	st, ids := scopeSeedStored(t, pool)
	s := New(&Readiness{}, nil, ledger.NewPostgres(pool), "USD", WithTenant(scopeTenant), WithBreakStore(st))
	h := httptest.NewServer(s)
	defer h.Close()
	t.Run("denied_new_stale_missing_and_malformed_are_read_only", func(t *testing.T) {
		before := scopeSnapshot(t, pool)
		for _, tc := range []struct {
			id, revision string
			scope        []string
			code         int
		}{
			{ids["PF-B"], "1", []string{`["PF-A"]`}, 404},
			{ids["PF-B"], "999", []string{`["PF-A"]`}, 404},
			{"absent", "1", []string{`["PF-A"]`}, 404},
			{ids["PF-A"], "1", []string{`{}`}, 403},
			{ids["PF-A"], "1", []string{`[]`, `[]`}, 403},
		} {
			code, body := scopeActionHTTP(t, h, scopeTenant, tc.id, "denied", tc.revision, tc.scope...)
			if code != tc.code {
				t.Fatalf("status=%d want=%d %s", code, tc.code, body)
			}
			if got := scopeSnapshot(t, pool); got != before {
				t.Fatal("denied action changed break/revision/history/evidence/outbox")
			}
		}
	})
	var receipt string
	t.Run("exact_stored_owner_authorizes_opaque_escaped_ID", func(t *testing.T) {
		code, body := scopeActionHTTP(t, h, scopeTenant, ids["PF-A"], "claim-a", "1", `["PF-A"]`)
		if code != 200 {
			t.Fatalf("status=%d %s", code, body)
		}
		receipt = body
		var e custody.ActionEvidence
		if err := json.Unmarshal([]byte(body), &e); err != nil || e.Values == nil || e.Values.IBOR != "100" || e.Values.Difference != "10" || e.After.Revision != 2 {
			t.Fatalf("receipt=%s err=%v", body, err)
		}
	})
	if receipt == "" {
		t.Fatal("authorized action failed; cannot qualify retries")
	}
	t.Run("reauthorized_scope_change_and_legacy_empty_keep_original_receipt", func(t *testing.T) {
		before := scopeSnapshot(t, pool)
		for _, scope := range [][]string{{`["PF-B","PF-A"]`}, {`[]`}, nil} {
			code, body := scopeActionHTTP(t, h, scopeTenant, ids["PF-A"], "claim-a", "1", scope...)
			if code != 200 || body != receipt {
				t.Fatalf("historical retry changed: status=%d %s want=%s", code, body, receipt)
			}
		}
		if scopeSnapshot(t, pool) != before {
			t.Fatal("receipt retry rewrote evidence or re-enqueued FACT")
		}
	})
	t.Run("revoked_scope_and_foreign_receipt_are_indistinguishable", func(t *testing.T) {
		before := scopeSnapshot(t, pool)
		for _, tc := range []struct{ tenant, id, request, revision, scope string }{
			{scopeTenant, ids["PF-A"], "claim-a", "1", `["PF-B"]`},
			{scopeTenant, ids["PF-A"], "new-a", "1", `["PF-B"]`},
			{scopeTenant, ids["PF-A"], "stale-a", "999", `["PF-B"]`},
			{scopeTenant, ids["PF-B"], "claim-a", "1", `["PF-B"]`},
			{scopeTenant, "absent", "claim-a", "1", `["PF-B"]`},
			{"foreign-tenant", ids["PF-A"], "claim-a", "1", `["PF-A"]`},
		} {
			code, body := scopeActionHTTP(t, h, tc.tenant, tc.id, tc.request, tc.revision, tc.scope)
			if code != 404 || body != "{\"error\":\"no such break\"}\n" && tc.tenant == scopeTenant {
				t.Fatalf("receipt existence/value oracle: %d %s", code, body)
			}
			if scopeSnapshot(t, pool) != before {
				t.Fatal("revoked/reused request changed durable state")
			}
		}
	})
	t.Run("authorized_conflict_and_stale_preserved", func(t *testing.T) {
		before := scopeSnapshot(t, pool)
		for _, tc := range []struct{ id, request, revision string }{{ids["PF-B"], "claim-a", "1"}, {ids["PF-A"], "new-stale", "1"}} {
			code, body := scopeActionHTTP(t, h, scopeTenant, tc.id, tc.request, tc.revision, `["PF-A","PF-B"]`)
			if code != 409 {
				t.Fatalf("status=%d %s", code, body)
			}
		}
		if scopeSnapshot(t, pool) != before {
			t.Fatal("conflict/stale changed state")
		}
	})
	t.Run("concurrent_authorized_and_revoked_retries", func(t *testing.T) {
		const n = 12
		codes := make([]int, n)
		bodies := make([]string, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				scope := `["PF-B"]`
				if i%2 == 1 {
					scope = `["PF-A"]`
				}
				codes[i], bodies[i] = scopeActionHTTP(t, h, scopeTenant, ids["PF-B"], "claim-b", "1", scope)
			}()
		}
		close(start)
		wg.Wait()
		for i := range n {
			want := 200
			if i%2 == 1 {
				want = 404
			}
			if codes[i] != want {
				t.Fatalf("retry %d status=%d %s", i, codes[i], bodies[i])
			}
			if i%2 == 0 && bodies[i] != bodies[0] {
				t.Fatal("authorized concurrent retries returned different receipt")
			}
		}
		for _, table := range []string{"custody_actions", "outbox"} {
			var count int
			if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 2 {
				t.Fatalf("%s exactly once: count=%d %v", table, count, err)
			}
		}
		b, err := st.LoadBreak(t.Context(), ids["PF-B"])
		if err != nil || b.Revision != 2 || b.Assignee != "alice@kanz" {
			t.Fatalf("concurrent lifecycle %+v %v", b, err)
		}
	})
	t.Run("reopened_pool_and_server_reauthorize_original_receipt", func(t *testing.T) {
		reopened := scopePool(t, pool, scopeTenant)
		fresh := httptest.NewServer(New(&Readiness{}, nil, ledger.NewPostgres(reopened), "USD", WithTenant(scopeTenant), WithBreakStore(custody.NewPostgres(reopened))))
		defer fresh.Close()
		code, body := scopeActionHTTP(t, fresh, scopeTenant, ids["PF-A"], "claim-a", "1", `["PF-A"]`)
		if code != 200 || body != receipt {
			t.Fatalf("restart retry: %d %s", code, body)
		}
		code, body = scopeActionHTTP(t, fresh, scopeTenant, ids["PF-A"], "claim-a", "1", `["PF-B"]`)
		if code != 404 {
			t.Fatalf("restart revoked retry: %d %s", code, body)
		}
		code, body = scopedHTTP(t, fresh.Client(), fresh.URL, "GET", "/v1/custody/breaks", scopeTenant, "", `["PF-A"]`)
		if code != 200 || strings.Contains(body, ids["PF-B"]) || !strings.Contains(body, `"count":1`) || !strings.Contains(body, `"ibor":"100"`) {
			t.Fatalf("reopened filtered values/count: %d %s", code, body)
		}
	})
	t.Run("restricted_RLS_hides_resources_and_receipts", func(t *testing.T) {
		other := scopePool(t, pool, "scope-other")
		scopeRestricted(t, other)
		for _, table := range []string{"custody_breaks", "custody_actions", "outbox"} {
			var count int
			if err := other.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("foreign %s visible count=%d %v", table, count, err)
			}
		}
		fresh := httptest.NewServer(New(&Readiness{}, nil, ledger.NewPostgres(other), "USD", WithTenant("scope-other"), WithBreakStore(custody.NewPostgres(other))))
		defer fresh.Close()
		code, body := scopeActionHTTP(t, fresh, "scope-other", ids["PF-A"], "claim-a", "1", `["PF-A"]`)
		if code != 404 {
			t.Fatalf("foreign resource discovered: %d %s", code, body)
		}
	})
	t.Run("same_ID_in_another_tenant_remains_independent", func(t *testing.T) {
		before := scopeSnapshot(t, pool)
		other := scopePool(t, pool, "scope-independent")
		otherStore := custody.NewPostgres(other)
		subject := custody.Subject{PortfolioID: "PF-A", CustodianID: "C", BusinessDate: breakT0}
		b := custody.FromRecon(subject, []recon.Break{{Kind: recon.BreakQuantity, Key: "other-asset", IBOR: big.NewRat(777, 1), Custodian: big.NewRat(1, 1), Diff: big.NewRat(776, 1)}}, breakT0)
		b[0].BreakID = ids["PF-A"]
		if _, err := otherStore.UpsertBreaks(t.Context(), subject, b, breakT0); err != nil {
			t.Fatal(err)
		}
		fresh := httptest.NewServer(New(&Readiness{}, nil, ledger.NewPostgres(other), "USD", WithTenant("scope-independent"), WithBreakStore(otherStore)))
		defer fresh.Close()
		code, body := scopeActionHTTP(t, fresh, "scope-independent", ids["PF-A"], "claim-a", "1", `["PF-A"]`)
		if code != 200 || !strings.Contains(body, `"ibor":"777"`) || strings.Contains(body, `"ibor":"100"`) {
			t.Fatalf("tenant-owned same-ID receipt: %d %s", code, body)
		}
		if scopeSnapshot(t, pool) != before {
			t.Fatal("other tenant same-ID action changed original book/evidence")
		}
	})
}

func TestPostgresAccountingScopedFinancialReads(t *testing.T) {
	pool := newServerPool(t)
	scopeRestricted(t, pool)
	book := ledger.NewPostgres(pool)
	pairs := []custody.Subject{{PortfolioID: "PF-A", CustodianID: "C"}, {PortfolioID: "PF-B", CustodianID: "C"}}
	scope, err := custody.NewBookScope(pairs, map[string]map[string][]string{"PF-A": {"C": {"account-a"}}, "PF-B": {"C": {"account-b"}}})
	if err != nil {
		t.Fatal(err)
	}
	for i, pf := range []string{"PF-A", "PF-B"} {
		amount := big.NewRat(405, 4)
		if i == 1 {
			amount = big.NewRat(3639, 4)
		}
		if err := book.Append(t.Context(), &ledger.Event{EntryID: fmt.Sprintf("scope-cash-%d", i), PortfolioID: pf, VenueAccountID: []string{"account-a", "account-b"}[i], Type: ledger.EntryCash, Cash: amount, CashCurrency: "USD", Effective: breakT0, Knowledge: breakT0}, nil); err != nil {
			t.Fatal(err)
		}
	}
	reopened := scopePool(t, pool, scopeTenant)
	h := httptest.NewServer(New(&Readiness{}, nil, ledger.NewPostgres(reopened), "USD", WithTenant(scopeTenant), WithCustodyBookScope(scope)))
	defer h.Close()
	t.Run("authorized_NAV_is_the_selected_book", func(t *testing.T) {
		code, body := scopedHTTP(t, h.Client(), h.URL, "POST", "/v1/portfolios/PF-A/nav", scopeTenant, `{}`, `["PF-A"]`)
		if code != 200 || !strings.Contains(body, `"total":"101.25"`) || strings.Contains(body, "909.75") {
			t.Fatalf("NAV %d %s", code, body)
		}
	})
	t.Run("authorized_reconciliation_uses_selected_book", func(t *testing.T) {
		code, body := scopedHTTP(t, h.Client(), h.URL, "POST", "/v1/portfolios/PF-A/reconcile", scopeTenant, `{"custodian_id":"C","cash":{"USD":"101.25"}}`, `["PF-A"]`)
		if code != 200 || !strings.Contains(body, `"count":0`) || strings.Contains(body, "909.75") {
			t.Fatalf("comparison %d %s", code, body)
		}
	})
	t.Run("excluded_book_and_configuration_never_disclosed", func(t *testing.T) {
		for _, path := range []string{"nav", "reconcile"} {
			code, body := scopedHTTP(t, h.Client(), h.URL, "POST", "/v1/portfolios/PF-B/"+path, scopeTenant, `{}`, `["PF-A"]`)
			if code != 404 || strings.Contains(body, "909.75") || strings.Contains(body, "configured") {
				t.Fatalf("financial oracle %d %s", code, body)
			}
		}
	})
}
