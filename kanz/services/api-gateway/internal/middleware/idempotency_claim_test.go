package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// helpers shared by the claim tests.

func idemRequest(tenant, subject, method, path, key string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Header.Set("Idempotency-Key", key)
	return r.WithContext(WithPrincipal(r.Context(), &Principal{Subject: subject, Tenant: tenant}))
}

// TWO CONCURRENT RETRIES MUST NOT BOTH EXECUTE.
//
// The store this replaced wrote its entry AFTER the handler returned, so both
// arrivals missed the cache and both ran. On the order path that is two live
// orders at the venue from one client intent — and concurrent retry is exactly
// the shape of a client timeout storm, so this is the common case, not the
// exotic one.
func TestIdempotencyRefusesAConcurrentDuplicate(t *testing.T) {
	var mu sync.Mutex
	served := 0
	release := make(chan struct{})
	h := IdempotencyWith(nil, time.Minute, 100)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		served++
		first := served == 1
		mu.Unlock()
		// ONLY the first request parks. If the reservation is ever removed, the
		// second request runs the handler and returns promptly, so this test
		// FAILS on the assertions below rather than deadlocking — a hang reports
		// as a timeout, which is a much worse signal than a named failure.
		if first {
			<-release
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"order_id":"o-1"}`))
	}))

	first := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, idemRequest("acme", "u1", http.MethodPost, "/v1/orders", "retry-1"))
		first <- w.Code
	}()

	// Wait until the first request is definitely inside the handler.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		in := served
		mu.Unlock()
		if in == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the first request never reached the handler")
		default:
		}
	}

	second := httptest.NewRecorder()
	h.ServeHTTP(second, idemRequest("acme", "u1", http.MethodPost, "/v1/orders", "retry-1"))

	close(release)
	<-first

	mu.Lock()
	defer mu.Unlock()
	if served != 1 {
		t.Errorf("the handler ran %d times for one Idempotency-Key — a second order was placed "+
			"from one client intent", served)
	}
	if second.Code != http.StatusConflict {
		t.Errorf("the concurrent duplicate got %d, want 409; it must be REFUSED and told so, "+
			"not silently executed", second.Code)
	}
}

// A KEY IS A PROMISE ABOUT ONE OPERATION. The same key on a different route
// must not replay the first route's body — replaying a submit's response for a
// cancel is the cross-tenant defect wearing a different hat.
func TestIdempotencyScopesToTheRoute(t *testing.T) {
	h := IdempotencyWith(nil, time.Minute, 100)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"route":"` + r.URL.Path + `"}`))
	}))

	submit := httptest.NewRecorder()
	h.ServeHTTP(submit, idemRequest("acme", "u1", http.MethodPost, "/v1/orders", "k"))

	cancel := httptest.NewRecorder()
	h.ServeHTTP(cancel, idemRequest("acme", "u1", http.MethodPost, "/v1/orders/o-1/cancel", "k"))

	if strings.Contains(cancel.Body.String(), `"/v1/orders"`) {
		t.Errorf("the cancel replayed the submit's response: %q", cancel.Body.String())
	}
	if cancel.Code == http.StatusConflict {
		t.Error("the cancel was refused as a duplicate; a different route is a different operation")
	}
}

// TWO SUBJECTS IN ONE TENANT DO NOT SHARE A KEY. An Idempotency-Key is chosen by
// a client, and two clients of the same fund have no way to coordinate.
func TestIdempotencyScopesToTheSubject(t *testing.T) {
	var served int
	h := IdempotencyWith(nil, time.Minute, 100)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusAccepted)
	}))

	for _, subject := range []string{"alice", "bob"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, idemRequest("acme", subject, http.MethodPost, "/v1/orders", "k"))
		if w.Code == http.StatusConflict {
			t.Fatalf("%s was refused a key %s had used", subject, "the other")
		}
	}
	if served != 2 {
		t.Errorf("the handler ran %d time(s); alice and bob each placed an order", served)
	}
}

// A 5xx RELEASES THE KEY. A request that failed INSIDE the platform must be free
// to retry — holding the key would turn one transient fault into a permanently
// unusable key, and the client cannot mint a new one without changing what it
// believes it asked for.
func TestIdempotencyReleasesTheKeyAfterAServerError(t *testing.T) {
	var served int
	h := IdempotencyWith(nil, time.Minute, 100)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		if served == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"order_id":"o-1"}`))
	}))

	first := httptest.NewRecorder()
	h.ServeHTTP(first, idemRequest("acme", "u1", http.MethodPost, "/v1/orders", "k"))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first = %d, want 502", first.Code)
	}

	second := httptest.NewRecorder()
	h.ServeHTTP(second, idemRequest("acme", "u1", http.MethodPost, "/v1/orders", "k"))
	if second.Code == http.StatusConflict {
		t.Fatal("the retry was refused: a 5xx held the key, so the client can never complete " +
			"this operation under the key it already committed to")
	}
	if second.Code != http.StatusAccepted || served != 2 {
		t.Errorf("second = %d after %d handler runs, want 202 after 2", second.Code, served)
	}
}

// A 4xx HOLDS THE KEY. A refusal is a definite outcome — the platform decided —
// so a retry must read that decision rather than re-running it.
func TestIdempotencyHoldsTheKeyAfterAClientError(t *testing.T) {
	var served int
	h := IdempotencyWith(nil, time.Minute, 100)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad instrument"}`))
	}))

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, idemRequest("acme", "u1", http.MethodPost, "/v1/orders", "k"))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d = %d, want 400 (the replayed refusal)", i, w.Code)
		}
	}
	if served != 1 {
		t.Errorf("the handler ran %d times; the second read a replay, not a re-evaluation", served)
	}
}

// THE REPLAY CACHE EVICTS THE ENTRY EXPIRING SOONEST, NOT AN ARBITRARY LIVE ONE.
//
// The map this replaces did `for k := range entries { delete(k); break }` — Go
// randomizes map iteration, so the victim was chosen by nothing. Correctness no
// longer rests here (bus.Deduper carries at-most-once), but an eviction that
// discards the freshest entry still costs a client the replay it was promised.
func TestReplayCacheEvictsTheSoonestToExpire(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base
	c := newReplayCache(time.Minute, 2)
	c.now = func() time.Time { return now }

	c.put("oldest", cachedResponse{status: 200, body: []byte("a")})
	now = base.Add(10 * time.Second)
	c.put("middle", cachedResponse{status: 200, body: []byte("b")})
	now = base.Add(20 * time.Second)
	c.put("newest", cachedResponse{status: 200, body: []byte("c")}) // forces an eviction

	if _, ok := c.get("oldest"); ok {
		t.Error("the entry expiring soonest survived the eviction")
	}
	for _, k := range []string{"middle", "newest"} {
		if _, ok := c.get(k); !ok {
			t.Errorf("%q was evicted; it expires later than \"oldest\"", k)
		}
	}
}
