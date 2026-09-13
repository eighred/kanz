package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func durableBreakServer(t *testing.T) (*custody.Postgres, *Server) {
	t.Helper()
	pool := newServerPool(t)
	store := custody.NewPostgres(pool)
	detected, err := seededBreakStore(t).OutstandingBreaks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.UpsertBreaks(context.Background(), custodySubject(), detected, breakT0); err != nil {
		t.Fatal(err)
	}
	s := New(&Readiness{}, nil, ledger.NewMemoryStore(), "USD", WithTenant("__system__"), WithBreakStore(store))
	return store, s
}

func TestCustodyActionRefusesVolatileEvidence(t *testing.T) {
	store := seededBreakStore(t)
	s := breakServer(t, store)
	w := do(t, s, "POST", "/v1/custody/breaks/"+breakID()+"/actions", testTenant, `{"request_id":"claim-1","expected_revision":"1","action":"claim"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("volatile action: %d", w.Code)
	}
	b, err := store.LoadBreak(context.Background(), breakID())
	if err != nil || b.Status != custody.BreakOpen {
		t.Fatalf("volatile action mutated state: %+v %v", b, err)
	}
}

func TestCustodyActionRefusesForgedAttributionAndUnversionedBodies(t *testing.T) {
	s := breakServer(t, seededBreakStore(t))
	for _, body := range []string{
		`{"request_id":"claim-1","expected_revision":"1","action":"claim","assignee":"someone-else"}`,
		`{"request_id":"claim-1","expected_revision":"1","action":"claim","actor":"someone-else"}`,
		`{"request_id":"claim-1","action":"claim"}`,
		`{"expected_revision":"1","action":"claim"}`,
		`{"request_id":"claim-1","expected_revision":"1","action":"resolve"}`,
		`{"request_id":"claim-1","expected_revision":"1","action":"explain","explanation":" "}`,
		`{"request_id":"claim-1","expected_revision":"1","action":"claim"} {}`,
		`{"request_id":"claim-1","expected_revision":1,"action":"claim"}`,
	} {
		w := do(t, s, "POST", "/v1/custody/breaks/"+breakID()+"/actions", testTenant, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("invalid action status: %d", w.Code)
		}
	}
}

func TestCustodyActionRequiresAuthenticatedSubject(t *testing.T) {
	s := breakServer(t, seededBreakStore(t))
	r := httptest.NewRequest("POST", "/v1/custody/breaks/"+breakID()+"/actions", strings.NewReader(`{"request_id":"claim-1","expected_revision":"1","action":"claim"}`))
	r.Header.Set(auth.HeaderPrincipalTenant, testTenant)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing actor: %d", w.Code)
	}
}

func TestCustodyActionHTTPBindsActorAndReplaysEvidence(t *testing.T) {
	store, s := durableBreakServer(t)
	path := "/v1/custody/breaks/" + breakID() + "/actions"
	body := `{"request_id":"claim-http","expected_revision":"1","action":"claim"}`
	first := do(t, s, "POST", path, "__system__", body)
	if first.Code != 200 {
		t.Fatalf("claim: %d %s", first.Code, first.Body.String())
	}
	retry := do(t, s, "POST", path, "__system__", body)
	if retry.Code != 200 || retry.Body.String() != first.Body.String() {
		t.Fatal("retry did not return the original evidence")
	}
	b, err := store.LoadBreak(context.Background(), breakID())
	if err != nil || b.Assignee != "alice@kanz" || b.Revision != 2 {
		t.Fatalf("actor/revision: %+v %v", b, err)
	}
	stale := do(t, s, "POST", path, "__system__", `{"request_id":"new-claim","expected_revision":"1","action":"claim"}`)
	if stale.Code != 409 {
		t.Fatalf("stale screen: %d", stale.Code)
	}
	foreign := do(t, s, "POST", path, otherTenant, body)
	if foreign.Code != 404 {
		t.Fatalf("cross tenant action: %d", foreign.Code)
	}
}
