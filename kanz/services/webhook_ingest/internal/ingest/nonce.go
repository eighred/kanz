package ingest

import (
	"context"
	"errors"
	"time"

	"github.com/eighred/kanz/pkg/bus"
)

// ErrNonceStoreUnavailable: the replay defence could not be consulted.
//
// It is NOT ErrReplayed, and the difference is the whole point. "This alert is a
// duplicate" is a verdict and the caller must never retry it; "I could not find out
// whether this alert is a duplicate" is an outage, and the caller MUST retry it. The
// server maps this to 503, never 409 — answering 4xx here would make TradingView drop
// a live trading signal on the floor because our Redis blinked.
var ErrNonceStoreUnavailable = errors.New("ingest: replay defence unavailable")

// NonceStore claims (strategy, nonce) for exactly one delivery. It is the replay
// defence at the internet-facing perimeter, and it is a THREE-PHASE LEASE, not a
// consumption: Claim takes the nonce, and the caller then Commits it (this alert was
// decided — hold it) or Releases it (nothing acted on it — let a redelivery retry).
//
// # Why this is not bus.Deduper
//
// It is the same lease shape, and the in-process implementation IS a bus.DedupWindow —
// the lease logic is not duplicated. What differs is the ONE thing that cannot be
// expressed in bus.Deduper's `Claim(key) bool`: what happens when the store itself is
// unreachable.
//
// bus.RedisDedup fails OPEN, deliberately and correctly for its job — bus dedup
// suppresses duplicate WORK, and a Redis outage there should degrade to "do the work
// twice" rather than halt consumption. This store fails CLOSED. It is the replay
// defence on the path that submits orders to a live exchange, so "I cannot tell whether
// this alert already traded" must never resolve to "trade it again" — the same stance
// the halt gate takes when it loses the bus (EXEC-M6). A bool cannot carry "I don't
// know", so the claim returns an error.
type NonceStore interface {
	// Claim atomically takes key. It reports false when the key is already claimed or
	// committed (a genuine replay), and an ERROR when the store could not be reached —
	// which the caller must treat as a refusal to serve, not as a replay.
	Claim(ctx context.Context, key string) (bool, error)
	// Commit holds key for the full replay window: this alert was decided.
	Commit(ctx context.Context, key string)
	// Release frees key: nothing acted on this alert, so a redelivery may retry it.
	Release(ctx context.Context, key string)
}

// MemoryNonces is the single-replica nonce store: the SAME bus.DedupWindow the bus
// consumers use, so the atomic claim, the lease expiry and the release-on-failure
// semantics are one implementation rather than two. It cannot fail, so its Claim never
// errors.
//
// It is correct for EXACTLY ONE REPLICA. Two pods are two maps, and a re-delivered
// alert landing on the other pod is admitted a second time and fans out a second set of
// orders. The composition root refuses to run on this unless the deployment says so out
// loud.
type MemoryNonces struct{ w *bus.DedupWindow }

// NewMemoryNonces returns the in-process store, bounded by window and max entries.
func NewMemoryNonces(window time.Duration, max int) *MemoryNonces {
	return &MemoryNonces{w: bus.NewDedupWindow(window, max)}
}

func (m *MemoryNonces) Claim(_ context.Context, key string) (bool, error) {
	return m.w.Claim(key), nil
}
func (m *MemoryNonces) Commit(_ context.Context, key string)  { m.w.Commit(key) }
func (m *MemoryNonces) Release(_ context.Context, key string) { m.w.Release(key) }

// RedisNonces is the cross-pod nonce store (EXEC-M17): every replica claims against one
// Redis, so a re-delivered alert is a replay wherever it lands. This is what lets
// webhook-ingest — the one service the internet talks to — run more than a single pod.
//
// It speaks bus.RedisClient, the same minimal interface bus.RedisDedup uses and the
// same one pkg/redisadapter implements over go-redis, so no Redis client is linked into
// this binary's default build and the store is testable without a server.
//
// Every failure PROPAGATES. That is the entire reason this type exists rather than
// bus.RedisDedup: see NonceStore.
type RedisNonces struct {
	client bus.RedisClient
	window time.Duration
	lease  time.Duration
	prefix string
}

// NewRedisNonces returns the cross-pod store. lease bounds a claim held by a pod that
// dies mid-signal — the alert becomes re-deliverable when it expires, rather than being
// stranded forever. window is how long a DECIDED alert stays un-replayable.
func NewRedisNonces(client bus.RedisClient, window, lease time.Duration) *RedisNonces {
	if lease <= 0 || lease > window {
		lease = 5 * time.Second
	}
	return &RedisNonces{client: client, window: window, lease: lease, prefix: "kanz:webhook-nonce:"}
}

// Claim is a single atomic SET NX EX round-trip: two pods racing the same re-delivered
// alert both issue it, Redis serializes them, and exactly one wins. An Exists-then-Set
// pair here would reintroduce the very race this exists to close.
func (r *RedisNonces) Claim(ctx context.Context, key string) (bool, error) {
	ok, err := r.client.ClaimNX(ctx, r.prefix+key, r.lease)
	if err != nil {
		return false, errors.Join(ErrNonceStoreUnavailable, err)
	}
	return ok, nil
}

// Commit extends the claim's short lease to the full replay window.
//
// A failure here is logged by the caller and NOT fatal: the alert HAS been acted on, and
// the claim's lease still suppresses a redelivery until it expires. Refusing the webhook
// now would tell TradingView the signal failed when it did not — and a retry would then
// be a genuine double trade.
func (r *RedisNonces) Commit(ctx context.Context, key string) {
	_ = r.client.SetWithTTL(ctx, r.prefix+key, r.window)
}

// Release drops the claim so a redelivery of an alert nobody acted on can retry.
//
// A failure here is not fatal either: the claim expires with its lease anyway, so the
// worst case is that the retry waits out the lease instead of being admitted at once.
func (r *RedisNonces) Release(ctx context.Context, key string) {
	_ = r.client.Del(ctx, r.prefix+key)
}
