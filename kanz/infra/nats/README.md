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
`kanz-schemas/docs/subject-taxonomy.md` §4. File storage, 3 replicas, `limits`
retention, `old`-discard, 2m duplicate window (broker-side idempotency keyed on
the envelope `idempotency_key`).

| Stream | Subjects | Max age |
|---|---|---|
| `MARKET` `RISK` `INFERENCE` | `<domain>.>` | 24h |
| `EXECUTION` | `execution.>` + `strategy.>` + `order.>` (the signal→order path) | 24h |
| `PLATFORM` `DATA` | `<domain>.>` | 168h (lifecycle + FACT-grade data-quality) |
| `OBSERVABILITY` | `observability.>` | 1h (ephemeral metrics) |
| `DLQ` | `dlq.>` | 720h (poison-event investigation runway) |

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
(`subject-taxonomy.md` §6). `tenancy.yaml` ships the `SYS`, `__system__`
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
