package server

// THE AD-HOC RECONCILE ENDPOINT COMPARES ONE CUSTODIAN'S BOOK (#1025).
//
// #1006 scoped the book side of the SCHEDULED comparison to the custodian on the
// subject. It did not touch this endpoint, which materialized the WHOLE portfolio
// and handed it to the one custodian's statement in the request body — so for a
// portfolio custodied in two places an operator posting custodian A's statement
// got every position held at B reported as a break.
//
// It is worse here than on the scheduled path in one respect: an ad-hoc
// reconciliation is what an operator runs WHILE INVESTIGATING, so the fabricated
// breaks arrive exactly when somebody is trying to read the real one.
//
// The refusals below are the other half. A custodian this deployment cannot place
// has no declared accounts, so the only book the handler could answer with is the
// whole portfolio — the defect, returned as a 200. It refuses instead, and names
// the configuration.

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

const (
	dualPortfolio     = "PF-DUAL"
	unmappedPortfolio = "PF-UNMAPPED"
	custodianA        = "CUST-A"
	custodianB        = "CUST-B"
)

// testScope is the declaration every server in this package is built over: the
// single-custodian portfolio the older tests use, the two-custodian one this file
// exists for, and one whose journal touches an account nobody claims.
func testScope(t *testing.T) *custody.BookScope {
	t.Helper()
	scope, err := custody.NewBookScope(
		[]custody.Subject{
			{PortfolioID: "PF", CustodianID: custodianA},
			{PortfolioID: dualPortfolio, CustodianID: custodianA},
			{PortfolioID: dualPortfolio, CustodianID: custodianB},
			{PortfolioID: unmappedPortfolio, CustodianID: custodianA},
			{PortfolioID: unmappedPortfolio, CustodianID: custodianB},
		},
		map[string]map[string][]string{
			dualPortfolio:     {custodianA: {"okx-sub-1"}, custodianB: {"bin-main"}},
			unmappedPortfolio: {custodianA: {"okx-sub-1"}, custodianB: {"bin-main"}},
		},
	)
	if err != nil {
		t.Fatalf("NewBookScope: %v", err)
	}
	return scope
}

// accountTrade is one settled trade against a named exchange account.
func accountTrade(id, portfolio, account, instrument string, qty, cash int64) *ledger.Event {
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	return &ledger.Event{
		EntryID:        id,
		PortfolioID:    portfolio,
		VenueAccountID: account,
		Type:           ledger.EntryTrade,
		InstrumentID:   instrument,
		Quantity:       big.NewRat(qty, 1),
		Price:          big.NewRat(1, 1),
		Cash:           big.NewRat(cash, 1),
		CashCurrency:   "USD",
		Effective:      at,
		Knowledge:      at,
		SourceRef:      id,
	}
}

func seedDual(t *testing.T, store ledger.Store) {
	t.Helper()
	for _, e := range []*ledger.Event{
		accountTrade("d1", dualPortfolio, "okx-sub-1", "AAPL", 100, -15000),
		accountTrade("d2", dualPortfolio, "bin-main", "MSFT", 250, -25000),
	} {
		if err := store.Append(context.Background(), e, nil); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}
}

// reconcileAs posts a statement for one custodian and returns the recorder.
func reconcileAs(t *testing.T, s *Server, portfolio, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/"+portfolio+"/reconcile", strings.NewReader(body)))
	return rec
}

type reconcileResponse struct {
	PortfolioID string              `json:"portfolio_id"`
	CustodianID string              `json:"custodian_id"`
	Count       int                 `json:"count"`
	Breaks      []map[string]string `json:"breaks"`
	Error       string              `json:"error"`
}

func decodeReconcile(t *testing.T, rec *httptest.ResponseRecorder) reconcileResponse {
	t.Helper()
	var out reconcileResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// THE DEFECT, DIRECTLY. PF-DUAL holds AAPL at CUST-A and MSFT at CUST-B. Posting
// CUST-A's statement — which names AAPL and nothing else, because CUST-A does not
// hold MSFT and never will — must reconcile CLEAN.
//
// Before #1025 the handler folded the whole portfolio, so MSFT came back as
// MISSING_AT_CUSTODIAN and the operator investigating a real break saw a
// fabricated one beside it. Both directions are asserted, because a scoping bug
// that folded an EMPTY book would also pass a one-sided check — it would report
// the custodian's own holdings as MISSING_IN_IBOR instead.
func TestReconcileComparesOnlyTheNamedCustodiansBook(t *testing.T) {
	s, store := newServer(t)
	seedDual(t, store)

	for _, tc := range []struct {
		custodian string
		body      string
	}{
		{custodianA, `{"custodian_id":"CUST-A","positions":{"AAPL":"100"},"cash":{"USD":"-15000"}}`},
		{custodianB, `{"custodian_id":"CUST-B","positions":{"MSFT":"250"},"cash":{"USD":"-25000"}}`},
	} {
		rec := reconcileAs(t, s, dualPortfolio, tc.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", tc.custodian, rec.Code, rec.Body.String())
		}
		got := decodeReconcile(t, rec)
		if got.Count != 0 {
			t.Errorf("%s: %d break(s) against a statement that matches its own holdings: %v\n\n"+
				"The book side was not scoped to this custodian, so the statement was compared "+
				"against every position the portfolio holds anywhere. Each position held at the "+
				"OTHER custodian becomes a break, and the one break that means a fill never reached "+
				"the ledger is buried in them (#1006/#1025).", tc.custodian, got.Count, got.Breaks)
		}
		if got.CustodianID != tc.custodian {
			t.Errorf("%s: response names custodian %q — a break list an operator pastes into an "+
				"investigation must say whose book it compared", tc.custodian, got.CustodianID)
		}
	}
}

// AND THE SCOPING MUST NOT SWALLOW A REAL BREAK. The same portfolio, the same
// custodian, a statement that disagrees — one break and only one.
func TestReconcileStillReportsARealBreakWithinOneCustodiansBook(t *testing.T) {
	s, store := newServer(t)
	seedDual(t, store)

	rec := reconcileAs(t, s, dualPortfolio,
		`{"custodian_id":"CUST-A","positions":{"AAPL":"90"},"cash":{"USD":"-15000"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeReconcile(t, rec)
	if got.Count != 1 {
		t.Fatalf("want exactly 1 break (AAPL 100 vs 90), got %d: %v", got.Count, got.Breaks)
	}
	if got.Breaks[0]["key"] != "AAPL" {
		t.Fatalf("break is on %q, want AAPL", got.Breaks[0]["key"])
	}
}

// A STATEMENT BELONGS TO ONE CUSTODIAN, SO THE REQUEST MUST NAME ONE. Not
// defaulted, not inferred from a portfolio that happens to have a single
// custodian: a guessed custodian compares a book slice nobody asked about and
// presents the result as an answer about the one they did.
func TestReconcileRefusesAStatementThatNamesNoCustodian(t *testing.T) {
	s, store := newServer(t)
	seedDual(t, store)

	rec := reconcileAs(t, s, dualPortfolio, `{"positions":{"AAPL":"100"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a statement with no custodian would be compared against "+
			"the whole portfolio, breaking every position held elsewhere. Body: %s",
			rec.Code, rec.Body.String())
	}
	// THE REFUSAL MUST BE THE REQUIRED-FIELD ONE. The unknown-pair refusal below
	// also mentions custodian_id, so a laxer assertion here would be satisfied by
	// deleting this check entirely — the empty string simply falls through and
	// matches no configured custodian.
	if !strings.Contains(decodeReconcile(t, rec).Error, "custodian_id is required") {
		t.Errorf("the refusal does not say custodian_id is required, so it does not say what to fix: %s",
			rec.Body.String())
	}
}

// A CUSTODIAN THIS DEPLOYMENT CANNOT PLACE IS A REFUSAL, NOT A WHOLE-BOOK ANSWER.
//
// An unrecognised custodian has no declared exchange accounts, so the only book
// available to answer with is the whole portfolio — the defect, wearing a 200.
// The refusal names the configuration because "you typed it wrong" and "nobody
// declared it" are both live and an operator can act on either.
func TestReconcileRefusesACustodianThatIsNotAConfiguredPair(t *testing.T) {
	s, store := newServer(t)
	seedDual(t, store)

	rec := reconcileAs(t, s, dualPortfolio, `{"custodian_id":"CUST-NOBODY","positions":{"AAPL":"100"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. Body: %s", rec.Code, rec.Body.String())
	}
	msg := decodeReconcile(t, rec).Error
	for _, want := range []string{"CUST-NOBODY", "ACCOUNTING_CUSTODY_PAIRS", custodianA} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q, so it names neither the mistake nor the "+
				"configuration that would fix it: %s", want, msg)
		}
	}
}

// NOTHING CONFIGURED AND CHECKED-AND-FINE MUST NOT LOOK THE SAME. A server built
// without the custody declaration can place no custodian at all, so it reconciles
// nothing — rather than falling back to the whole portfolio, which is the answer
// that looks healthy and is wrong.
func TestReconcileFailsClosedWithNoCustodyScopeWired(t *testing.T) {
	store := ledger.NewMemoryStore()
	r := &Readiness{}
	r.Set(true)
	s := New(r, nil, store, "USD", WithTenant(testTenant)) // no WithCustodyBookScope
	seedDual(t, store)

	rec := reconcileAs(t, s, dualPortfolio, `{"custodian_id":"CUST-A","positions":{"AAPL":"100"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. A deployment that wired no custody declaration must refuse "+
			"and say so, not compare one custodian's statement against every holding. Body: %s",
			rec.Code, rec.Body.String())
	}
}

// A PARTIAL BOOK MUST NOT RECONCILE. PF-UNMAPPED holds value in an account no
// custodian claims, so the fold would silently omit it and break every position
// it omitted. The loader refuses and names the account; the handler surfaces that
// rather than answering with the slice it could build.
func TestReconcileRefusesWhenTheJournalTouchesAnUnclaimedAccount(t *testing.T) {
	s, store := newServer(t)
	for _, e := range []*ledger.Event{
		accountTrade("u1", unmappedPortfolio, "okx-sub-1", "AAPL", 100, -15000),
		accountTrade("u2", unmappedPortfolio, "okx-sub-9", "TSLA", 40, -8000),
	} {
		if err := store.Append(context.Background(), e, nil); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}

	rec := reconcileAs(t, s, unmappedPortfolio, `{"custodian_id":"CUST-A","positions":{"AAPL":"100"}}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("a portfolio holding value in an unclaimed account reconciled anyway: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "okx-sub-9") {
		t.Errorf("the refusal does not name the unclaimed account, so nobody knows what to declare: %s",
			rec.Body.String())
	}
}
