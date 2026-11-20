package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

type dateStore struct {
	*custody.MemoryStore
	calls  int
	result custody.DatedResult
}

func (d *dateStore) DatedResult(ctx context.Context, s custody.Subject) (custody.DatedResult, error) {
	if bus.TenantIDFromContext(ctx) == "reader" && s.PortfolioID == "PF1" && s.CustodianID == "C" && s.BusinessDate.Format("2006-01-02") == "2020-02-04" {
		d.calls++
	}
	return d.result, nil
}
func TestDatedCustodyReadOnlyScope(t *testing.T) {
	st := &dateStore{MemoryStore: custody.NewMemoryStore(), result: custody.DatedResult{State: "not_compared"}}
	scope, err := custody.NewBookScope([]custody.Subject{{PortfolioID: "PF1", CustodianID: "C"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(&Readiness{}, nil, nil, "USD", WithTenant("reader"), WithBreakStore(st), WithCustodyBookScope(scope), WithReadOnlyCustodyReview())
	for _, tc := range []struct {
		tenant, pf, query string
		code              int
	}{
		{"reader", "PF1", "custodian_id=C&business_date=2020-02-04", 200},
		{"other", "PF1", "custodian_id=C&business_date=2020-02-04", 404},
		{"reader", "PF2", "custodian_id=C&business_date=2020-02-04", 404},
		{"reader", "PF1", "custodian_id=X&business_date=2020-02-04", 404},
		{"reader", "PF1", "custodian_id=C&business_date=2020-02-30", 400},
		{"reader", "PF1", "custodian_id=C&business_date=2020-02-04&custodian_id=X", 400},
	} {
		r := httptest.NewRequest("GET", "/v1/portfolios/PF1/custody/comparison?"+tc.query, nil)
		auth.SetPrincipalHeaders(r.Header, "scoped-reader", tc.tenant, []string{"read"})
		if err := auth.SetPrincipalPortfolios(r.Header, []string{tc.pf}); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s %s %s: %d", tc.tenant, tc.pf, tc.query, w.Code)
		}
	}
	// A recorded source from the custodian's old account must not leak as
	// the answer for its newly assigned account.
	st.result = custody.DatedResult{State: "recorded", StatementID: "old-source", Run: custody.Run{Outcome: custody.OutcomeClean, RunID: "old-run", Basis: &custody.RunBasis{Mode: custody.BasisDeclared, Accounts: map[string][]string{"C": {"old-account"}}}}}
	r := httptest.NewRequest("GET", "/v1/portfolios/PF1/custody/comparison?custodian_id=C&business_date=2020-02-04", nil)
	auth.SetPrincipalHeaders(r.Header, "scoped-reader", "reader", []string{"read"})
	if err := auth.SetPrincipalPortfolios(r.Header, []string{"PF1"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var masked map[string]string
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &masked) != nil || masked["state"] != "unknown" || masked["statement_id"] != "" || masked["run_id"] != "" {
		t.Fatal("unbound source exposed")
	}
	if st.calls != 2 {
		t.Fatal("denied scope reached the store")
	}
}
