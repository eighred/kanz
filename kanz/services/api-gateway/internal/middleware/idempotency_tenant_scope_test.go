package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// THE IDEMPOTENCY CACHE MUST NOT SPAN TENANTS.
//
// Idempotency keys on the raw client-supplied Idempotency-Key header. Two
// tenants that choose the same key within the TTL share a cache entry, and the
// middleware short-circuits BEFORE the handler — so the second tenant reads the
// first tenant's response body and its own request is never executed.
//
// On /v1/orders that body is {"order_id":"...","status":"submitted"}: another
// fund's order id, returned with 202, for an order this caller never placed.
// Both halves are defects and the second is the worse one — the client believes
// it holds exposure it does not hold.
//
// This needs no adversary. `1`, `retry-1` and a client's own sequence number
// collide between tenants by accident.
func TestIdempotencyDoesNotReplayAcrossTenants(t *testing.T) {
	var served int
	h := Idempotency(time.Minute, 100)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		p := PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"order_id":"order-for-` + p.Tenant + `","status":"submitted"}`))
	}))

	call := func(tenant string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader("{}"))
		r.Header.Set("Idempotency-Key", "retry-1") // the SAME key both tenants chose
		r = r.WithContext(WithPrincipal(r.Context(), &Principal{
			Subject: "user@" + tenant, Tenant: tenant,
		}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	first := call("acme")
	second := call("globex")

	if got := first.Body.String(); !strings.Contains(got, "order-for-acme") {
		t.Fatalf("first tenant got %q", got)
	}
	if second.Header().Get("Idempotent-Replayed") == "true" {
		t.Error("tenant globex was served a REPLAY of a request it never made")
	}
	if got := second.Body.String(); strings.Contains(got, "order-for-acme") {
		t.Errorf("CROSS-TENANT LEAK: globex received acme's response %q — another fund's order id, "+
			"and globex's own order was never published", got)
	}
	if served != 2 {
		t.Errorf("the handler ran %d time(s) for two different tenants; globex's order was never "+
			"submitted, yet it received a 202 naming an order id", served)
	}
}
