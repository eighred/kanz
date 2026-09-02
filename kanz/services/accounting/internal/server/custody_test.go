package server

// Operator-surface tests for the custody break queue (#962).
//
// THE SURFACE IS THE HALF THAT MAKES THE LIFECYCLE REAL. Assign/Explain/Resolve
// are unit-tested in the custody package; these prove they are REACHABLE, that
// the tenant gate covers them, and that the two refusals which stop the control
// being silenced by hand survive the HTTP layer.

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

var breakT0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func custodySubject() custody.Subject {
	return custody.Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: breakT0}
}

func breakID() string {
	return custody.BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")
}

// seededBreakStore returns a store holding one outstanding quantity break.
func seededBreakStore(t *testing.T) *custody.MemoryStore {
	t.Helper()
	store := custody.NewMemoryStore()
	detected := custody.FromRecon(custodySubject(), []recon.Break{{
		Kind: recon.BreakQuantity, Key: "AAPL",
		IBOR: big.NewRat(100, 1), Custodian: big.NewRat(90, 1), Diff: big.NewRat(10, 1),
	}}, breakT0)
	if _, err := store.UpsertBreaks(context.Background(), custodySubject(), detected, breakT0); err != nil {
		t.Fatalf("UpsertBreaks: %v", err)
	}
	return store
}

func breakServer(t *testing.T, store custody.Store) *Server {
	t.Helper()
	return New(&Readiness{}, nil, ledger.NewMemoryStore(), "USD",
		WithTenant(testTenant), WithBreakStore(store))
}

func do(t *testing.T, s *Server, method, path, tenant, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if tenant != "" {
		r.Header.Set(auth.HeaderPrincipalTenant, tenant)
		r.Header.Set(auth.HeaderPrincipalSubject, "alice@kanz")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func decodeBreaks(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var out struct {
		Breaks []map[string]any `json:"breaks"`
		Count  int              `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body.String())
	}
	return out.Breaks
}

// THE QUEUE IS REACHABLE AND CARRIES THE AGE.
func TestBreakQueueIsServedWithAges(t *testing.T) {
	s := breakServer(t, seededBreakStore(t))
	w := do(t, s, "GET", "/v1/custody/breaks", testTenant, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	breaks := decodeBreaks(t, w)
	if len(breaks) != 1 {
		t.Fatalf("%d breaks, want 1", len(breaks))
	}
	b := breaks[0]
	if b["kind"] != "quantity" || b["key"] != "AAPL" {
		t.Fatalf("break = %+v", b)
	}
	// FIGURES ARE TEXT, NOT JSON NUMBERS. encoding/json renders a number as
	// float64, and the person deciding what to do about a break must see the
	// figure the book actually holds.
	if _, ok := b["difference"].(string); !ok {
		t.Fatalf("difference is %T, want a decimal string — a float would show a different number than the book holds", b["difference"])
	}
	if b["difference"] != "10" {
		t.Fatalf("difference = %v, want 10", b["difference"])
	}
	if _, ok := b["age_seconds"]; !ok {
		t.Fatal("no age_seconds — the number the queue is triaged by")
	}
}

// THE TENANT GATE COVERS THE NEW ROUTES. The queue names one fund's unresolved
// control failures; another tenant must not be able to discover them.
func TestBreakQueueRefusesAnotherTenant(t *testing.T) {
	s := breakServer(t, seededBreakStore(t))
	for _, path := range []string{
		"/v1/custody/breaks",
	} {
		if w := do(t, s, "GET", path, otherTenant, ""); w.Code == http.StatusOK {
			t.Fatalf("GET %s from another tenant returned 200", path)
		}
	}
	for _, path := range []string{
		"/v1/custody/breaks/" + breakID() + "/assign",
		"/v1/custody/breaks/" + breakID() + "/explain",
		"/v1/custody/breaks/" + breakID() + "/resolve",
	} {
		if w := do(t, s, "POST", path, otherTenant, `{"assignee":"mallory","explanation":"x"}`); w.Code == http.StatusOK {
			t.Fatalf("POST %s from another tenant returned 200", path)
		}
	}
}

// WITHOUT A STORE THE ROUTES ARE NOT MOUNTED AT ALL — 404, which is the truthful
// answer for a deployment with no custody reconciliation. A route over a nil
// store would answer 500 and read as "the queue is broken".
func TestBreakQueueIsAbsentWithoutAStore(t *testing.T) {
	s := New(&Readiness{}, nil, ledger.NewMemoryStore(), "USD", WithTenant(testTenant))
	if w := do(t, s, "GET", "/v1/custody/breaks", testTenant, ""); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unmounted queue", w.Code)
	}
}

func TestAssignAndExplainMoveTheLifecycle(t *testing.T) {
	store := seededBreakStore(t)
	s := breakServer(t, store)

	w := do(t, s, "POST", "/v1/custody/breaks/"+breakID()+"/assign", testTenant, `{"assignee":"alice"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("assign status = %d (body %s)", w.Code, w.Body.String())
	}
	w = do(t, s, "POST", "/v1/custody/breaks/"+breakID()+"/explain", testTenant, `{"explanation":"late settlement, clears T+2"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("explain status = %d (body %s)", w.Code, w.Body.String())
	}

	got, err := store.LoadBreak(context.Background(), breakID())
	if err != nil {
		t.Fatalf("LoadBreak: %v", err)
	}
	if got.Status != custody.BreakExplained {
		t.Fatalf("status = %s, want explained", got.Status)
	}
	if got.Assignee != "alice" || got.Explanation == "" {
		t.Fatalf("assignee=%q explanation=%q", got.Assignee, got.Explanation)
	}
}

// AN EXPLANATION IS MANDATORY. "Explained" with nothing recorded is the same
// silence the whole control abolishes, one level in.
func TestExplainWithoutAnExplanationIsRefused(t *testing.T) {
	s := breakServer(t, seededBreakStore(t))
	w := do(t, s, "POST", "/v1/custody/breaks/"+breakID()+"/explain", testTenant, `{"explanation":""}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestAssignWithoutAnAssigneeIsRefused(t *testing.T) {
	s := breakServer(t, seededBreakStore(t))
	w := do(t, s, "POST", "/v1/custody/breaks/"+breakID()+"/assign", testTenant, `{"assignee":"  "}`)
	if w.Code == http.StatusOK {
		t.Fatalf("status = %d, want a refusal for a blank assignee", w.Code)
	}
}

// THE CONTROL CANNOT BE SILENCED BY HAND. A break the latest run still detects
// must not be resolvable: that would leave the number wrong and the queue
// looking clean, which is the failure that makes a reconciliation process
// decorative.
func TestResolveIsRefusedWhileTheDifferenceIsStillDetected(t *testing.T) {
	store := seededBreakStore(t)
	// A second run re-detects it, so LastSeenAt moves past StatusChangedAt — the
	// store's statement that the difference is still there.
	detected := custody.FromRecon(custodySubject(), []recon.Break{{
		Kind: recon.BreakQuantity, Key: "AAPL",
		IBOR: big.NewRat(100, 1), Custodian: big.NewRat(90, 1), Diff: big.NewRat(10, 1),
	}}, breakT0.Add(24*time.Hour))
	if _, err := store.UpsertBreaks(context.Background(), custodySubject(), detected, breakT0.Add(24*time.Hour)); err != nil {
		t.Fatalf("UpsertBreaks: %v", err)
	}

	s := breakServer(t, store)
	w := do(t, s, "POST", "/v1/custody/breaks/"+breakID()+"/resolve", testTenant, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — the control was silenced by hand (body %s)", w.Code, w.Body.String())
	}
	got, err := store.LoadBreak(context.Background(), breakID())
	if err != nil {
		t.Fatalf("LoadBreak: %v", err)
	}
	if got.Status == custody.BreakResolved {
		t.Fatal("the break was resolved despite the difference still being detected")
	}
}

// A BREAK THE RUNS NO LONGER FIND MAY BE RESOLVED. This is the legitimate path,
// and it must actually work or an operator can never close anything by hand.
func TestResolveSucceedsOnceTheSidesAgree(t *testing.T) {
	store := seededBreakStore(t)
	s := breakServer(t, store)
	w := do(t, s, "POST", "/v1/custody/breaks/"+breakID()+"/resolve", testTenant, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	got, err := store.LoadBreak(context.Background(), breakID())
	if err != nil {
		t.Fatalf("LoadBreak: %v", err)
	}
	if got.Status != custody.BreakResolved {
		t.Fatalf("status = %s, want resolved", got.Status)
	}
}

// A BREAK NOTHING EVER DETECTED IS A 404, NOT A CREATION. An operator may record
// what they know about a break and may not invent one.
func TestATransitionOnAnUnknownBreakIs404(t *testing.T) {
	s := breakServer(t, seededBreakStore(t))
	w := do(t, s, "POST", "/v1/custody/breaks/PF1%7CCUST-A%7Cquantity%7CNEVER/assign", testTenant, `{"assignee":"alice"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
