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
# "nats-rebuild complete tenants=… topics=… published=…" ⇒ the run finished.
```

**A completed run is not the same as a restored spine.** Read the per-tenant
summary, not just the last line: a tenant whose topics exist but held nothing in
the window is reported `empty=N` and named as barren, and the Job still exits 0
— because "the topic is there and had no traffic" is a legitimate outcome, while
looking identical to a real restore is not. A topic that does NOT exist is a
hard failure (exit 1), on the reasoning that a DR run reading a name nothing was
ever written to has no honest success to report.

Tune the window/topics/tenants via the Job env (`NATS_REBUILD_SINCE`,
`NATS_REBUILD_TOPICS`, `NATS_REBUILD_STATE_TOPICS`, `NATS_REBUILD_TENANTS`).
Re-running is safe.

`NATS_REBUILD_TENANTS` decides WHOSE data comes back. Unset ⇒ `__system__`
only, which is the historical behaviour and keeps the un-prefixed legacy
topics. Every other tenant's archived topics live under its own `{tenant}.`
prefix (MT-01c), so **a tenant absent from this list is a tenant with no DR** —
its history is not restored and, before this variable existed, nothing said so.
Set-but-empty is refused rather than defaulted: it means a value failed to
survive templating, and that run would restore nothing for anyone.

`NATS_REBUILD_STATE_TOPICS` matters more than the others: it names the
COMPACTED topics (`compliance.mandate`, `risk.position`) that must be read IN
FULL, never through `NATS_REBUILD_SINCE`'s time window. Compaction retains
the latest record per key regardless of age, so reading a compacted topic
through a window loses every key not rewritten inside it — a mandate armed
last week does not come back, and a compliance control that returns
DISARMED is exactly the failure this list exists to prevent. The binary
refuses to run with this variable empty; do not clear it to "simplify" the
Job.
