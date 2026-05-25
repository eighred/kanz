package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestVersionNegotiation(t *testing.T) {
	h := Version()(okHandler())
	cases := []struct {
		header string
		want   int
	}{
		{"", http.StatusOK},   // default ⇒ v1
		{"v1", http.StatusOK}, // explicit supported
		{"v2", http.StatusNotAcceptable},
		{"garbage", http.StatusNotAcceptable},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		if tc.header != "" {
			req.Header.Set("X-API-Version", tc.header)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Errorf("X-API-Version=%q ⇒ %d, want %d", tc.header, rr.Code, tc.want)
		}
		if rr.Code == http.StatusOK && rr.Header().Get("X-API-Version") != SupportedVersion {
			t.Errorf("response missing X-API-Version echo")
		}
	}
}

func TestRateLimit429(t *testing.T) {
	// 1 token, no refill within the test window ⇒ first request passes, second 429.
	h := RateLimit(1, 1)(okHandler())
	req := func() int {
		r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr.Code
	}
	if c := req(); c != http.StatusOK {
		t.Fatalf("first request = %d, want 200", c)
	}
	if c := req(); c != http.StatusTooManyRequests {
		t.Errorf("second request = %d, want 429", c)
	}
}

func TestRateLimitPerKeyIsolation(t *testing.T) {
	h := RateLimit(1, 1)(okHandler())
	call := func(addr string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		r.RemoteAddr = addr
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr.Code
	}
	call("10.0.0.1:1") // exhaust key A
	if c := call("10.0.0.2:1"); c != http.StatusOK {
		t.Errorf("key B starved by key A: got %d", c)
	}
}

func TestRateLimitDisabled(t *testing.T) {
	h := RateLimit(0, 0)(okHandler())
	for i := 0; i < 100; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("disabled limiter blocked request %d", i)
		}
	}
}

func TestIdempotencyReplay(t *testing.T) {
	var calls int
	sink := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"n":1}`))
	})
	h := Idempotency(time.Minute, 100)(sink)

	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/portfolios/p/scenario", strings.NewReader("{}"))
		r.Header.Set("Idempotency-Key", "key-1")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	first := do()
	second := do()

	if calls != 1 {
		t.Errorf("handler invoked %d times, want 1 (second should replay)", calls)
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("replayed response missing Idempotent-Replayed header")
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("replay body mismatch: %q vs %q", first.Body.String(), second.Body.String())
	}
}

func TestIdempotencySkipsGET(t *testing.T) {
	var calls int
	sink := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(200) })
	h := Idempotency(time.Minute, 100)(sink)
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		r.Header.Set("Idempotency-Key", "k")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if calls != 2 {
		t.Errorf("GET should not be deduped: calls = %d, want 2", calls)
	}
}
