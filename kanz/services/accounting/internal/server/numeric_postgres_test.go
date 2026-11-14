package server

import (
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/jackc/pgx/v5/pgxpool"
)

func numericHistory(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var result []string
	for _, table := range []string{"ledger_entries", "ledger_snapshots", "ledger_heads", "ledger_append_positions", "execution_fee_revisions", "custody_statements", "custody_runs", "custody_breaks", "custody_actions", "outbox"} {
		var rows string
		if err := pool.QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(value ORDER BY value::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(t) AS value FROM `+table+` t) contents`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		result = append(result, table+":"+rows)
	}
	return strings.Join(result, "\n")
}

func TestPostgresAccountingNumericFinancialViews(t *testing.T) {
	pool := newServerPool(t)
	scopeRestricted(t, pool)
	seedNumericBook(t, ledger.NewPostgres(pool))
	reopened := scopePool(t, pool, scopeTenant)
	h := httptest.NewServer(New(&Readiness{}, nil, ledger.NewPostgres(reopened), "USD", WithTenant(scopeTenant), WithCustodyBookScope(testScope(t))))
	defer h.Close()
	initial := numericHistory(t, pool)
	t.Run("reopened_exact_fraction_NAV", func(t *testing.T) {
		// Independently: USD100 + three shares * USD(1/3) = USD101.
		code, body := numericRequest(t, h, scopeTenant, "nav", `{"prices":{"AAPL":"1/3"}}`, false, `["PF"]`)
		if code != 200 || !strings.Contains(body, `"total":"101.00"`) || !strings.Contains(body, `"security_value":"1.00"`) {
			t.Fatalf("reopened exact valuation: %d %s", code, body)
		}
	})
	t.Run("bounded_radix_FX_and_exact_NAV", func(t *testing.T) {
		// Three shares * EUR(1/3) * USD1.25/EUR + USD100 = USD101.25.
		code, body := numericRequest(t, h, scopeTenant, "nav", `{"prices":{"AAPL":"1/3"},"instrument_currency":{"AAPL":"EUR"},"fx":{"EUR":"0x1.4p0"}}`, false, `["PF"]`)
		if code != 200 || !strings.Contains(body, `"total":"101.25"`) || !strings.Contains(body, `"security_value":"1.25"`) {
			t.Fatalf("radix FX exact valuation: %d %s", code, body)
		}
	})
	t.Run("reconciliation_tolerance_boundary", func(t *testing.T) {
		for _, tc := range []struct{ tolerance, count string }{{".25", `"count":0`}, {".125", `"count":1`}, {"0", `"count":1`}} {
			code, body := numericRequest(t, h, scopeTenant, "reconcile", `{"custodian_id":"CUST-A","positions":{"AAPL":"3.25"},"cash":{"USD":"100"},"tolerance":"`+tc.tolerance+`"}`, false, `["PF"]`)
			if code != 200 || !strings.Contains(body, tc.count) {
				t.Fatalf("inclusive exact tolerance %q: %d %s", tc.tolerance, code, body)
			}
		}
	})
	t.Run("all_five_refusals_leave_history_unchanged", func(t *testing.T) {
		for _, field := range numericFieldCases() {
			code, body := numericRequest(t, h, scopeTenant, field.path, strings.ReplaceAll(field.template, "VALUE", "1e155"), false, `["PF"]`)
			if code != 400 || numericHistory(t, pool) != initial {
				t.Fatalf("%s refusal changed journal/snapshot/lifecycle/evidence/outbox: %d %s", field.name, code, body)
			}
		}
	})
	t.Run("valid_recovery_after_refusal", func(t *testing.T) {
		code, body := numericRequest(t, h, scopeTenant, "nav", `{"prices":{"AAPL":"+0x1.8p1"}}`, true, `["PF"]`)
		if code != 200 || !strings.Contains(body, `"total":"109.00"`) || numericHistory(t, pool) != initial {
			t.Fatalf("valid retry after refusals: %d %s", code, body)
		}
	})
	t.Run("restricted_RLS_and_same_ID_isolation", func(t *testing.T) {
		other := scopePool(t, pool, "numeric-other")
		scopeRestricted(t, other)
		var count int
		if err := other.QueryRow(t.Context(), `SELECT count(*) FROM ledger_entries`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("foreign journal visible count=%d %v", count, err)
		}
		if err := ledger.NewPostgres(other).Append(t.Context(), &ledger.Event{EntryID: "numeric-opening", PortfolioID: "PF", VenueAccountID: "okx-sub-1", Type: ledger.EntryCash, Cash: big.NewRat(777, 1), CashCurrency: "USD", Effective: breakT0, Knowledge: breakT0}, nil); err != nil {
			t.Fatal(err)
		}
		foreign := httptest.NewServer(New(&Readiness{}, nil, ledger.NewPostgres(other), "USD", WithTenant("numeric-other")))
		defer foreign.Close()
		code, body := numericRequest(t, foreign, "numeric-other", "nav", `{}`, false, `["PF"]`)
		if code != 200 || !strings.Contains(body, `"total":"777.00"`) {
			t.Fatalf("tenant-owned same-ID book: %d %s", code, body)
		}
		code, body = numericRequest(t, foreign, scopeTenant, "nav", `{}`, false, `["PF"]`)
		if code != 404 || strings.Contains(body, "777") {
			t.Fatalf("foreign tenant financial oracle: %d %s", code, body)
		}
		if numericHistory(t, pool) != initial {
			t.Fatal("another tenant changed the original journal/history/outbox")
		}
	})
}
