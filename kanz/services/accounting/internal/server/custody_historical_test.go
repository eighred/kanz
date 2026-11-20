package server

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

type historicalStore struct {
	dateStore
	t    *testing.T
	hits int
}

func (s *historicalStore) HistoricalReceipt(ctx context.Context, sub custody.Subject, account, id string) (custody.DatedResult, error) {
	s.t.Helper()
	if bus.TenantIDFromContext(ctx) != "reader" || sub.PortfolioID != "PF1" || sub.CustodianID != "C" || sub.BusinessDate.Format("2006-01-02") != "2020-02-04" || account != "ACC1" || id != "saved|run" {
		s.t.Fatalf("invalid scoped read %v %q %q", sub, account, id)
	}
	s.hits++
	return s.result, nil
}

func TestHistoricalCustodyProjection(t *testing.T) {
	store := &historicalStore{t: t}
	store.MemoryStore = custody.NewMemoryStore()
	store.result = custody.DatedResult{State: "recorded", StatementID: "r1", SignedStatementSHA256: strings.Repeat("a", 64), Run: custody.Run{RunID: "saved|run", Outcome: custody.OutcomeBreaks,
		Basis:  &custody.RunBasis{Mode: custody.BasisDeclared, JournalPosition: 42, Accounts: map[string][]string{"C": {"ACC1"}}},
		Breaks: []custody.Break{{BreakID: "id", Key: "clé", IBOR: big.NewRat(9007199254740993, 1), Custodian: big.NewRat(9007199254740991, 1), Diff: big.NewRat(2, 1), ValuesVerified: true, Assignee: "private", Explanation: "do not disclose"}}}}
	scope, err := custody.NewBookScope([]custody.Subject{{PortfolioID: "PF1", CustodianID: "C"}}, map[string]map[string][]string{"PF1": {"C": {"ACC1"}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(&Readiness{}, nil, nil, "USD", WithTenant("reader"), WithBreakStore(store), WithCustodyBookScope(scope), WithReadOnlyCustodyReview())
	get := func(prefix, q, tenant, pf string) (int, string) {
		r := httptest.NewRequest("GET", "/v1/portfolios/"+prefix+"/custody/comparison?"+q, nil)
		auth.SetPrincipalHeaders(r.Header, "accountant", tenant, []string{"read"})
		if err := auth.SetPrincipalPortfolios(r.Header, []string{pf}); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	base := "custodian_id=C&business_date=2020-02-04"
	query := base + "&run_id=" + url.QueryEscape("saved|run")
	code, body := get("PF1", query, "reader", "PF1")
	var receipt struct {
		Tenant string `json:"tenant_id"`
		Pass   string `json:"transaction_pass"`
		Breaks []map[string]string
	}
	if code != 200 || json.Unmarshal([]byte(body), &receipt) != nil || len(receipt.Breaks) != 1 || receipt.Tenant != "reader" || receipt.Pass != "unknown" {
		t.Fatal(code, body)
	}
	row := receipt.Breaks[0]
	if len(row) != 7 || row["key"] != "clé" || row["ibor"] != "9007199254740993" || row["custodian"] != "9007199254740991" || row["difference"] != "2" || row["values_state"] != "exact" || strings.Contains(body, "private") || strings.Contains(body, "do not disclose") {
		t.Fatal(body)
	}
	for _, bad := range []string{base + "&run_id=", query + "&run_id=another", query + "&extra=x", base + "&run_id=%0A", base + "&run_id=%FF", base + "&run_id=" + strings.Repeat("x", 513), base + "&run_id=+padded"} {
		if code, _ := get("PF1", bad, "reader", "PF1"); code != 400 {
			t.Fatal(bad, code)
		}
	}
	for _, bad := range [][2]string{{"other", "PF1"}, {"reader", "PF2"}} {
		if code, b := get("PF1", query, bad[0], bad[1]); code != 404 || strings.Contains(b, "saved") {
			t.Fatal(code, b)
		}
	}
	if store.hits != 1 || store.calls != 0 {
		t.Fatalf("historical=%d current=%d", store.hits, store.calls)
	}
}
