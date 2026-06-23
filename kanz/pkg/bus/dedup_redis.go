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
// # Best-effort, fail-open
//
// Redis dedup is an OPTIMIZATION that cuts cross-pod duplicate side-effects — it
// is not the correctness guarantee. The authoritative defense is idempotent
// handlers (event-class-rules §1). So a Redis error fails OPEN: Seen returns
// false and Record is a no-op (both reported via the error hook), degrading to
// "no dedup" rather than blocking consumption when Redis is down. And because
// Seen and Record are distinct (Record only after a successful dispatch), two
// replicas can both pass Seen before either Records the key — same non-atomic
// caveat as DedupWindow, now cross-pod, and the same answer: idempotent handlers.
type RedisDedup struct {
	client RedisClient
	ttl    time.Duration
	prefix string
	ctx    context.Context
	onErr  func(op string, err error)
}

// RedisClient is the minimal Redis surface RedisDedup needs. Implement it over
// go-redis, Dragonfly, or a cluster client at the composition root. Methods take
// a context so the adapter can bound each call; RedisDedup passes its base ctx.
type RedisClient interface {
	// Exists reports whether key is present (backs Seen). Returning an error
	// makes Seen fail open (treat as not-seen).
	Exists(ctx context.Context, key string) (bool, error)
	// SetWithTTL records key with the given TTL (backs Record). An overwrite of
	// an existing key is fine — the TTL simply refreshes.
	SetWithTTL(ctx context.Context, key string, ttl time.Duration) error
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
// name "seen"/"record") on a Redis error before failing open. Nil ⇒ errors are
// silently swallowed (still fail-open).
func WithRedisDedupErrorHandler(fn func(op string, err error)) RedisDedupOption {
	return func(d *RedisDedup) { d.onErr = fn }
}

// NewRedisDedup builds a distributed Deduper over client with the given TTL
// (size it to the broker dedup window, like DedupWindow — default 2m). Returns
// nil (a no-op Deduper, nil-receiver safe) when client is nil or ttl<=0, so
// "disabled" behaves identically to NewDedupWindow.
func NewRedisDedup(client RedisClient, ttl time.Duration, opts ...RedisDedupOption) *RedisDedup {
	if client == nil || ttl <= 0 {
		return nil
	}
	d := &RedisDedup{
		client: client,
		ttl:    ttl,
		prefix: "kanz:dedup:",
		ctx:    context.Background(),
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Seen reports whether key is in the distributed window. Fails open (false) on a
// Redis error so an outage degrades to no-dedup rather than blocking.
func (d *RedisDedup) Seen(key string) bool {
	if d == nil || key == "" {
		return false
	}
	ok, err := d.client.Exists(d.ctx, d.prefix+key)
	if err != nil {
		d.reportErr("seen", err)
		return false
	}
	return ok
}

// Record marks key seen for ttl. Best-effort — a Redis error is reported and
// swallowed (idempotent handlers are the real guarantee).
func (d *RedisDedup) Record(key string) {
	if d == nil || key == "" {
		return
	}
	if err := d.client.SetWithTTL(d.ctx, d.prefix+key, d.ttl); err != nil {
		d.reportErr("record", err)
	}
}

func (d *RedisDedup) reportErr(op string, err error) {
	if d.onErr != nil {
		d.onErr(op, err)
	}
}

// Compile-time assertion RedisDedup satisfies the interface.
var _ Deduper = (*RedisDedup)(nil)
