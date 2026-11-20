package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

func TestExecutionEconomicsReadUsesTheAuthorizedOriginal(t *testing.T) {
	store := &historicalStore{t: t}
	store.MemoryStore = custody.NewMemoryStore()
	one, zero := 1, 0
	amount := func(c int64, exponent int32) *commonpb.Decimal {
		return &commonpb.Decimal{Coefficient: c, Exponent: exponent}
	}
	figures := func(price, settled int64) *accountingpb.ExecutionEconomicsFigures {
		return &accountingpb.ExecutionEconomicsFigures{Quantity: amount(1, 0), Price: amount(price, 0), Commission: amount(1, -1),
			GrossCash: amount(-price, 0), NetCash: amount(settled, -1), SettledCash: amount(settled, -1)}
	}
	comparison := &accountingpb.ExecutionEconomicsComparison{Rows: []*accountingpb.ExecutionEconomicsRow{
		{ExternalRef: "venue-7", InstrumentId: "SYN", CurrencyCode: "USD", EntryId: "journal-1", Status: "mismatch",
			Convention: "unit_price_net_cash_same_currency_commission_v1", BookSettlementDate: "2020-02-04", CustodianSettlementDate: "2020-02-04",
			Book: figures(9007199254740993, -90071992547409931), Custodian: figures(9007199254740992, -90071992547409921),
			Difference: &accountingpb.ExecutionEconomicsFigures{Quantity: amount(0, 0), Price: amount(1, 0), GrossCash: amount(-1, 0),
				Commission: amount(0, 0), NetCash: amount(-1, 0), SettledCash: amount(-1, 0)}},
	}}
	digest := strings.Repeat("a", 64)
	store.result = custody.DatedResult{State: "recorded", StatementID: "custody-batch.v1.source.r1." + digest, SignedStatementSHA256: digest, AccountAssignmentVerified: true, Run: custody.Run{RunID: "saved|run", Outcome: custody.OutcomeClean,
		Basis: &custody.RunBasis{Mode: custody.BasisDeclared, Accounts: map[string][]string{"C": {"ACC1"}}, Transactions: &custody.TransactionComparison{
			Pass: "ran", StatementReferences: &one, BookReferences: &one, UnreferencedBookTrades: &zero}},
		ExecutionEconomics: comparison}}
	scope, err := custody.NewBookScope([]custody.Subject{{PortfolioID: "PF1", CustodianID: "C"}}, map[string]map[string][]string{"PF1": {"C": {"ACC1"}}})
	if err != nil {
		t.Fatal(err)
	}
	router := New(&Readiness{}, nil, nil, "USD", WithTenant("reader"), WithBreakStore(store), WithCustodyBookScope(scope), WithReadOnlyCustodyReview())
	get := func(saved bool, tenant, portfolio string) (int, map[string]json.RawMessage) {
		t.Helper()
		path := "/v1/portfolios/PF1/custody/comparison?custodian_id=C&business_date=2020-02-04"
		if saved {
			path += "&run_id=saved%7Crun"
		}
		r := httptest.NewRequest("GET", path, nil)
		auth.SetPrincipalHeaders(r.Header, "reviewer", tenant, []string{"read"})
		if err := auth.SetPrincipalPortfolios(r.Header, []string{portfolio}); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		var reply map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		return w.Code, reply
	}
	for _, saved := range []bool{false, true} {
		for _, wrong := range [][2]string{{"reader", "other"}, {"other", "PF1"}} {
			code, r := get(saved, wrong[0], wrong[1])
			if code != 404 || r["execution_economics_status"] != nil || r["execution_economics"] != nil {
				t.Fatal(code, r)
			}
		}
		code, r := get(saved, "reader", "PF1")
		if code != 200 || string(r["execution_economics_status"]) != `"mismatch"` || string(r["outcome"]) != `"clean"` {
			t.Fatal(code, r)
		}
		if !saved && r["execution_economics"] != nil {
			t.Fatal("current lookup disclosed execution rows")
		}
		if saved {
			var detail struct {
				Rows []struct {
					Book struct {
						SettledCash struct {
							Coefficient string
							Exponent    int
						} `json:"settled_cash"`
					}
				}
			}
			if err := json.Unmarshal(r["execution_economics"], &detail); err != nil || len(detail.Rows) != 1 || detail.Rows[0].Book.SettledCash.Coefficient != "-90071992547409931" || detail.Rows[0].Book.SettledCash.Exponent != -1 {
				t.Fatal("inexact settlement", err)
			}
		}
	}
	store.result.AccountAssignmentVerified = false
	_, r := get(true, "reader", "PF1")
	if r["execution_economics"] != nil || r["execution_economics_status"] != nil {
		t.Fatal("unverified account disclosed economics")
	}
	store.result.AccountAssignmentVerified = true
	store.result.Run.ExecutionEconomics = nil
	_, r = get(true, "reader", "PF1")
	if r["execution_economics"] != nil || r["execution_economics_status"] != nil {
		t.Fatal("legacy receipt gained economics")
	}
}
