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
// forever — the claim carries a LEASE (redisClaimLease, 5s, deliberately shorter
// than the shortest AckWait and deliberately NOT configurable), so the event is
// reprocessed once it expires rather than being silently lost. That is the
// opposite choice from the in-process DedupWindow's lease; redisClaimLease's own
// comment explains why the two must differ.
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

// redisClaimLease bounds how long a claimed-but-unfinished key stays claimed in
// the CROSS-POD window. It is 5s, it is not configurable, and it deliberately
// reaches the OPPOSITE conclusion from dedupClaimLease (75s) — which is the
// single most surprising thing in this package, so here is why.
//
// The two windows make different assumptions about the CLAIM HOLDER'S LIVENESS,
// and the lease is the consequence of that assumption, not a tuning preference.
//
//   - DedupWindow is per-process. A claim that is still held necessarily means a
//     goroutine in THIS process is still running the handler, because a crash
//     takes the whole map with it. So the lease may safely outlast AckWait: when
//     the redelivery arrives and Claim refuses it, the refusal is TRUE — the
//     first copy really is in flight and really will finish — and the Consumer
//     acking the redelivery as a duplicate is correct.
//
//   - RedisDedup is cross-pod. A claim that is still held may be held by a pod
//     that DIED holding it, and Redis cannot tell the difference. Claim returns
//     only a bool, so the Consumer treats every refusal identically: ack, and
//     skip the dispatch (consumer.go). Give this lease the 75s of the in-process
//     one and a hard crash becomes SILENT PERMANENT LOSS — pod A claims and dies,
//     the broker redelivers at AckWait, pod B is refused by A's ghost claim, and
//     pod B acks an event nobody ever handled. Keeping the lease strictly BELOW
//     the shortest AckWait means the ghost has always expired by the time the
//     redelivery lands, so the redelivery reprocesses instead of discarding.
//
// WHAT THAT COSTS, STATED PLAINLY. A handler that outruns this 5s lease can be
// claimed concurrently by a redelivery — the inversion #237 removed from the
// in-process window survives here, on purpose, because the alternative is losing
// events on crash. It is bounded by two things: the class AckWaits in tuning.go
// are now sized above the worst-case handler, so a redelivery-while-in-flight is
// the pathological case rather than the routine one; and across pods the OMS's
// real arbiter is Store.Save's version predicate (#122), which REJECTS the
// losing writer rather than letting two pods overwrite each other. The one
// production wiring of this deduper is risk-engine (redis build tag,
// RISK_ENGINE_REDIS_URL set), whose handlers are in-memory risk folds and
// nowhere near 5s.
//
// Closing the gap properly needs Claim to distinguish "leased by someone who may
// be dead" from "already committed" so the Consumer can defer instead of ack —
// an interface change plus a Redis round-trip this issue does not buy.
const redisClaimLease = 5 * time.Second

// Compile-time assertion: the cross-pod lease must expire BEFORE the earliest
// possible redelivery, or a pod that crashes holding a claim causes its own
// redelivery to be acked and discarded. Raising redisClaimLease to or past
// minTunedAckWait makes this expression negative and the package stops
// compiling.
const _ = uint(minTunedAckWait - redisClaimLease - 1)

// NewRedisDedup builds a distributed Deduper over client with the given TTL
// (size it to the broker dedup window, like DedupWindow — default 2m). Returns
// nil (a no-op Deduper, nil-receiver safe) when client is nil or ttl<=0, so
// "disabled" behaves identically to NewDedupWindow.
//
// The claim lease is redisClaimLease and is NOT a caller knob: it is pinned
// below minTunedAckWait for crash-safety, and the previous WithRedisDedupLease
// option (which had no production call site) was the one way to break that from
// outside the package.
func NewRedisDedup(client RedisClient, ttl time.Duration, opts ...RedisDedupOption) *RedisDedup {
	if client == nil || ttl <= 0 {
		return nil
	}
	if ttl < redisClaimLease {
		ttl = redisClaimLease // widen the suppression horizon; never narrow the lease
	}
	d := &RedisDedup{
		client: client,
		ttl:    ttl,
		lease:  redisClaimLease,
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
