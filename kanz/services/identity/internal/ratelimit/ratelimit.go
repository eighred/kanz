// Package ratelimit bounds unauthenticated credential attempts (#364).
//
// WHY THIS EXISTS AT ALL. The identity service's login route is the only place
// in the estate that answers an anonymous caller with "that credential was
// right". The gateway's quota middleware runs AFTER authentication and so never
// sees this request. Without a limiter here, login is an offline
// password-guessing oracle that answers as fast as Argon2id allows — and
// Argon2id's cost is a speed bump, not a bound.
//
// THE KEYS ARE ATTACKER-CONTROLLED, WHICH MAKES MEMORY A CORRECTNESS PROBLEM. A
// caller chooses the subject they attempt, so a naive map grows once per
// distinct guess and the limiter becomes the cheapest way to exhaust the
// process. Entries are therefore swept, and the map is hard-capped; past the cap
// this REFUSES rather than admitting an unbounded key set. That is a deliberate
// trade: at the cap an attack is already in progress, and refusing logins loudly
// is a better failure than a limiter that quietly stops limiting.
package ratelimit

import (
	"log/slog"
	"sync"
	"time"
)

// Defaults sized for a human signing in, not for a script.
//
// Burst is what a person actually does — a typo, a second typo, the right one —
// and Refill is slow enough that sustained guessing gains almost nothing:
// 12/minute against a single account, against an Argon2id verification that
// costs 64 MiB. A legitimate operator never notices it; a script never gets past
// it.
const (
	DefaultBurst     = 5
	DefaultRefill    = 5 * time.Second // one token per 5s ⇒ 12/min sustained
	DefaultMaxKeys   = 100_000         // ~10 MB of buckets, a hard ceiling
	DefaultIdleAfter = 15 * time.Minute
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a token bucket per key, with a bounded key set.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	burst     float64
	refill    time.Duration
	maxKeys   int
	idleAfter time.Duration

	logger *slog.Logger
	now    func() time.Time

	// atCapacity latches so the refusal is reported once per episode rather than
	// once per request — an attack would otherwise produce the log line at the
	// same rate as the attack itself, burying everything else.
	atCapacity bool
}

// Options configures a Limiter. The zero value of each field takes the default.
type Options struct {
	Burst     int
	Refill    time.Duration
	MaxKeys   int
	IdleAfter time.Duration
	Logger    *slog.Logger
}

// New builds a limiter.
func New(o Options) *Limiter {
	l := &Limiter{
		buckets:   make(map[string]*bucket),
		burst:     float64(o.Burst),
		refill:    o.Refill,
		maxKeys:   o.MaxKeys,
		idleAfter: o.IdleAfter,
		logger:    o.Logger,
		now:       time.Now,
	}
	if l.burst <= 0 {
		l.burst = DefaultBurst
	}
	if l.refill <= 0 {
		l.refill = DefaultRefill
	}
	if l.maxKeys <= 0 {
		l.maxKeys = DefaultMaxKeys
	}
	if l.idleAfter <= 0 {
		l.idleAfter = DefaultIdleAfter
	}
	if l.logger == nil {
		l.logger = slog.Default()
	}
	return l
}

// Allow reports whether an attempt keyed by k may proceed, consuming a token.
func (l *Limiter) Allow(k string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	b, ok := l.buckets[k]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			l.sweepLocked(now)
		}
		if len(l.buckets) >= l.maxKeys {
			// FAIL CLOSED, LOUDLY. Admitting the key would make the key set
			// unbounded, which is the failure this cap exists to prevent; and
			// admitting it WITHOUT a bucket would mean no limit on the very
			// traffic that filled the table.
			if !l.atCapacity {
				l.atCapacity = true
				l.logger.Error("credential rate limiter at capacity — refusing new keys. "+
					"Distinct-key pressure this high is a guessing attack, not usage; "+
					"logins for accounts without an existing bucket are refused until it drains.",
					"max_keys", l.maxKeys)
			}
			return false
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[k] = b
	}

	// Refill for elapsed time, then spend.
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() / l.refill.Seconds()
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Sweep drops buckets that carry no information — full and idle. A full bucket
// is indistinguishable from one that has never been used, so forgetting it
// cannot weaken the limit.
func (l *Limiter) Sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(l.now())
}

func (l *Limiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		full := b.tokens+now.Sub(b.last).Seconds()/l.refill.Seconds() >= l.burst
		if full && now.Sub(b.last) >= l.idleAfter {
			delete(l.buckets, k)
		}
	}
	if l.atCapacity && len(l.buckets) < l.maxKeys {
		l.atCapacity = false
		l.logger.Info("credential rate limiter back below capacity", "keys", len(l.buckets))
	}
}

// Len reports the number of tracked keys. For tests and metrics.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
