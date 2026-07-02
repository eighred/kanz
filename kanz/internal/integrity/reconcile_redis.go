package integrity

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// RedisEval is the minimal Redis surface the shared PendingStore needs: atomic
// server-side script evaluation (PARITY-04h / DEBT-02b). The integrity package
// does NOT import a Redis client — the deployment implements this one method
// over go-redis / Dragonfly / a cluster client at the composition root (the same
// decoupling bus.RedisClient uses for RedisDedup; a concrete binding ships in
// pkg/redisadapter behind the `redis` build tag). Keeping it to a single Eval
// puts the atomicity where it must live — in the Lua scripts below, on the
// server — so no consumer pays for a Redis dependency it doesn't use.
type RedisEval interface {
	// Eval runs script atomically on the server with keys[0] as the pending-set
	// hash and returns the raw reply (a string, an int64, or a []any of strings,
	// matching the scripts below). A Redis error propagates so the store can
	// degrade rather than fabricate a reconciliation result.
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
}

// The pending set is one Redis hash: field = idempotency_key, value =
// "<transportInt>:<firstSeenUnixNano>". Only ClaimOrMatch must be atomic (the
// coordination.md / reconcile.go contract) — it is one HGET-then-(HSET|HDEL), so
// it runs as a single Lua script. SweepExpired/Count carry no atomicity
// constraint but ride the same Eval seam to keep the interface to one method.
const (
	// claimScript: KEYS[1]=hash, ARGV = key, transportInt, nowNano.
	// absent ⇒ HSET + "P"; present same-transport ⇒ "D"; present other ⇒ HDEL +
	// "M:<transportInt>:<firstSeenNano>".
	claimScript = `local v = redis.call('HGET', KEYS[1], ARGV[1])
if not v then
  redis.call('HSET', KEYS[1], ARGV[1], ARGV[2]..':'..ARGV[3])
  return 'P'
end
local sep = string.find(v, ':')
if string.sub(v, 1, sep-1) == ARGV[2] then
  return 'D'
end
redis.call('HDEL', KEYS[1], ARGV[1])
return 'M:'..v`

	// sweepScript: KEYS[1]=hash, ARGV[1]=cutoffNano. Deletes and returns every
	// entry whose firstSeen < cutoff, each as "<transportInt>:<firstSeenNano>:<key>".
	sweepScript = `local all = redis.call('HGETALL', KEYS[1])
local out = {}
local cutoff = tonumber(ARGV[1])
for i=1,#all,2 do
  local field = all[i]
  local val = all[i+1]
  local sep = string.find(val, ':')
  local seen = tonumber(string.sub(val, sep+1))
  if seen < cutoff then
    redis.call('HDEL', KEYS[1], field)
    table.insert(out, val..':'..field)
  end
end
return out`

	// countScript: KEYS[1]=hash. Returns HLEN.
	countScript = `return redis.call('HLEN', KEYS[1])`
)

// DefaultReconcilePendingKey is the Redis hash holding the cross-transport
// pending set. Override with WithRedisPendingKey to isolate environments/streams
// sharing one Redis.
const DefaultReconcilePendingKey = "kanz:reconcile:pending"

// RedisPendingStore is the distributed PendingStore (PARITY-04h): the reconciler
// pending set lives in a Redis hash every replica shares, so a NATS sighting on
// one pod matches the Kafka sighting on another (what the in-memory store cannot
// do). Drop it into NewReconcilerWithStore for the multi-replica cutover
// (PARITY-05c).
//
// # Failure policy — degrade to per-process, never fabricate
//
// The PendingStore contract has no context/error channel, so a Redis outage is
// handled here: ClaimOrMatch degrades to ReconcilePending (records nothing on
// the shared side — it must never invent a match), and SweepExpired/Count return
// empty. Both are reported via the error hook. The correctness floor stays the
// idempotent handlers (event-class-rules §1); the shared store only removes
// FALSE cross-replica discrepancies, so losing it during an outage regresses to
// per-process behavior rather than incorrectness.
type RedisPendingStore struct {
	eval  RedisEval
	key   string
	ctx   context.Context
	onErr func(op string, err error)
}

// RedisPendingStoreOption customizes a RedisPendingStore.
type RedisPendingStoreOption func(*RedisPendingStore)

// WithRedisPendingKey sets the pending-set hash key (default
// DefaultReconcilePendingKey).
func WithRedisPendingKey(key string) RedisPendingStoreOption {
	return func(s *RedisPendingStore) {
		if key != "" {
			s.key = key
		}
	}
}

// WithRedisPendingContext sets the base context for Redis calls (default
// context.Background). Use a deadline-bearing ctx so a hung Redis can't stall
// the reconciler.
func WithRedisPendingContext(ctx context.Context) RedisPendingStoreOption {
	return func(s *RedisPendingStore) {
		if ctx != nil {
			s.ctx = ctx
		}
	}
}

// WithRedisPendingErrorHandler sets the observability hook invoked (with op
// "claim"/"sweep"/"count") on a Redis error before degrading. Nil ⇒ errors are
// swallowed (still degrade safely).
func WithRedisPendingErrorHandler(fn func(op string, err error)) RedisPendingStoreOption {
	return func(s *RedisPendingStore) { s.onErr = fn }
}

// NewRedisPendingStore builds a distributed PendingStore over eval. Returns a
// true nil PendingStore when eval is nil — so NewReconcilerWithStore's nil check
// falls back to the in-memory default, and "disabled" behaves identically to the
// per-process store without hitting the typed-nil-interface trap. (Returning the
// interface, not *RedisPendingStore, is what makes that nil real.)
func NewRedisPendingStore(eval RedisEval, opts ...RedisPendingStoreOption) PendingStore {
	if eval == nil {
		return nil
	}
	s := &RedisPendingStore{eval: eval, key: DefaultReconcilePendingKey, ctx: context.Background()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ClaimOrMatch runs the atomic claim-or-match Lua script. On a Redis error it
// degrades to ReconcilePending (records nothing) rather than fabricating a match.
func (s *RedisPendingStore) ClaimOrMatch(key string, transport Transport, now time.Time) (ReconcileStatus, Transport, time.Time) {
	reply, err := s.eval.Eval(s.ctx, claimScript, []string{s.key},
		key, strconv.Itoa(int(transport)), strconv.FormatInt(now.UnixNano(), 10))
	if err != nil {
		s.reportErr("claim", err)
		return ReconcilePending, TransportUnspecified, time.Time{}
	}
	str := asString(reply)
	switch {
	case str == "P":
		return ReconcilePending, TransportUnspecified, time.Time{}
	case str == "D":
		return ReconcileDuplicate, TransportUnspecified, time.Time{}
	case strings.HasPrefix(str, "M:"):
		ft, fs := parsePendingEntry(strings.TrimPrefix(str, "M:"))
		return ReconcileMatched, ft, fs
	default:
		s.reportErr("claim", fmt.Errorf("integrity: unexpected claim reply %q", str))
		return ReconcilePending, TransportUnspecified, time.Time{}
	}
}

// SweepExpired removes and returns entries older than deadline as of now.
func (s *RedisPendingStore) SweepExpired(deadline time.Duration, now time.Time) []Discrepancy {
	cutoff := now.Add(-deadline).UnixNano()
	reply, err := s.eval.Eval(s.ctx, sweepScript, []string{s.key}, strconv.FormatInt(cutoff, 10))
	if err != nil {
		s.reportErr("sweep", err)
		return nil
	}
	items, ok := reply.([]any)
	if !ok {
		return nil
	}
	var out []Discrepancy
	for _, it := range items {
		transport, firstSeen, key := parseSweepEntry(asString(it))
		if key == "" {
			continue
		}
		out = append(out, Discrepancy{
			Key:       key,
			SeenOn:    transport,
			MissingOn: transport.other(),
			FirstSeen: firstSeen,
			Age:       now.Sub(firstSeen),
		})
	}
	return out
}

// Count is the number of pending entries.
func (s *RedisPendingStore) Count() int {
	reply, err := s.eval.Eval(s.ctx, countScript, []string{s.key})
	if err != nil {
		s.reportErr("count", err)
		return 0
	}
	return asInt(reply)
}

func (s *RedisPendingStore) reportErr(op string, err error) {
	if s.onErr != nil {
		s.onErr(op, err)
	}
}

// parsePendingEntry decodes "<transportInt>:<firstSeenNano>".
func parsePendingEntry(s string) (Transport, time.Time) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return TransportUnspecified, time.Time{}
	}
	ti, _ := strconv.Atoi(parts[0])
	nano, _ := strconv.ParseInt(parts[1], 10, 64)
	return Transport(ti), time.Unix(0, nano)
}

// parseSweepEntry decodes "<transportInt>:<firstSeenNano>:<key>". SplitN(…,3)
// keeps any colons in the idempotency_key intact.
func parseSweepEntry(s string) (Transport, time.Time, string) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return TransportUnspecified, time.Time{}, ""
	}
	ti, _ := strconv.Atoi(parts[0])
	nano, _ := strconv.ParseInt(parts[1], 10, 64)
	return Transport(ti), time.Unix(0, nano), parts[2]
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}

func asInt(v any) int {
	switch t := v.(type) {
	case int64:
		return int(t)
	case int:
		return t
	case string:
		n, _ := strconv.Atoi(t)
		return n
	default:
		return 0
	}
}

// Compile-time assertion RedisPendingStore satisfies the interface.
var _ PendingStore = (*RedisPendingStore)(nil)
