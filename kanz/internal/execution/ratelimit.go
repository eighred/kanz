// Shared exchange-connector infra (weight-metered rate limiting), compiled under
// either exchange tag so Binance and OKX reuse one implementation while the
// default OMS binary still carries no exchange-vendor code.

package execution

import (
	"sync"
	"time"
)

// WeightBucket is a Binance-weight token bucket. Binance meters REST calls by a
// per-endpoint "weight" against a per-minute budget; this refills the budget
// continuously and blocks (or reports starvation) when a call would exceed it,
// so we respect the exchange limit aggressively rather than get banned.
type WeightBucket struct {
	mu         sync.Mutex
	capacity   float64 // max weight per window
	tokens     float64
	refillRate float64 // tokens per second
	now        func() time.Time
	last       time.Time
}

// NewWeightBucket builds a bucket that refills `capacity` weight over `window`.
func NewWeightBucket(capacity int, window time.Duration, now func() time.Time) *WeightBucket {
	if now == nil {
		now = time.Now
	}
	if window <= 0 {
		window = time.Minute
	}
	return &WeightBucket{
		capacity:   float64(capacity),
		tokens:     float64(capacity),
		refillRate: float64(capacity) / window.Seconds(),
		now:        now,
		last:       now(),
	}
}

// allow tries to consume `weight` tokens without blocking. It returns true and
// consumes on success; false (consuming nothing) when the budget is exhausted —
// the caller backs off and alerts rather than firing the request.
func (b *WeightBucket) Allow(weight int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	if b.tokens < float64(weight) {
		return false
	}
	b.tokens -= float64(weight)
	return true
}

// retryAfter is how long until `weight` tokens are available (0 if now).
func (b *WeightBucket) RetryAfter(weight int) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	deficit := float64(weight) - b.tokens
	if deficit <= 0 {
		return 0
	}
	return time.Duration(deficit / b.refillRate * float64(time.Second))
}

func (b *WeightBucket) refill() {
	now := b.now()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed * b.refillRate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
}
