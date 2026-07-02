# Coordination & consistency guarantees (DEBT-02)

How the platform's stateful consumers behave when run as **N replicas**. Two
guarantee classes:

- **Best-effort (per-process).** State lives in each replica's memory. Correct
  under scale-out *because the handlers are idempotent* (event-class-rules §1) —
  the per-process state is an optimization (skip redundant work) and an
  in-instance safety net, not the correctness boundary. Re-baselines on restart.
- **Strong (shared-state).** State lives in a store every replica shares
  (Redis/Dragonfly, or the `platform.model` log), so the replicas coordinate.
  Opt-in: each component below ships the per-process default and a seam to inject
  the shared backend, so a deployment pays for coordination only where it needs it.

**The floor is always idempotent handlers.** Shared state reduces duplicate
*work* and false cross-replica divergence; it does not replace handler
idempotency. Where the shared op cannot be atomic across pods, the handler is
still the thing that makes a duplicate harmless.

## Per-component

| Component | Default (best-effort) | Shared-state mode (strong) | Seam |
|---|---|---|---|
| Bus dedup | `DedupWindow` (per-instance window) | `RedisDedup` (cross-pod seen-set) | `bus.WithDeduper` (DEBT-02a); go-redis binding `redisadapter.New` (PARITY-04h) |
| DATA-05 reconciler | in-memory `PendingStore` | `integrity.RedisPendingStore` (Lua-atomic, PARITY-04h) | `integrity.NewReconcilerWithStore` + `NewRedisPendingStore` (DEBT-02b) |
| PRED-09 model registry | process-local `Registry` | `CoordinatedRegistry` over `platform.model` | `registry.CoordinatedRegistry` (DEBT-02b) |
| Gap / staleness / watermark / drift detectors | per-process, per-partition | — (rely on per-partition ordering) | n/a |

The detectors need no shared state: each keys on a `partition_key` the bus
already routes consistently to one consumer in a group (per-partition single
owner), so per-process state is correct without coordination. Don't add shared
state where partitioning already gives single-ownership.

## Bus dedup (DEBT-02a)

`DedupWindow` is per-instance: a redelivery landing on a *different* replica
isn't recognized. `RedisDedup` puts the seen-set in Redis so dedup spans the
group. Both satisfy `bus.Deduper`; the Consumer pipeline only ever calls
`Seen`/`Record`, so it's a pure wiring choice.

```go
consumer, _ := bus.NewConsumer(sub, bus.WithDeduper(
    bus.NewRedisDedup(redisAdapter, 2*time.Minute)))
```

**Best-effort, fail-open.** `Seen` and `Record` are distinct (record only after a
*successful* dispatch), so two replicas can both pass `Seen` before either
`Record`s — same non-atomic caveat as the in-memory window, now cross-pod, same
answer: idempotent handlers. A Redis error fails *open* (Seen→false, Record
no-op): an outage degrades to no-dedup, never blocks consumption.

The bus does not import a Redis client — implement the two-method
`bus.RedisClient` over go-redis/Dragonfly at the composition root (the same
decoupling as `NATSConfig.TLSConfig` taking a ready `*tls.Config`):

```go
type goRedis struct{ c *redis.Client }
func (g goRedis) Exists(ctx context.Context, k string) (bool, error) {
    n, err := g.c.Exists(ctx, k).Result(); return n > 0, err
}
func (g goRedis) SetWithTTL(ctx context.Context, k string, ttl time.Duration) error {
    return g.c.Set(ctx, k, 1, ttl).Err()
}
```

## DATA-05 reconciler (DEBT-02b)

The reconciler matches a NATS sighting against the Kafka sighting of the same
`idempotency_key`. With per-process state, if the two sides land on different
replicas they never match and both age out as **false** discrepancies. The
shared-state mode puts the pending set in a `PendingStore` every replica shares:

```go
r := integrity.NewReconcilerWithStore(sharedStore, integrity.DefaultMatchDeadline, time.Now)
```

`PendingStore.ClaimOrMatch` **must be atomic per key** — it's a single
check-set-or-delete, and a non-atomic distributed impl would let two replicas
both record Pending and never match. `integrity.RedisPendingStore` (PARITY-04h)
realizes it as one atomic Lua script over a Redis hash (HGET → HSET, or HDEL +
return match); `SweepExpired`/`Count` are separate scripts with no atomicity
constraint. The store depends only on the one-method `integrity.RedisEval` seam —
the go-redis binding is `redisadapter.New` (a single value also satisfies
`bus.RedisClient`, so one connection serves dedup and reconciliation). A Redis
outage degrades ClaimOrMatch to Pending (never a fabricated match), leaving
idempotent handlers as the floor.

```go
r := integrity.NewReconcilerWithStore(
    integrity.NewRedisPendingStore(redisadapter.New(rdb)),
    integrity.DefaultMatchDeadline, time.Now)
```

## PRED-09 model registry (DEBT-02b)

The registry is process-local: a model promoted on one worker is invisible to the
others. `CoordinatedRegistry` mirrors every mutation onto the `platform.model`
topic and applies inbound events from peers, so the fleet converges.

`platform.model` is the append log of record (infinite retention, EVT-09): a
starting replica **replays it from offset 0 to rebuild the registry** — the topic
is the source of truth, the local `Registry` is the materialized cache. Events
carry `ModelMetadata` + role + validation (not the live `Model`, which isn't
serializable); `apply` rematerializes the `Model` via the injected `ModelLoader`.

- **Ordering / the primary gate:** publish keyed by `model_id` so a model's
  `record_validation` precedes its primary register in per-partition order —
  every replica satisfies the MLOPS-01a gate identically.
- **Loop prevention:** events carry the producing replica's `origin`; a replica
  skips applying its own.

## Choosing a mode

Reach for shared-state only when a flow's correctness or cost genuinely spans
replicas:

- **Dedup:** in-memory is enough when handlers are idempotent and you only want
  the in-instance optimization. Use Redis when duplicate *side-effect work*
  across pods is expensive (e.g. a costly external call the handler can't cheaply
  make idempotent).
- **Reconciler:** use the shared store whenever the NATS and Kafka feeds are
  consumed by *different* replicas (the common multi-replica case) — otherwise
  you get false discrepancies.
- **Model registry:** use `CoordinatedRegistry` for any multi-replica inference
  fleet that promotes models at runtime; the per-process registry is fine only
  for a single worker or config-push-+-restart deployments.
