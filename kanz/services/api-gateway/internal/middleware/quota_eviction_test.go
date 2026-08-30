package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// THE ISSUE'S OWN "VERIFIED WHEN" (#834): drive N distinct principals through
// the WIRED chain link and neither map may retain one entry per principal.
//
// Before the fix this reported 30000/30000 — the gateway kept a token bucket and
// a zero-valued in-flight counter for every identity it had ever served, for the
// life of the pod, and the gateway is the sole entry point for POST /v1/orders.
func TestQuotaMapsAreBoundedAcrossPrincipalChurn(t *testing.T) {
	q := newQuota(TenantLimits{Default: Limits{RatePerSec: 10, Burst: 10, MaxInFlight: 4}}, nil)
	now := time.Unix(0, 0)
	q.rates.now = func() time.Time { return now }
	h := q.handler()(okHandler())

	const principals = 3 * maxRateBuckets
	for i := 0; i < principals; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, reqWithTenant(fmt.Sprintf("tenant-%06d", i)))
		if rr.Code != http.StatusOK {
			t.Fatalf("principal %d refused with %d — a first request must never be throttled", i, rr.Code)
		}
		now = now.Add(time.Second) // each principal calls once, a second apart
	}

	t.Logf("after %d distinct principals: buckets=%d inflight=%d", principals, len(q.rates.buckets), len(q.inflight))
	if got := len(q.rates.buckets); got > maxRateBuckets {
		t.Errorf("rate buckets = %d after %d principals, want <= %d: the map grows with every "+
			"identity ever seen rather than with the identities currently active", got, principals, maxRateBuckets)
	}
	if got := len(q.inflight); got != 0 {
		t.Errorf("inflight = %d with nothing in flight, want 0: a completed request must leave "+
			"no key behind", got)
	}
}

// The in-flight map is bounded BY CONSTRUCTION rather than by a ceiling: a key
// exists only while it holds a slot. This asserts both halves — present at one,
// gone at zero — because a decrement that stops at zero looks identical to a
// delete from every response the gateway writes.
func TestQuotaInflightDropsTheKeyAtZero(t *testing.T) {
	q := newQuota(TenantLimits{Default: Limits{MaxInFlight: 2}}, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	h := q.handler()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), reqWithTenant("acme"))
		close(done)
	}()
	<-entered

	q.mu.Lock()
	held := q.inflight["t:acme"]
	q.mu.Unlock()
	if held != 1 {
		t.Fatalf("inflight[t:acme] = %d while the handler is running, want 1", held)
	}

	close(release)
	<-done

	q.mu.Lock()
	_, present := q.inflight["t:acme"]
	size := len(q.inflight)
	q.mu.Unlock()
	if present {
		t.Errorf("inflight still holds t:acme after the request completed — a zero-valued entry " +
			"per principal is exactly the retention #834 is about")
	}
	if size != 0 {
		t.Errorf("len(inflight) = %d after the only request completed, want 0", size)
	}
}

// EVICTION IS EXACT, NOT APPROXIMATE. gc's free pass drops buckets that have
// refilled to full, and this pins WHY that costs nothing: a set that kept the
// full bucket and a set that never saw the key must answer every subsequent
// request identically. If they ever diverge, eviction has handed someone a token
// they had not earned back, and the rate limit is weaker than it reads.
func TestEvictingARefilledBucketChangesNoVerdict(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	const perSec, burst = 1.0, 3.0

	kept := newBucketSet()
	kept.now = clock
	for i := 0; i < int(burst); i++ {
		kept.allow("t:acme", perSec, burst) // spend the whole burst
	}
	now = now.Add(10 * time.Second) // ... and let it recover
	if b := kept.buckets["t:acme"]; b == nil || !b.refilled(now) {
		t.Fatalf("premise broken: the bucket is not refilled after 10s at %v/s burst %v", perSec, burst)
	}

	evicted := newBucketSet() // what gc leaves behind: the key simply absent
	evicted.now = clock

	for step := 0; step < 5; step++ {
		a := kept.allow("t:acme", perSec, burst)
		b := evicted.allow("t:acme", perSec, burst)
		if a != b {
			t.Fatalf("step %d: retained bucket allowed=%v, evicted bucket allowed=%v — evicting a "+
				"refilled bucket changed the limiter's answer", step, a, b)
		}
	}
}

// THE PATHOLOGICAL CASE the ceiling exists for: every bucket is still draining,
// so gc's free pass sheds nothing and the LRU pass is the only thing standing
// between the gateway and unbounded growth. A rate this slow means no bucket
// refills for the whole run.
func TestBucketSetShedsWhenNothingHasRefilled(t *testing.T) {
	now := time.Unix(0, 0)
	s := &bucketSet{buckets: map[string]*bucket{}, max: 64, now: func() time.Time { return now }}
	const perSec, burst = 0.001, 1.0 // one token per ~17 minutes

	const keys = 500
	for i := 0; i < keys; i++ {
		s.allow(fmt.Sprintf("s:svc-%04d", i), perSec, burst)
		now = now.Add(time.Millisecond)
	}
	for k, b := range s.buckets {
		if b.refilled(now) {
			t.Fatalf("premise broken: %s refilled, so the free pass did the work and the LRU pass "+
				"is untested here", k)
		}
	}
	if got := len(s.buckets); got > s.max {
		t.Errorf("buckets = %d after %d never-refilling keys, want <= %d", got, keys, s.max)
	}
}

// gc's free pass must DISCRIMINATE, and this is the test that says so. Bounding
// the map is not enough on its own: a gc that ignored refilled and always fell
// through to LRU would keep the same ceiling while evicting buckets that were
// still draining — the limiter would go quietly permissive for whoever had been
// throttled longest, and every size assertion would still be green.
func TestGCShedsTheRefilledBucketAndKeepsTheDrainingOnes(t *testing.T) {
	now := time.Unix(0, 0)
	s := &bucketSet{buckets: map[string]*bucket{}, max: 4, now: func() time.Time { return now }}

	for i := 0; i < 3; i++ {
		s.allow(fmt.Sprintf("t:draining-%d", i), 0.001, 1) // ~17 minutes to earn one token
		now = now.Add(time.Millisecond)
	}
	s.allow("t:refilled", 1000, 1) // one token per millisecond
	now = now.Add(time.Second)     // long enough for t:refilled alone to recover

	s.allow("t:newcomer", 0.001, 1) // len == max, so this insert runs gc

	if _, still := s.buckets["t:refilled"]; still {
		t.Error("gc kept a bucket that had refilled to full — the free pass is not running, so " +
			"every sweep pays with a draining bucket instead")
	}
	for i := 0; i < 3; i++ {
		k := fmt.Sprintf("t:draining-%d", i)
		if _, kept := s.buckets[k]; !kept {
			t.Errorf("gc evicted %s, which was still draining, while a refilled bucket was "+
				"available — that hands its key a fresh burst it had not earned", k)
		}
	}
}

// The limiter still limits after the rework: eviction bounds memory, it does not
// hand a repeat caller a fresh budget.
func TestBucketSetStillThrottlesAKeyItKeeps(t *testing.T) {
	now := time.Unix(0, 0)
	s := newBucketSet()
	s.now = func() time.Time { return now }
	if !s.allow("t:acme", 1, 1) {
		t.Fatal("first request refused")
	}
	if s.allow("t:acme", 1, 1) {
		t.Error("second request at the same instant allowed — the bucket was empty")
	}
	now = now.Add(time.Second)
	if !s.allow("t:acme", 1, 1) {
		t.Error("request refused a second later — the bucket should have refilled one token")
	}
}
