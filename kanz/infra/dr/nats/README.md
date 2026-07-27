# DR-01c — NATS spine reconstruction

NATS is the **live spine** — a short-retention, low-latency fan-out tier, *not* a
system of record. Kafka is the durable log (replicated cross-region by DR-01a).
That split is what makes NATS **rebuildable**: there is nothing to replicate for
NATS itself; on failover you stand up an empty NATS and re-derive the live spine
from the Kafka log.

| File | Purpose |
|---|---|
| `rebuild-job.yaml` | one-shot Job that drains a recent window of the DR Kafka log back onto the live NATS subjects |

The Job runs `kanz/tools/natsrebuild` (reuses the EVT-20 read-only `replay.Reader`
as its Kafka source, but republishes onto the **original** subject with the
envelope **unchanged** — the inverse of replay, which isolates onto `replay.*`
and stamps `QUALITY_FLAG_REPLAYED`). Live consumers accept the rebuilt events
normally; idempotent handlers (DEBT-02) make re-consuming the window safe.

## Why only a recent window

The live tier's max-age is 24h (market/risk/execution/inference) — so the spine
only needs the last ~24h to be functionally restored; older history stays in
Kafka where it belongs and is read on demand (replay/backtest). Rebuilding a
bounded window is what keeps this inside the RTO budget.

## RTO

Reconstruction is part of the **RTO ≤ 15min** target (validated live by DR-01e):

1. NATS cluster up + streams provisioned — `infra/nats` + `bootstrap-job` (~1–2min).
2. **This Job** drains the 24h window — minutes, bounded by topic volume and
   `tasks`/throughput; scale `resources` / shard topics across parallel Jobs if a
   high-volume topic dominates.
3. Start consumers (DR-01d) — they read the now-populated spine.

## Run (in the DR region, after Kafka + NATS are up)

```sh
kubectl apply -f ../../nats/             # cluster + streams (bootstrap-job)
kubectl apply -f rebuild-job.yaml
kubectl -n kanz-messaging logs job/nats-rebuild -f
# "nats-rebuild complete topics=… published=…" ⇒ spine restored.
```

Tune the window/topics via the Job env (`NATS_REBUILD_SINCE`,
`NATS_REBUILD_TOPICS`, `NATS_REBUILD_STATE_TOPICS`). Re-running is safe.

`NATS_REBUILD_STATE_TOPICS` matters more than the others: it names the
COMPACTED topics (`compliance.mandate`, `risk.position`) that must be read IN
FULL, never through `NATS_REBUILD_SINCE`'s time window. Compaction retains
the latest record per key regardless of age, so reading a compacted topic
through a window loses every key not rewritten inside it — a mandate armed
last week does not come back, and a compliance control that returns
DISARMED is exactly the failure this list exists to prevent. The binary
refuses to run with this variable empty; do not clear it to "simplify" the
Job.
