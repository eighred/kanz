package server

import (
	"encoding/json"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostgresCashCommandHTTPReviewAndScope(t *testing.T) {
	pool := newServerPool(t)
	var privileged bool
	if err := pool.QueryRow(t.Context(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil || privileged {
		t.Fatalf("restricted role required: %v %v", privileged, err)
	}
	scope, err := custody.NewBookScope([]custody.Subject{{PortfolioID: "PF", CustodianID: "C"}, {PortfolioID: "OTHER", CustodianID: "D"}}, map[string]map[string][]string{"PF": {"C": {"account"}}, "OTHER": {"D": {"foreign"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(&Readiness{}, nil, ledger.NewPostgres(pool), "USD", WithTenant("__system__"), WithCashCommands(cashmove.NewCommands(pool)), WithCustodyBookScope(scope))
	body := map[string]any{"movement_id": "M1", "kind": "subscription", "amount": "0.000000001", "currency": "USDT", "effective": "2026-03-01T00:00:00Z", "source_ref": "transfer-123", "venue_account_id": "account", "reason": "reviewed transfer evidence"}
	call := func(method, path, tenant, actor, portfolio string) *httptest.ResponseRecorder {
		b, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		req := httptest.NewRequest(method, "/v1/portfolios/PF/cash-movements"+path, strings.NewReader(string(b)))
		auth.SetPrincipalHeaders(req.Header, actor, tenant, []string{"funder"})
		if e = auth.SetPrincipalPortfolios(req.Header, []string{portfolio}); e != nil {
			t.Fatal(e)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}
	good := func(method, path string) *httptest.ResponseRecorder {
		return call(method, path, "__system__", "maker", "PF")
	}
	for _, tc := range []struct{ tenant, actor, pf string }{{"foreign", "maker", "PF"}, {"__system__", "maker", "OTHER"}, {"__system__", "", "PF"}} {
		if r := call("POST", "/preview", tc.tenant, tc.actor, tc.pf); r.Code != 404 {
			t.Fatalf("scope: %d", r.Code)
		}
	}
	for _, account := range []string{"foreign", "undeclared"} {
		body["venue_account_id"] = account
		if r := good("POST", "/preview"); r.Code != 404 {
			t.Fatalf("account %s: %d", account, r.Code)
		}
	}
	body["venue_account_id"] = "account"
	for _, field := range []string{"currency", "effective", "source_ref", "reason", "venue_account_id"} {
		old := body[field]
		delete(body, field)
		if r := good("POST", "/preview"); r.Code != 400 {
			t.Fatalf("missing %s: %d", field, r.Code)
		}
		body[field] = old
	}
	for _, amount := range []string{"1/3", "1e1000000", "9223372036854775809.1", strings.Repeat("1", 401)} {
		body["amount"] = amount
		if r := good("POST", "/preview"); r.Code != 400 {
			t.Fatalf("amount: %d", r.Code)
		}
	}
	body["amount"] = "0.000000001"
	body["actor"] = "spoof"
	if r := good("POST", "/preview"); r.Code != 400 {
		t.Fatalf("spoof: %d", r.Code)
	}
	delete(body, "actor")
	if r := good("POST", ""); r.Code != 409 {
		t.Fatalf("missing review: %d", r.Code)
	}
	preview := good("POST", "/preview")
	if preview.Code != 200 {
		t.Fatal(preview.Body.String())
	}
	var review map[string]string
	if err = json.Unmarshal(preview.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	body["reviewed_digest"] = review["reviewed_digest"]
	body["amount"] = "1"
	if r := good("POST", ""); r.Code != 409 {
		t.Fatalf("changed reviewed terms: %d", r.Code)
	}
	body["amount"] = "0.000000001"
	first := good("POST", "")
	if first.Code != 202 {
		t.Fatal(first.Body.String())
	}
	if retry := good("POST", ""); retry.Code != 202 || retry.Body.String() != first.Body.String() {
		t.Fatalf("unstable receipt: %d %s", retry.Code, retry.Body.String())
	}
	var actor, account string
	if err = pool.QueryRow(t.Context(), `SELECT actor,command->>'VenueAccountID' FROM cash_commands WHERE movement_id='M1'`).Scan(&actor, &account); err != nil || actor != "maker" || account != "account" {
		t.Fatalf("durable attribution: %s %s %v", actor, account, err)
	}
	if r := good("GET", "/M1"); r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"accepted"`) {
		t.Fatalf("status: %d %s", r.Code, r.Body.String())
	}
	body["movement_id"] = "M2"
	body["venue_account_id"] = ""
	delete(body, "reviewed_digest")
	preview = good("POST", "/preview")
	if preview.Code != 200 {
		t.Fatalf("explicit no exchange account: %d", preview.Code)
	}
}
