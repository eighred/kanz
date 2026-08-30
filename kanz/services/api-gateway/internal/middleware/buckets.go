package middleware

import (
	"sync"
	"time"
)

// maxRateBuckets is the hard ceiling on how many token buckets one bucketSet
// holds. It is a BACKSTOP, not the working bound: gc's first pass sheds every
// bucket that has refilled to full, and a bucket is non-full only if its key
// spent a token within the last burst/rate seconds — so the live set is sized by
// principals active in the last few seconds, not by principals ever seen.
//
// 10,000 is chosen against that horizon rather than against traffic: the gateway
// would need ten thousand DISTINCT principals mid-burst simultaneously to reach
// it, which is orders of magnitude past any tenant roster this fronts. At roughly
// 60 bytes per entry the ceiling itself costs well under a megabyte, so it is
// cheap to set far above the working set and let it stay unreached.
const maxRateBuckets = 10_000

type bucket struct {
	tokens float64
	last   time.Time
	// perSec/burst are the budget that was applied to this bucket most recently.
	// They are recorded on the bucket, rather than passed to gc, because the
	// budget is PER KEY — TenantLimits.Overrides gives one tenant a different
	// rate from another — and gc walks keys whose tenant it does not know.
	perSec float64
	burst  float64
}

// take lazily refills the bucket by elapsed time (no background goroutine, so
// an idle gateway holds no timers), caps it at burst, and consumes one token —
// returning false when empty. Caller holds the owning mutex.
func (b *bucket) take(now time.Time, perSec, burst float64) bool {
	b.perSec, b.burst = perSec, burst
	b.tokens += now.Sub(b.last).Seconds() * perSec
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// refilled reports whether the bucket has recovered to its full burst by now.
//
// This is the eviction test, and it is EXACT rather than a heuristic TTL. A key
// absent from the map is created with tokens = burst-1 at the time of the
// request; a full bucket taken from yields tokens = burst-1 at the same instant.
// The two states are indistinguishable to every later call, so dropping a
// refilled bucket cannot hand anyone a token they had not already earned back.
func (b *bucket) refilled(now time.Time) bool {
	return b.tokens+now.Sub(b.last).Seconds()*b.perSec >= b.burst
}

// bucketSet is the gateway's one keyed token-bucket store, shared by the global
// RateLimit (API-01d) and the per-tenant Quota (MT-01e).
//
// # WHY IT IS ONE TYPE AND NOT TWO MAPS
//
// It was two maps, and both leaked the same way (#834). A copied helper is how a
// fix stops spreading, so the eviction discipline lives here once — the shape
// bus.DedupWindow.gc and bus.Producer.gcSequence already use, one service out.
//
// # WHAT THE LEAK COST
//
// The keys are principals (rateKey: tenant → subject → remote addr), so a
// gateway that stays up across staff turnover, service accounts, rotated
// subjects and per-integration identities accumulated one permanent entry per
// identity it had ever served. It arrives as an OOM kill, not an error, and the
// gateway is the sole entry point for POST /v1/orders — so the failure mode is
// the order path being unreachable, reported by nothing until the pod dies.
type bucketSet struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	max     int
	now     func() time.Time
}

func newBucketSet() *bucketSet {
	return &bucketSet{buckets: map[string]*bucket{}, max: maxRateBuckets, now: time.Now}
}

// allow consumes one token from key's bucket at the given budget, returning
// false when the bucket is empty. An unseen key starts full, so a first request
// is never throttled.
func (s *bucketSet) allow(key string, perSec, burst float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	b, live := s.buckets[key]
	if live {
		return b.take(now, perSec, burst)
	}
	// SWEPT ONLY WHEN A NEW KEY WOULD CROSS THE CEILING. A request from a
	// principal the map already holds — the overwhelming majority — touches
	// nothing but its own bucket, so the steady-state path is unchanged.
	if len(s.buckets) >= s.max {
		s.gc(now)
	}
	s.buckets[key] = &bucket{tokens: burst - 1, last: now, perSec: perSec, burst: burst}
	return true
}

// gc sheds refilled buckets, then the least-recently-used until the map is under
// its ceiling. Caller holds s.mu.
//
// The second pass is what makes the bound HARD rather than a hope, and it is the
// only part with a cost: evicting a bucket that is still draining hands its key
// a fresh burst, so above maxRateBuckets simultaneously-throttled principals the
// limiter degrades toward permissive for the least recently active of them.
// That is deliberate and it is the cheaper failure — the alternative to shedding
// is unbounded growth, and a gateway killed for memory refuses EVERY tenant's
// orders rather than briefly under-throttling one. The first pass is free, so
// the second only runs in that pathological case.
func (s *bucketSet) gc(now time.Time) {
	for k, b := range s.buckets {
		if b.refilled(now) {
			delete(s.buckets, k)
		}
	}
	for len(s.buckets) >= s.max {
		var oldestK string
		var oldestAt time.Time
		first := true
		for k, b := range s.buckets {
			if first || b.last.Before(oldestAt) {
				oldestK, oldestAt, first = k, b.last, false
			}
		}
		delete(s.buckets, oldestK)
	}
}
