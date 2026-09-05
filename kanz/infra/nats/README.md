# NATS JetStream — live spine

Provisions the NATS JetStream cluster that carries the Kanz event backbone's
**live spine**. Kafka (EVT-09) is the durable log of record; NATS holds a
short-retention live tier for low-latency fan-out.

## Layout

| File | Purpose |
|---|---|
| `namespace.yaml` | `kanz-messaging` namespace (SPIFFE-enabled label, SEC-01a) |
| `nats.yaml` | 3-node JetStream cluster — ConfigMap, headless + client Services, StatefulSet, PodDisruptionBudget, SEC-01c mTLS + spiffe-helper |
| `bootstrap-job.yaml` | ConfigMap + Job that creates the streams/consumers (idempotent) |
| `tenancy.yaml` | MT-01c: per-tenant accounts (SYS + `__system__` + tenant template), included by `nats.conf` |
| `smoke-test.sh` | pub/sub + persistence smoke test |

## Streams

One stream per domain, binding `{domain}.>` per
`kanz-schemas/README.md` § Subject / Topic Taxonomy §4. File storage, 3 replicas, `limits`
retention, `old`-discard, 2m duplicate window (broker-side idempotency keyed on
the envelope `idempotency_key`).

| Stream | Subjects | Max age |
|---|---|---|
| `MARKET` `INFERENCE` | `<domain>.>` | 24h |
| `RISK` | `risk.portfolio.>` + `risk.exposure.>` + `risk.signal.>` + `risk.command.>` + `risk.curve.>` + `risk.factor.>` | 24h |
| `EXECUTION` | `execution.>` + `strategy.>` + `order.>` (the signal→order path) | 24h |
| `PLATFORM` `DATA` | `<domain>.>` | 168h (lifecycle + FACT-grade data-quality) |
| `OBSERVABILITY` | `observability.>` | 1h (ephemeral metrics) |
| `DLQ` | `dlq.>` | 720h (poison-event investigation runway) |

`RISK` is NOT `risk.>`, and has not been since `POSITION` was split out: a subject
belongs to exactly one stream, so `risk.position.>` needed its own compacted,
never-ageing stream. The consequence for a NEW `risk.*` subject is that it is
UNBOUND until `bootstrap-job.yaml` names it explicitly, and a JetStream publish
to an unbound subject is a hard error rather than a silent drop —
`test/arch`'s `TestEverySubjectIsCarriedByAStream` is what turns that into a red
build instead of a production denial.

Replay streams (`replay.{run_id}.*`) are created per-run by replay tooling
(EVT-20), not provisioned here.

## mTLS (SEC-01c)

The client listener requires + verifies a client cert (`tls { verify: true }`),
so a plaintext or untrusted client is rejected. The server presents its SPIRE
SVID: a `spiffe-helper` sidecar fetches it from the SEC-01a agent socket (SPIFFE
CSI volume) and writes `svid.pem`/`svid_key.pem`/`bundle.pem` to a memory-backed
`/etc/nats-certs`; on rotation it signals `nats-server --signal reload` (shared
PID namespace) so no restart is needed. Go clients connect with
`bus.NATSConfig.TLSConfig` from `transport.ClientTLSConfig` (SEC-01b) — the
client SVID is both the encryption and the auth credential.

Requires SPIRE (SEC-01a) deployed; the smoke test below needs a client SVID or a
plaintext listener.

## Tenant isolation (MT-01c)

Tenants are isolated by **NATS accounts** — the broker-native boundary; one
account's subjects are physically invisible to another. `tls.verify_and_map`
maps the client's SVID URI SAN to a per-tenant user → account, so isolation is
enforced by *which account a connection lands in*, not by the subject string —
the bus keeps publishing the unchanged 3-segment logical subject
(`kanz-schemas/README.md` § Subject / Topic Taxonomy §6). `tenancy.yaml` ships the `SYS`, `__system__`
(platform + pre-tenancy events), and an example `acme` tenant account; each
tenant account carries its own JetStream quota (noisy-neighbor bound, tightened
in MT-01e).

**Add a tenant**: append an account + a user keyed on the tenant workload's
SPIFFE URI to `tenancy.yaml`, `kubectl apply`, then `nats-server --signal
reload`. Dynamic JWT account-resolver onboarding (no reload) is the MT-01f path.

**Per-tenant streams**: streams are per-account, so run the `nats-bootstrap`
script connected on the tenant account (a pod under the tenant ServiceAccount,
so its SVID maps into that account). Same per-domain stream layout as the
platform account.

## Deploy

`tenancy.yaml` must be applied **before** `nats.yaml`: it declares the
`nats-tenants` ConfigMap that `nats.yaml` projects into `/etc/nats`. Apply
`nats.yaml` first and every NATS pod sits in `ContainerCreating` on
`configmap "nats-tenants" not found`.

```sh
kubectl apply -f namespace.yaml -f tenancy.yaml -f nats.yaml
kubectl -n kanz-messaging rollout status statefulset/nats   # wait for ready
kubectl apply -f bootstrap-job.yaml
kubectl -n kanz-messaging wait --for=condition=complete job/nats-bootstrap
```

## Smoke test

```sh
kubectl -n kanz-messaging port-forward svc/nats 4222:4222 &
NATS_URL=nats://localhost:4222 ./smoke-test.sh
```

Expects the `nats` CLI locally (or run inside `natsio/nats-box`). Publishes
under `market.smoketest.>`, asserts live delivery + JetStream persistence, then
purges the test subject.

### Reading stream contents on an mTLS-only cluster

A bare `kubectl run nats-box` cannot connect: the pod carries no SPIFFE volume
and no identity `verify_and_map` can map, so it is refused at the handshake.
`bootstrap-job.yaml` already carries the SPIFFE mount, ServiceAccount and
NetworkPolicy an ad-hoc diagnostic pod needs — reuse its shape and swap only
the container command:

```sh
python3 - <<'PY' > /tmp/nats-diag.yaml
import yaml
docs=[d for d in yaml.safe_load_all(open("bootstrap-job.yaml")) if d]
job=[d for d in docs if d.get("kind")=="Job"][0]
job["metadata"]["name"]="nats-diag"
job["spec"]["template"]["spec"]["containers"][0]["command"]=["sh","-c",
  "nats --server $NATS_URL stream subjects EXECUTION"]
print(yaml.safe_dump(job))
PY
kubectl delete job -n kanz-messaging nats-diag --ignore-not-found
kubectl apply -f /tmp/nats-diag.yaml
kubectl -n kanz-messaging logs job/nats-diag -c bootstrap
```

## Incident runbooks

The two broker failure modes an on-call is paged for. They cover **NATS and
Kafka together** — a consumer that has fallen behind cannot tell which side
stalled, and the confirm steps below are what separate them. The shape and the
GameDay discipline behind these pages are described in
`infra/observability/alerts/README.md`.

### Broker outage (NATS / Kafka)

Severity **page** · SLO `event-bus/delivery-success` · rehearsed by `infra/chaos/broker-kill.yaml`.


#### Symptom
`event-bus/delivery-success` burning and/or consumer lag climbing. Broker pods
not Ready in `kanz-messaging`.

#### Confirm
```promql
sum(kanz_bus_consumer_lag)                                    # rising
sum(increase(kanz_bus_consume_total{result="error"}[10m]))   # DLQ-routed terminal failures
sum(increase(kanz_data_gap_missing_total[15m]))              # MUST stay 0 — gap = real loss
```
```sh
kubectl get pods -n kanz-messaging -l app=nats
kubectl get pods -n kanz-messaging -l app=kafka
```

#### Impact
Events queue rather than drop (NATS JetStream / Kafka are durable). Downstream
consumers fall behind; the risk-engine ages into degraded mode (see
the partition runbook below). A non-zero `kanz_data_gap_missing_total` means the loss
escaped durability — escalate.

#### Mitigate
1. Let the broker StatefulSet self-heal (pod reschedules, partition leadership
   moves). Do **not** delete PVCs — that's the durable log.
2. If a single pod is wedged, delete it to force reschedule:
   `kubectl delete pod <pod> -n kanz-messaging`.
3. Verify the DLQ for poisoned messages once recovered (`dlq.<subject>`); replay
   if the original failure was transient.

#### Recover
Lag drains to baseline after consumers reconnect (they reconnect automatically).
Confirm `sum(kanz_bus_consumer_lag)` back to baseline and no new gaps.

#### Root-cause pointers
DLQ/retry design: `pkg/bus/{consumer,retry}.go`. Broker topology:
`infra/nats/`, `infra/kafka/`. DATA-05 reconciliation confirms NATS↔Kafka stayed
matched through the outage.

### Broker partition → risk-engine degraded mode

Severity **page** · SLO `market-data/freshness` · rehearsed by `infra/chaos/network-partition.yaml`.


#### Symptom
Risk query responses tagged **stale** (RISK-11 QualityFlags); staleness lag
climbing while the risk-engine itself stays Ready (not crash-looping).

#### Confirm
```promql
max(kanz_data_staleness_lag_seconds)        # > 30s (FRESH→DEGRADED), > 5m (full degraded)
```
```sh
kubectl get pods -n kanz-services -l app=risk-engine          # Ready, not CrashLoop
kubectl exec -n kanz-services <risk-engine-pod> -- nc -vz nats.kanz-messaging 4222   # reachability
```

#### Impact
The engine serves **last-known-good** results tagged stale rather than erroring
— degraded mode working as designed. Decisions are made on an aging view; the
older the AsOf, the less trustworthy. This is graceful, not an outage.

#### Mitigate
1. This is a *network/connectivity* problem, not a compute one — do not restart
   the engine (restart loses nothing but doesn't fix the partition and resets
   the warm cache).
2. Restore connectivity between `kanz-services` and `kanz-messaging`
   (NetworkPolicy, CNI, broker reachability). Confirm with the `nc` check above.

#### Recover
On heal the consumer reconnects and replays the backlog; AsOf catches up and
mode returns to FRESH. Confirm `max(kanz_data_staleness_lag_seconds)` back under
the 30s freshness budget and responses no longer tagged stale. No data gap
(`kanz_data_gap_missing_total` flat) — events were delayed, not lost.

#### Root-cause pointers
Degraded-mode design + thresholds (`DefaultFreshnessBudget` 30s,
`DefaultDegradedThreshold` 5m): `internal/risk/degraded.go`.
