package bus

import (
	"context"
	"time"
)

// RedisDedup is a distributed Deduper (DEBT-02a): a seen-key set held in
// Redis/Dragonfly so dedup spans every consumer replica, not just one instance
// like DedupWindow. Wire it via WithDeduper; it slots behind the same Deduper
// API the Consumer already uses.
//
// # Decoupled from any concrete client
//
// The bus library does NOT import a Redis client — it depends on the minimal
// RedisClient interface below, the same decoupling as NATSConfig.TLSConfig
// taking a ready *tls.Config rather than importing go-spiffe. The deployment
// wires go-redis / a Dragonfly client / a cluster client by implementing two
// methods (see the package docs / coordination.md for the ~10-line adapter), so
// no consumer pays for a Redis dependency it doesn't use.
//
// # Atomic claim, cross-pod
//
// Claim is a SINGLE round-trip SET NX EX. That is what makes it safe across
// replicas: two pods racing the same key both issue the same command, and Redis
// serializes them, so exactly one gets the key. The previous Seen()+Record() pair
// could not do this — two round-trips are two chances to interleave, and both
// replicas passed Seen before either Recorded.
//
// A failed dispatch Releases the key rather than committing it, so the retry
// semantics the old split protected are preserved: a handler failure does not
// poison the window. And a worker that dies mid-dispatch does not strand the key
// forever — the claim carries a LEASE (WithRedisDedupLease, default 5s), so the
// event is reprocessed once it expires rather than being silently lost.
//
// # Fail-open
//
// A Redis error fails OPEN: Claim returns true (proceed), Commit and Release are
// no-ops, all reported via the error hook. A Redis outage degrades to "no dedup"
// rather than halting consumption — dedup suppresses duplicate work; it is not the
// thing standing between the platform and an unprocessed event.
type RedisDedup struct {
	client RedisClient
	ttl    time.Duration
	lease  time.Duration
	prefix string
	ctx    context.Context
	onErr  func(op string, err error)
}

// RedisClient is the minimal Redis surface RedisDedup needs. Implement it over
// go-redis, Dragonfly, or a cluster client at the composition root. Methods take
// a context so the adapter can bound each call; RedisDedup passes its base ctx.
type RedisClient interface {
	// ClaimNX atomically sets key IFF it is absent, with lease as its TTL, and
	// reports whether this caller set it. It MUST be a single atomic operation —
	// `SET key v NX EX lease`, one round-trip. An Exists-then-Set pair here would
	// reintroduce exactly the TOCTOU window this interface exists to close.
	// Returning an error makes Claim fail open (proceed with the dispatch).
	ClaimNX(ctx context.Context, key string, lease time.Duration) (bool, error)
	// SetWithTTL overwrites key with the given TTL (backs Commit): the claim's
	// short lease is replaced by the full dedup window once the dispatch succeeds.
	SetWithTTL(ctx context.Context, key string, ttl time.Duration) error
	// Del removes key (backs Release), freeing a failed dispatch for redelivery.
	Del(ctx context.Context, key string) error
}

// RedisDedupOption customizes a RedisDedup.
type RedisDedupOption func(*RedisDedup)

// WithRedisDedupPrefix sets the key namespace (default "kanz:dedup:"). Use a
// per-environment or per-stream prefix to keep tenants/streams from colliding in
// a shared Redis.
func WithRedisDedupPrefix(prefix string) RedisDedupOption {
	return func(d *RedisDedup) { d.prefix = prefix }
}

// WithRedisDedupContext sets the base context for Redis calls (default
// context.Background). Use a deadline-bearing ctx to bound a slow Redis so a
// hung call can't stall the consumer.
func WithRedisDedupContext(ctx context.Context) RedisDedupOption {
	return func(d *RedisDedup) {
		if ctx != nil {
			d.ctx = ctx
		}
	}
}

// WithRedisDedupErrorHandler sets the observability hook invoked (with the op
// name "claim"/"commit"/"release") on a Redis error before failing open. Nil ⇒
// errors are silently swallowed (still fail-open).
func WithRedisDedupErrorHandler(fn func(op string, err error)) RedisDedupOption {
	return func(d *RedisDedup) { d.onErr = fn }
}

// WithRedisDedupLease bounds how long a claimed-but-unfinished key stays claimed
// (default 5s). This is the crash window: a worker that dies mid-dispatch strands
// its claim until the lease expires, and only then is the event reprocessed. Size
// it ABOVE the consumer's worst-case dispatch time including retry backoff — a
// dispatch that outlives its lease can be claimed concurrently by a redelivery,
// which is the very race the claim exists to prevent.
func WithRedisDedupLease(lease time.Duration) RedisDedupOption {
	return func(d *RedisDedup) {
		if lease > 0 {
			d.lease = lease
		}
	}
}

// NewRedisDedup builds a distributed Deduper over client with the given TTL
// (size it to the broker dedup window, like DedupWindow — default 2m). Returns
// nil (a no-op Deduper, nil-receiver safe) when client is nil or ttl<=0, so
// "disabled" behaves identically to NewDedupWindow.
func NewRedisDedup(client RedisClient, ttl time.Duration, opts ...RedisDedupOption) *RedisDedup {
	if client == nil || ttl <= 0 {
		return nil
	}
	lease := defaultClaimLease
	if ttl < lease {
		lease = ttl
	}
	d := &RedisDedup{
		client: client,
		ttl:    ttl,
		lease:  lease,
		prefix: "kanz:dedup:",
		ctx:    context.Background(),
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Claim atomically takes key across every replica — one SET NX EX round-trip.
// Fails OPEN (true) on a Redis error, so an outage degrades to no-dedup rather
// than stalling consumption.
func (d *RedisDedup) Claim(key string) bool {
	if d == nil || key == "" {
		return true
	}
	ok, err := d.client.ClaimNX(d.ctx, d.prefix+key, d.lease)
	if err != nil {
		d.reportErr("claim", err)
		return true // fail open: process rather than block
	}
	return ok
}

// Commit promotes a claim to the full dedup window after a successful dispatch.
func (d *RedisDedup) Commit(key string) {
	if d == nil || key == "" {
		return
	}
	if err := d.client.SetWithTTL(d.ctx, d.prefix+key, d.ttl); err != nil {
		d.reportErr("commit", err)
	}
}

// Release drops a claim after a failed dispatch so a redelivery can retry. On a
// Redis error the key is simply left to its lease, which expires — the failure
// mode is a delayed retry, never a lost event.
func (d *RedisDedup) Release(key string) {
	if d == nil || key == "" {
		return
	}
	if err := d.client.Del(d.ctx, d.prefix+key); err != nil {
		d.reportErr("release", err)
	}
}

func (d *RedisDedup) reportErr(op string, err error) {
	if d.onErr != nil {
		d.onErr(op, err)
	}
}

// Compile-time assertion RedisDedup satisfies the interface.
var _ Deduper = (*RedisDedup)(nil)
