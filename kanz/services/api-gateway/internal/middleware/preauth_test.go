package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/eighred/kanz/internal/clientip"
)

// codeHandler answers with a fixed status, standing in for whatever the auth
// chain concluded about the request.
func codeHandler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
}

// callFrom drives one request through h from a source address and returns the
// status. The port varies deliberately on some callers below: a real client
// opens a new TCP connection per request, so a limiter that keyed on
// RemoteAddr verbatim (IP:PORT) would hand every one of them a fresh bucket.
func callFrom(h http.Handler, remoteAddr string) int {
	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.RemoteAddr = remoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr.Code
}

// TestPreAuthRefusesASourceThatKeepsFailingToAuthenticate is the property #835
// is about: after the source has spent its budget on 401s, the next request is
// refused WITHOUT the handler behind it running — which in the real chain is the
// HMAC verification and the token validation not running.
func TestPreAuthRefusesASourceThatKeepsFailingToAuthenticate(t *testing.T) {
	var reached int
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusUnauthorized)
	})
	h := PreAuth(1, 2, nil, nil)(inner)

	// burst 2 ⇒ two failures are absorbed, and both must reach the handler so the
	// caller learns WHY (401, not a bare 429 from the first request).
	for i := 1; i <= 2; i++ {
		if c := callFrom(h, "10.0.0.1:1000"); c != http.StatusUnauthorized {
			t.Fatalf("failure %d ⇒ %d, want 401", i, c)
		}
	}
	if c := callFrom(h, "10.0.0.1:1001"); c != http.StatusTooManyRequests {
		t.Fatalf("third attempt ⇒ %d, want 429 — the source is not bounded", c)
	}
	if reached != 2 {
		t.Errorf("the handler ran %d times, want 2 — a refused request still paid the cost the "+
			"limiter exists to avoid", reached)
	}
}

// TestPreAuthKeysOnTheAddressNotTheConnection pins the trap the issue named. Each
// call above used a different source PORT; if the key were RemoteAddr verbatim
// the three calls would have been three keys and nothing would have been
// limited. This asserts it from the other side: a DIFFERENT address is not
// starved by the first one's failures.
func TestPreAuthKeysOnTheAddressNotTheConnection(t *testing.T) {
	h := PreAuth(1, 1, nil, nil)(codeHandler(http.StatusUnauthorized))

	callFrom(h, "10.0.0.1:1") // exhaust source A
	if c := callFrom(h, "10.0.0.1:2"); c != http.StatusTooManyRequests {
		t.Errorf("same address on a new connection ⇒ %d, want 429 — the key includes the port, so "+
			"every TCP connection gets a fresh burst", c)
	}
	if c := callFrom(h, "10.0.0.2:1"); c != http.StatusUnauthorized {
		t.Errorf("a different address ⇒ %d, want 401 — one source starved another", c)
	}
}

// TestPreAuthSpendsNothingOnASuccessfulRequest is what makes an aggregated key
// safe. Behind an edge the resolver does not trust, every caller shares one
// bucket; if authenticated traffic debited it, legitimate load would throttle the
// estate. It must not, at any volume.
func TestPreAuthSpendsNothingOnASuccessfulRequest(t *testing.T) {
	h := PreAuth(1, 1, nil, nil)(codeHandler(http.StatusOK))
	for i := 0; i < 500; i++ {
		if c := callFrom(h, "10.0.0.1:1"); c != http.StatusOK {
			t.Fatalf("authenticated request %d ⇒ %d, want 200 — success is debiting the bucket", i, c)
		}
	}
}

// TestPreAuthChargesNothingForAnOutageOrAnAuthorisationRefusal covers the two
// codes that deliberately do NOT debit, and each for its own reason.
//
//   - 503 is Auth saying the JWKS cache or the revocation feed is unusable. That
//     is the platform's fault, and debiting it would turn an identity-provider
//     outage into a lockout that outlives the outage.
//   - 403 means authentication SUCCEEDED and produced a principal. That caller is
//     named and already bounded by the per-tenant Quota one hop later; charging a
//     shared source axis for it would let one misconfigured client behind an edge
//     spend a budget every other tenant depends on.
func TestPreAuthChargesNothingForAnOutageOrAnAuthorisationRefusal(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusForbidden} {
		h := PreAuth(1, 1, nil, nil)(codeHandler(code))
		for i := 0; i < 20; i++ {
			if c := callFrom(h, "10.0.0.1:1"); c != code {
				t.Fatalf("%d: request %d ⇒ %d, want %d — this code is debiting the source bucket",
					code, i, c, code)
			}
		}
	}
}

// TestPreAuthDisabledPassesEverything: a non-positive rate is off, matching
// Quota's convention. The composition root logs a WARN in that posture rather
// than letting "unconfigured" and "enforcing" look the same.
func TestPreAuthDisabledPassesEverything(t *testing.T) {
	h := PreAuth(0, 0, nil, nil)(codeHandler(http.StatusUnauthorized))
	for i := 0; i < 100; i++ {
		if c := callFrom(h, "10.0.0.1:1"); c != http.StatusUnauthorized {
			t.Fatalf("disabled limiter refused request %d with %d", i, c)
		}
	}
}

// TestPreAuthHonoursAForwardedHeaderOnlyFromATrustedPeer is the difference
// between a limiter and no limiter. A caller who can name their own key gets a
// fresh bucket per request, and the metrics show a wide spread of well-behaved
// clients while the bound does not exist.
func TestPreAuthHonoursAForwardedHeaderOnlyFromATrustedPeer(t *testing.T) {
	trusted, err := clientip.NewResolver("X-Forwarded-For", []string{"10.0.0.1"})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	// UNTRUSTED PEER: the header is ignored, so varying it cannot escape the
	// bucket. 10.0.0.9 is not in the trusted set.
	untrustedH := PreAuth(1, 1, trusted, nil)(codeHandler(http.StatusUnauthorized))
	spoof := func(h http.Handler, peer, claimed string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Forwarded-For", claimed)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr.Code
	}
	spoof(untrustedH, "10.0.0.9:1", "203.0.113.1")
	if c := spoof(untrustedH, "10.0.0.9:1", "203.0.113.2"); c != http.StatusTooManyRequests {
		t.Errorf("a new self-declared address from an untrusted peer ⇒ %d, want 429 — the caller "+
			"is choosing their own bucket and the limiter has stopped existing", c)
	}

	// TRUSTED PEER: the header IS the caller, so two real clients behind the edge
	// do not share one bucket.
	trustedH := PreAuth(1, 1, trusted, nil)(codeHandler(http.StatusUnauthorized))
	spoof(trustedH, "10.0.0.1:1", "203.0.113.1")
	if c := spoof(trustedH, "10.0.0.1:1", "203.0.113.2"); c != http.StatusUnauthorized {
		t.Errorf("a second client behind a TRUSTED edge ⇒ %d, want 401 — the forwarded address is "+
			"being ignored and every caller behind the edge shares one bucket", c)
	}
}

// TestPreAuthReportsARefusalItCannotNameATenantFor: the refusal happens before
// authentication, so there is no tenant. It reports under the same
// kanz_gateway_rate_limited_total series the per-tenant quota uses, labelled
// anonymous — an honest label rather than a second parallel metric.
func TestPreAuthReportsARefusalItCannotNameATenantFor(t *testing.T) {
	obs := &recordingQuotaObserver{}
	h := PreAuth(1, 1, nil, obs)(codeHandler(http.StatusUnauthorized))

	callFrom(h, "10.0.0.1:1")
	if len(obs.rateLimited) != 0 {
		t.Fatalf("the 401 itself was reported as rate-limited: %v", obs.rateLimited)
	}
	callFrom(h, "10.0.0.1:1")
	if got := obs.rateLimited; len(got) != 1 || got[0] != anonymousTenant {
		t.Errorf("rateLimited = %v, want exactly one %q", got, anonymousTenant)
	}
}

type recordingQuotaObserver struct {
	mu          sync.Mutex
	rateLimited []string
}

func (o *recordingQuotaObserver) RateLimited(tenant string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rateLimited = append(o.rateLimited, tenant)
}
func (o *recordingQuotaObserver) AdmissionRejected(string) {}
func (o *recordingQuotaObserver) InFlight(string, float64) {}

// TestPreAuthUnderConcurrentFailuresRefusesAtLeastOne drives the limiter from
// many goroutines at once.
//
// # WHAT THIS PROVES, STATED NARROWLY BECAUSE IT WAS MEASURED
//
// It proves ONE thing: a single source cannot escape the bound by arriving
// concurrently. Sixty-four simultaneous failures against a burst of four must
// not all be admitted, and every request must be answered by one of the two
// codes this middleware can produce.
//
// It does NOT prove the check is non-consuming. That was checked by mutation
// rather than assumed: making hasToken consume (i.e. spend-then-refund) leaves
// this case PASSING and is caught by
// TestPreAuthSpendsNothingOnASuccessfulRequest and by
// TestPreAuthChargesNothingForAnOutageOrAnAuthorisationRefusal instead. A
// concurrency case that claimed the non-consuming property would be a guard
// asserting something weaker than its name.
//
// It does not prove race-freedom either. -race needs cgo and does not run on the
// Windows box this was written on, so that claim is CI's to make; the logical
// property below runs everywhere.
func TestPreAuthUnderConcurrentFailuresRefusesAtLeastOne(t *testing.T) {
	const (
		burst   = 4
		callers = 64
	)
	var mu sync.Mutex
	codes := map[int]int{}
	// perSec is 0.001: one token per ~17 minutes, so nothing refills mid-test and
	// the arithmetic below is exact rather than timing-dependent.
	h := PreAuth(0.001, burst, nil, nil)(codeHandler(http.StatusUnauthorized))

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c := callFrom(h, "10.0.0.1:1")
			mu.Lock()
			codes[c]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if codes[http.StatusUnauthorized]+codes[http.StatusTooManyRequests] != callers {
		t.Fatalf("codes = %v, want every one of %d requests answered 401 or 429", codes, callers)
	}
	// The check is on the way IN and does not consume, so more than `burst`
	// requests can pass the gate before any of them has debited — that race is
	// benign and expected. What must NOT happen is the source escaping the bound
	// entirely.
	if codes[http.StatusTooManyRequests] == 0 {
		t.Errorf("codes = %v: %d concurrent failures from one source and not one was refused — "+
			"the bucket is being drained and refunded per in-flight request rather than per "+
			"failure", codes, callers)
	}
	if codes[http.StatusUnauthorized] > callers-1 {
		t.Errorf("codes = %v: essentially nothing was bounded", codes)
	}
}
