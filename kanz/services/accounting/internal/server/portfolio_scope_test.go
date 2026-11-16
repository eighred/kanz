package server

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/collateralops"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

func scopedHTTP(t *testing.T, client *http.Client, base, method, path, tenant, body string, scope ...string) (int, string) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	auth.SetPrincipalHeaders(r.Header, "alice@kanz", tenant, []string{"funder"})
	for _, value := range scope {
		r.Header.Add(auth.HeaderPrincipalPortfolios, value)
	}
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b)
}

func scopeBreaks(t *testing.T) *custody.MemoryStore {
	t.Helper()
	st := custody.NewMemoryStore()
	for i, pf := range []string{"PF-A", "PF-B", "PF/escaped"} {
		subject := custody.Subject{PortfolioID: pf, CustodianID: "C", BusinessDate: breakT0}
		detected := custody.FromRecon(subject, []recon.Break{{Kind: recon.BreakQuantity, Key: pf + "-asset", IBOR: big.NewRat(int64(i+101), 1), Custodian: big.NewRat(1, 1), Diff: big.NewRat(int64(i+100), 1)}}, breakT0)
		if _, err := st.UpsertBreaks(t.Context(), subject, detected, breakT0); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestAccountingPortfolioReadScope(t *testing.T) {
	s := New(&Readiness{}, nil, ledger.NewMemoryStore(), "USD", WithTenant(testTenant))
	httpServer := httptest.NewServer(s)
	defer httpServer.Close()
	for _, endpoint := range []string{"nav", "reconcile"} {
		t.Run(endpoint, func(t *testing.T) {
			for _, tc := range []struct {
				name, pf string
				scope    []string
				want     int
			}{
				{"excluded_before_decode_or_configuration", "PF-B", []string{`["PF-A"]`}, 404},
				{"unknown_resource_same_refusal", "absent", []string{`["PF-A"]`}, 404},
				{"exact_member_reaches_decoder", "PF-A", []string{`["PF-A"]`}, 400},
				{"prefix_is_not_membership", "PF-A-other", []string{`["PF-A"]`}, 404},
				{"escaped_exact_member", "PF/escaped", []string{`["PF/escaped"]`}, 400},
				{"escaped_nonmember", "PF/escaped", []string{`["PF"]`}, 404},
				{"absent_read_scope", "PF-B", nil, 400},
				{"empty_read_scope", "PF-B", []string{`[]`}, 400},
				{"malformed", "PF-A", []string{`{}`}, 404},
				{"null", "PF-A", []string{`null`}, 404},
				{"repeated", "PF-A", []string{`["PF-A"]`, `["PF-A"]`}, 404},
			} {
				t.Run(tc.name, func(t *testing.T) {
					code, body := scopedHTTP(t, httpServer.Client(), httpServer.URL, "POST", "/v1/portfolios/"+url.PathEscape(tc.pf)+"/"+endpoint, testTenant, "{", tc.scope...)
					if code != tc.want {
						t.Fatalf("status=%d body=%s want=%d", code, body, tc.want)
					}
					if code == 404 && body != "{\"error\":\"portfolio not found\"}\n" {
						t.Fatalf("scope oracle: %s", body)
					}
				})
			}
		})
	}
	t.Run("denied_valid_request_never_reads_book", func(t *testing.T) {
		// Any attempted financial read panics through this deliberately absent
		// store; authorization must finish before materialization.
		unreadable := New(&Readiness{}, nil, ledger.NewMemoryStore(), "USD", WithTenant(testTenant))
		unreadable.store = nil
		h := httptest.NewServer(unreadable)
		defer h.Close()
		code, body := scopedHTTP(t, h.Client(), h.URL, "POST", "/v1/portfolios/PF-B/nav", testTenant, `{}`, `["PF-A"]`)
		if code != 404 {
			t.Fatalf("status=%d %s", code, body)
		}
	})
}

func TestAccountingCustodyQueuePortfolioScope(t *testing.T) {
	s := breakServer(t, scopeBreaks(t))
	httpServer := httptest.NewServer(s)
	defer httpServer.Close()
	for _, tc := range []struct {
		name  string
		scope []string
		ids   []string
		want  int
	}{
		{"only_A_values_and_count", []string{`["PF-A"]`}, []string{"PF-A"}, 200},
		{"only_B_values_and_count", []string{`["PF-B"]`}, []string{"PF-B"}, 200},
		{"escaped_exact_owner", []string{`["PF/escaped"]`}, []string{"PF/escaped"}, 200},
		{"no_matching_owner", []string{`["absent"]`}, nil, 200},
		{"absent_read_scope", nil, []string{"PF-A", "PF-B", "PF/escaped"}, 200},
		{"empty_read_scope", []string{`[]`}, []string{"PF-A", "PF-B", "PF/escaped"}, 200},
		{"malformed", []string{`{}`}, nil, 404},
		{"repeated", []string{`[]`, `[]`}, nil, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := scopedHTTP(t, httpServer.Client(), httpServer.URL, "GET", "/v1/custody/breaks", testTenant, "", tc.scope...)
			if code != tc.want {
				t.Fatalf("status=%d %s", code, body)
			}
			if code != 200 {
				return
			}
			var response struct {
				Breaks []struct {
					ID   string `json:"break_id"`
					IBOR string `json:"ibor"`
				}
				Count int
			}
			if err := json.Unmarshal([]byte(body), &response); err != nil {
				t.Fatal(err)
			}
			if response.Count != len(tc.ids) || len(response.Breaks) != len(tc.ids) {
				t.Fatalf("unfiltered count/values: %s", body)
			}
			for _, b := range response.Breaks {
				found := false
				for _, id := range tc.ids {
					if b.ID == custody.BreakID(id, "C", recon.BreakQuantity, id+"-asset") {
						found = true
					}
				}
				if !found || b.IBOR == "" {
					t.Fatalf("foreign or missing financial value: %s", body)
				}
			}
		})
	}
}

func TestAccountingVolatileActionScope(t *testing.T) {
	s := breakServer(t, scopeBreaks(t))
	httpServer := httptest.NewServer(s)
	defer httpServer.Close()
	for _, route := range []string{"actions", "assign", "explain"} {
		t.Run(route, func(t *testing.T) {
			body := `{"request_id":"denied","expected_revision":"1","action":"claim"}`
			if route == "explain" {
				body = `{"request_id":"denied","expected_revision":"1","explanation":"review"}`
			}
			for _, tc := range []struct {
				name, pf, scope string
				want            int
			}{
				{"excluded", "PF-B", `["PF-A"]`, 404},
				{"missing", "absent", `["PF-A"]`, 404},
				{"authorized_still_requires_durable_store", "PF-A", `["PF-A"]`, 503},
				{"empty_scope_preserves_operator_policy", "PF-A", `[]`, 503},
				{"malformed_scope", "PF-A", `{}`, 403},
			} {
				t.Run(tc.name, func(t *testing.T) {
					id := custody.BreakID(tc.pf, "C", recon.BreakQuantity, tc.pf+"-asset")
					code, result := scopedHTTP(t, httpServer.Client(), httpServer.URL, "POST", "/v1/custody/breaks/"+url.PathEscape(id)+"/"+route, testTenant, body, tc.scope)
					if code != tc.want {
						t.Fatalf("status=%d want=%d %s", code, tc.want, result)
					}
				})
			}
		})
	}
	rows, err := s.breaks.OutstandingBreaks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if b.Revision != 1 || b.Assignee != "" {
			t.Fatalf("denied/volatile action mutated %+v", b)
		}
	}
}

type scopeUnownedStore struct{ *custody.MemoryStore }

func (scopeUnownedStore) OutstandingBreaks(context.Context) ([]custody.Break, error) {
	return []custody.Break{{BreakID: "PF-A|C|quantity|unattributed", Status: custody.BreakOpen, IBOR: big.NewRat(777, 1), Custodian: big.NewRat(1, 1), Diff: big.NewRat(776, 1), ValuesVerified: true}}, nil
}

func TestAccountingUnattributedBreakIsNotServed(t *testing.T) {
	s := New(&Readiness{}, nil, ledger.NewMemoryStore(), "USD", WithTenant(testTenant), WithBreakStore(scopeUnownedStore{custody.NewMemoryStore()}))
	h := httptest.NewServer(s)
	defer h.Close()
	for _, tc := range []struct {
		name  string
		scope []string
	}{{"empty_read_scope", nil}, {"ID_prefix_cannot_assert_owner", []string{`["PF-A"]`}}} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := scopedHTTP(t, h.Client(), h.URL, "GET", "/v1/custody/breaks", testTenant, "", tc.scope...)
			if code != 200 || !strings.Contains(body, `"count":0`) || strings.Contains(body, "777") || strings.Contains(body, "unattributed") {
				t.Fatalf("unknown ownership disclosed: %d %s", code, body)
			}
		})
	}
}

func TestAccountingCapitalScopeRemainsExplicit(t *testing.T) {
	s := New(&Readiness{}, nil, ledger.NewMemoryStore(), "USD", WithTenant(testTenant), WithCashCommands(cashmove.NewCommands(nil)), WithCollateral(&collateralops.Store{}))
	h := httptest.NewServer(s)
	defer h.Close()
	for _, tc := range []struct{ name, method, path string }{
		{"cash_command", "POST", "cash-movements"},
		{"cash_preview", "POST", "cash-movements/preview"},
		{"cash_receipt", "GET", "cash-movements/M"},
		{"cash_forecast", "GET", "cash-forecast"},
		{"collateral_action", "POST", "collateral/actions"},
		{"collateral_read", "GET", "collateral/W"},
		{"collateral_proof", "GET", "collateral/W/proof"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, scope := range [][]string{nil, {`[]`}, {`["PF-B"]`}, {`{}`}, {`[]`, `[]`}} {
				code, body := scopedHTTP(t, h.Client(), h.URL, tc.method, "/v1/portfolios/PF-A/"+tc.path, testTenant, `{"expected_revision":"1"}`, scope...)
				if code != 404 {
					t.Fatalf("capital default broadened: %d %s", code, body)
				}
			}
		})
	}
}
