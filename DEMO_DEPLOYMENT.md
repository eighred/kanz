# DEMO_DEPLOYMENT.md

Standing the Kanz trading loop up on a single VDS, trading **Binance Spot testnet**
through the real `venue-binance` adapter.

This is a **demo posture**, not production. It is written down because the dev rig
was rebuilt three times from memory and drifted from the repository every time; a
runbook with no re-run path is the defect this file exists to stop repeating.

> **Provenance.** §2–§4, §7, §9 and §10 were EXECUTED on 2026-07-25 against a
> Lightsail VDS (Ubuntu 24.04, 2 vCPU / 3.8 GB, k3s v1.36.2) and are corrected here
> to what actually worked. The loop was proven end to end on the in-process
> simulator: `submit → accepted → routed → filled`, instrument `AAPL.XNAS`, account
> `sim:XNAS`, all seven lifecycle subjects on the `EXECUTION` stream.
>
> **§5, §6 and §8 (the Binance adapter) remain UNEXECUTED** — they need real testnet
> credentials. Everything up to the venue swap is proven; the swap itself is not.
>
> **The bus runs REAL SPIFFE mTLS here, not the plaintext deviation** — see §4b. That
> is a better posture than the kind rig's and it is what closing five repo defects
> bought. Do not reintroduce plaintext to make something start.

---

## 0. What this delivers, and what it deliberately does not

**Delivers:** a signed TradingView-shaped alert → `webhook-ingest` → NATS → `OMS`
admission → `venue-binance` → a **real order at Binance testnet** → a fill FACT back
on the `EXECUTION` stream.

**Deliberately absent**, and each one is a stated deviation rather than an oversight:

| Absent | Why | Consequence |
|---|---|---|
| Vault | Not reachable from a single VDS (ONBOARD-M6) | Every DSN/credential is a Kubernetes Secret. `tools/rig_dev_patch.py` performs the rewrite. |
| OIDC | Cannot reach `login.eighred.com` | `api-gateway` authenticates with an HS256 dev key. |
| ~~NATS mTLS~~ | **NOT absent.** `nats.yaml` serves `tls { verify: true, verify_and_map: true }` — mTLS-only, and the in-repo SPIRE satisfies it | Pass `--keep-spiffe` to `rig_dev_patch.py` (§4b). Stripping the socket here breaks every workload. |
| `venue-okx` | Needs a second real credential, and OKX demo needs an `x-simulated-trading` header the adapter does not send yet | **§6 removes it from `OMS_VENUE_ENDPOINTS`. Leaving it in is fatal — see the warning there.** |
| `risk-engine` | An Argo Rollout; no Rollouts controller here | The `positions` projection does not advance. A fill lands on the bus and in the OMS's `orders` table, but not in `positions`. |
| Kafka | Not in the live trading loop | Analytics/lakehouse paths are dark. |

**This trades against a real exchange endpoint.** Binance testnet is not real money,
but the code path is identical to the one that spends it. Every guard below that
looks paranoid is load-bearing for the day the base URL changes.

---

## 1. Preconditions

**Host:** 4 vCPU / 8 GB RAM / 60 GB SSD, Ubuntu 22.04 or 24.04, root or sudo.

**2 vCPU / 3.8 GB also works** — that is what this was proven on (a Lightsail
4 GB plan). It needs the two accommodations §3 makes: 4 GB of swap, and building the
8-service loop subset instead of all 23. At rest the loop draws ~2.5 GB of the 3.8,
so there is headroom but not much; adding `risk-engine` or the ML services would
need the larger box.

**Binance testnet credentials:** register at <https://testnet.binance.vision> and
generate an API key + secret with **spot trading enabled**. Note the account's
**uid** — §5 uses it to make the adapter prove itself, which is the difference
between a demo and a demo you can trust.

**A public DNS name is needed only if TradingView is to POST in** (§11). A
curl-driven demo needs no inbound exposure at all.

**Local checkout on the VDS.** Every path below is repo-root-relative and the
scripts fail closed if run elsewhere.

> **The TUI's "Add Node" cannot do §2, and this is not a gap to close.** It calls
> `operator.v1.AddNode`, which runs a one-shot Job that SSHes to the host and
> executes `curl -sfL https://get.k3s.io | K3S_URL=... K3S_TOKEN=... sh -s - agent`
> (`kanz/cmd/kanz-provisioner/join.go`). That **joins an agent to an existing
> control plane**: it needs `OPERATOR_K3S_SERVER_URL` and `OPERATOR_K3S_TOKEN`, and
> the operator serving the RPC runs *inside* the cluster being extended. This VDS is
> node #1 — there is no server to join and no operator to ask. Bootstrap it by hand
> once (§2); **every node after it should be added through the TUI.**
>
> Note also that Add Node does **not** set up SSH access. Its form takes a
> `key_path` to a private key you already hold, and the matching public key must
> already be in the target's `authorized_keys`. The key is read off the UI thread,
> used once, and stored in a Job-owned Secret — it is never generated, never
> installed on the target, and never held in TUI state.

---

## 2. Host prep and k3s

```bash
sudo apt-get update && sudo apt-get install -y docker.io python3 python3-yaml git curl
sudo usermod -aG docker "$USER" && newgrp docker

# --disable traefik is REQUIRED, not taste: webhook-ingest-ingress.yaml declares
# ingressClassName: nginx. k3s's bundled Traefik would claim port 80/443 and then
# ignore that Ingress, which presents as "the webhook URL 404s" with no error
# anywhere. Install ingress-nginx in §11 if the TradingView path is wanted.
curl -sfL https://get.k3s.io | sh -s - --disable traefik --write-kubeconfig-mode 644

mkdir -p ~/.kube && sudo cat /etc/rancher/k3s/k3s.yaml > ~/.kube/config
export KUBECONFIG=~/.kube/config

# tools/rig-apply.sh pins every kubectl call to context "kind-$CLUSTER" so that
# --cluster actually selects the target instead of acting on whatever context is
# ambient. k3s names its context "default". Rename it so the repo's own scripts
# run UNMODIFIED — a hand-edited copy of rig-apply.sh on the host is exactly the
# drift this runbook exists to prevent.
kubectl config rename-context default kind-kanz-dryrun

kubectl get nodes   # must be Ready before continuing
```

> **Known rough edge.** That rename is a workaround for `rig-apply.sh` hardcoding
> the `kind-` context prefix. Making the context configurable is a small, real
> task — until then, do the rename rather than forking the script.

## 3. Build and load the images

```bash
cd /path/to/eighred-kanz

# 4 GB of swap FIRST. Go's linker peaks well above what 3.8 GB leaves free, and an
# OOM-killed build looks like a compiler error.
sudo fallocate -l 4G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo "/swapfile none swap sw 0 0" | sudo tee -a /etc/fstab

# tools/rig-images.sh --build does ALL 23 services in the CI matrix. On 2 vCPU that
# is ~40 wasted minutes: the loop needs 8. Filter the matrix rather than hand-listing
# Dockerfile paths, so the parse-don't-duplicate property still holds.
tools/rig-images.sh --list \
  | grep -E "^(oms|webhook-ingest|compliance|api-gateway|tv-sync|venue-binance|kanz-migrate|kanz-halt) " \
  | while read -r svc dockerfile; do
      docker build -f "$dockerfile" -t "ghcr.io/kanz-eng/$svc:latest" .
    done
```

Measured: ~40 min for all 8 — oms ~5 min, the rest ~2 min each once the shared
dependency layer is cached. **`buf` was not needed**: `kanz-schemas/gen/` came across
with the tree. Install it and run `buf generate` only if a build fails on a missing
SDK symbol.

`--load` is `kind`-only. k3s uses its own containerd namespace, so import instead:

```bash
for svc in oms webhook-ingest compliance api-gateway tv-sync venue-binance kanz-migrate kanz-halt; do
  docker save "ghcr.io/kanz-eng/$svc:latest" | sudo k3s ctr images import -
done
sudo k3s ctr images ls -q | grep kanz-eng   # expect 8
```

`rig_dev_patch.py` sets `imagePullPolicy: IfNotPresent` on every container, which
makes these imported images authoritative — nothing reaches for the private ghcr.

## 4. SPIRE, the spine, and the data stores

```bash
# SPIRE first. Every workload manifest mounts a csi.spiffe.io volume, and
# rig_dev_patch.py leaves those volumes ALONE (it strips only the SPIFFE env).
# Without the SPIFFE CSI driver the pods hang in ContainerCreating on an
# unsatisfiable mount — which reads like a scheduling problem and is not one.
tools/rig-apply.sh --spire --cluster kanz-dryrun

kubectl apply -f kanz/infra/nats/namespace.yaml
# tenancy.yaml BEFORE nats.yaml. It declares the nats-tenants ConfigMap that
# nats.yaml projects into /etc/nats; without it every NATS pod sits in
# ContainerCreating on `configmap "nats-tenants" not found`. The two have always
# been separate files and rig-apply.sh applies neither, so nothing ever forced
# them to be applied together.
kubectl apply -f kanz/infra/nats/tenancy.yaml
kubectl apply -f kanz/infra/nats/nats.yaml
kubectl -n kanz-messaging rollout status statefulset/nats --timeout=300s

# Creates the JetStream streams (EXECUTION, PLATFORM, ...). The mTLS variant —
# NOT bootstrap-job-dev-plaintext.yaml, see below.
kubectl apply -f kanz/infra/nats/bootstrap-job.yaml
kubectl -n kanz-messaging wait --for=condition=complete job/nats-bootstrap --timeout=300s
```

> **Use `bootstrap-job.yaml`, not `bootstrap-job-dev-plaintext.yaml`.** The broker
> serves mTLS only, so the plaintext variant cannot connect at all. It also depends
> on the ServiceAccount `nats-bootstrap`, which is declared *only* in the mTLS
> manifest — applied alone it never creates a pod, and the Job reports
> `serviceaccount "nats-bootstrap" not found` in its events rather than its logs.

> **NATS loses its streams across a node restart, and nothing re-provisions them.**
> The bootstrap Job has already `Completed`, and a completed Job does not re-run.
> Services then CrashLoopBackOff with `nats: API error ... stream not found`, which
> looks like a code bug and is not. **After any host reboot, do this first:**
> ```bash
> kubectl -n kanz-messaging delete job nats-bootstrap
> kubectl apply -f kanz/infra/nats/bootstrap-job.yaml
> ```

Now Postgres, Redis, and the dev Secrets:

```bash
kubectl apply -f kanz/infra/deploy/rig-dev-secrets.yaml
kubectl apply -f kanz/infra/deploy/postgres-dev.yaml
kubectl -n kanz-services rollout status deploy/postgres --timeout=180s

python3 tools/rig_dev_patch.py kanz/infra/messaging/redis.yaml | kubectl apply -f -
```

### 4a. venue-binance needs its own database — create it

`postgres-dev.yaml` creates only `kanzapp` and `tvsync`. `venue-binance` runs a
`kanz-migrate` init container against `/migrations/venue-binance`, whose first file
is `0001_venue_orders.sql`.

**It cannot share `kanzapp`.** `kanz-migrate` keys `schema_migrations` by version,
and the OMS's `0001_orders` already claims version 1 — a shared database makes the
adapter's migrate reject the OMS's migration as a modified copy of its own. This is
the identical collision that forced `tv-sync` onto a separate database.

**Create it in the manifest, not by hand.** `postgres-dev.yaml` backs its data
directory with an **`emptyDir`** — every Postgres restart re-initialises an empty
database and re-runs the `postgres-dev-initdb` ConfigMap. A `CREATE DATABASE` typed
into a shell survives exactly until the pod is rescheduled, and then `venue-binance`
CrashLoopBackOffs on a database that existed an hour ago. Add a third key to the
ConfigMap in `kanz/infra/deploy/postgres-dev.yaml`, beside `010-kanzapp.sql` and
`020-tvsync.sql`, **before applying it in §4**:

```yaml
  030-venuebinance.sql: |
    CREATE DATABASE venuebinance OWNER kanzapp;
    \connect venuebinance
    GRANT ALL ON SCHEMA public TO kanzapp;
```

This is a real repository gap, not a demo workaround: the repo declares
`venue-binance` and declares its own order-view database, and nothing creates that
database anywhere. It is worth forging as a task.

If Postgres is already up, the recovery path — valid **until the next restart** —
is:

```bash
kubectl -n kanz-services exec -i deploy/postgres -- \
  psql -U postgres -v ON_ERROR_STOP=1 <<'SQL'
CREATE DATABASE venuebinance OWNER kanzapp;
\connect venuebinance
GRANT ALL ON SCHEMA public TO kanzapp;
SQL
```

> The same `emptyDir` means **all order history is lost on a Postgres restart** —
> the OMS's `orders` table included. Acceptable for a demo; §13 is where it stops
> being acceptable.

### 4b. The bus is mTLS — patch with `--keep-spiffe`

`rig_dev_patch.py` strips `SPIFFE_ENDPOINT_SOCKET` by default. That is correct only
against a plaintext broker. Here the broker is mTLS-only and SPIRE is real, so the
socket is **required**: strip it and the mesh stays disabled, the client still meets
a TLS server, and every workload dies with

```
nats connect: nats: tls error: x509: certificate signed by unknown authority
```

— an error that reads like a missing CA and is actually a missing env var. Every
`rig_dev_patch.py` invocation from here on takes `--keep-spiffe`.

Confirm it worked by the one log line that distinguishes the two postures:

```bash
kubectl -n kanz-services logs deploy/webhook-ingest | grep "bus transport"
# {"msg":"bus transport","mtls":true}     <- correct
# {"msg":"bus transport","mtls":false}    <- socket missing; it will not connect
```

Three defects had to be fixed in the repo before any of this came up, all of them
consequences of `infra/nats/nats.yaml` never once having been applied to a cluster.
They are committed, so a fresh checkout does not hit them — listed here because they
explain why the manifests changed:

1. **`nats.yaml`** — the spiffe-helper config set `cmd` with no `renew_signal`, which
   spiffe-helper 0.9.0 rejects at startup. Init container CrashLoopBackOff, no bus.
2. **`nats.yaml`** — `tenants.conf` was subPath-mounted *inside* the read-only
   ConfigMap volume at `/etc/nats`, which runc cannot do
   (`not a directory`). Now one projected volume carrying both files.
3. **`registration.yaml`** — the DNS-name template `{{ index .PodMeta.Labels "app" }}`
   renders `.<ns>.svc` for any pod without an `app` label, and SPIRE rejects the
   **whole entry** as an invalid DNS name. Jobs carry no `app` label, so
   `nats-bootstrap` silently got no SVID and hung forever. This one is the widest:
   it denied identity to every Job in the estate.

A fourth was needed for the services themselves: `compliance`, `tv-sync` and
`webhook-ingest` read `SPIFFE_ENDPOINT_SOCKET` in their config but their manifests
declared neither the env nor the `csi.spiffe.io` volume, so they could never reach
an mTLS bus. Both are now in the manifests.

## 5. The Binance testnet credentials

These are the only real credentials in the system, and they are created **by hand,
out of band**. They must never be committed:
`kanz/test/arch/rig_dev_posture_test.go` fails the build if the strings `binance`,
`okx`, `api_key`, `apikey`, or `private_key` appear in `rig-dev-secrets.yaml`. That
guard is correct — do not work around it.

```bash
kubectl -n kanz-services create secret generic venue-binance-keys \
  --from-literal=api-key='<TESTNET_API_KEY>' \
  --from-literal=api-secret='<TESTNET_API_SECRET>'

kubectl -n kanz-services create secret generic venue-binance-db \
  --from-literal=database-url='postgres://kanzapp:kanz@postgres.kanz-services.svc:5432/venuebinance?sslmode=disable' \
  --from-literal=migrate-database-url='postgres://kanzapp:kanz@postgres.kanz-services.svc:5432/venuebinance?sslmode=disable'
```

> **Prefer the TUI once the operator is deployed.** The *Set API Keys* form calls
> `operator.v1.SetVenueKeys`, which writes Secret `venue-<venue>-keys` with data
> keys `api-key` / `api-secret` (`operator/internal/secrets/kube.go`) — the same
> object the command above creates, with the hyphens correct by construction, and
> write-only (the operator never reads a key back). It is the better path because it
> removes the hand-typed data key, which is the one thing here that silently
> half-works.
>
> It is not the default in this runbook only because `operator-deploy.yaml` is not
> in the trading-loop subset §7 applies. If you want it: apply
> `python3 tools/rig_dev_patch.py --keep-spiffe kanz/infra/deploy/operator-deploy.yaml | kubectl apply -f -`
> and use the form instead of the first command below. `venue-binance-db` has no TUI
> path either way — create it with `kubectl`.

> **The key names are hyphenated, and that is not cosmetic.** In production the CSI
> driver maps Vault's underscore `secretKey`s (`api_key`) onto hyphenated
> `objectName` files (`api-key`). `rig_dev_patch.py` replaces the CSI volume with a
> plain Secret mount, which has **no mapping layer** — the Secret's data key *is*
> the filename. An underscore here mounts `/run/secrets/binance/api_key` while the
> adapter reads `/run/secrets/binance/api-key`, and the adapter starts with an
> empty credential. The Secret name must likewise match the SecretProviderClass it
> shadows, name for name.

### Make the adapter prove its account

`venue-binance-deploy.yaml` ships with `BINANCE_VENUE_ACCOUNT_UID` **empty** and
`BINANCE_ALLOW_UNVERIFIED_ACCOUNT=true` — an explicit admission that nobody has
bound the account. At startup the adapter asks Binance which account the mounted
key belongs to (`GET /api/v3/account`) and refuses to serve orders if the answer
disagrees. With the uid empty it cannot check, so a wrong key posts fills to one
fund's ledger while the exchange debits another.

You have the testnet uid. Bind it — this is one edit and it converts the
demo's central claim from an assertion into a proof:

```bash
python3 tools/rig_dev_patch.py --keep-spiffe kanz/infra/deploy/venue-binance-deploy.yaml \
  > /tmp/venue-binance.yaml
# In /tmp/venue-binance.yaml:
#   BINANCE_VENUE_ACCOUNT_UID: "<your testnet uid>"
#   BINANCE_ALLOW_UNVERIFIED_ACCOUNT: "false"
kubectl apply -f /tmp/venue-binance.yaml
kubectl -n kanz-services rollout status deploy/venue-binance --timeout=180s
```

Confirm the base URL really is testnet before it takes an order:

```bash
kubectl -n kanz-services get deploy venue-binance \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="BINANCE_BASE_URL")].value}{"\n"}'
# expect: https://testnet.binance.vision
```

## 6. The OMS — Binance only

**`OMS_VENUE_ENDPOINTS` ships naming both `XBIN` and `XOKX`. Leaving `XOKX` in will
stop the OMS from starting.** At boot the OMS dials every configured adapter and
calls `venue.v1.Describe`; an adapter it cannot reach is fatal by design
(`cmd/oms/venues.go`), because booting without it would route those orders nowhere
or silently into a simulator. With no `venue-okx` deployed, that is exactly the
state you would be in.

```bash
python3 tools/rig_dev_patch.py --keep-spiffe kanz/infra/deploy/oms-deploy.yaml > /tmp/oms.yaml
```

`rig_dev_patch.py` **drops `OMS_VENUE_ENDPOINTS` entirely** — that is what makes the
kind rig fall back to its in-process simulator. This demo wants the opposite, so add
it back to `/tmp/oms.yaml`, Binance only, and bind the portfolio to the account:

```yaml
            - { name: OMS_VENUE_ENDPOINTS, value: "XBIN/binance-main=venue-binance.kanz-services.svc:9000" }
            # tenant/portfolio@MIC=account. tenant == fund_id at webhook-ingest.
            - { name: OMS_VENUE_ACCOUNTS, value: "fund-alpha/fund-alpha@XBIN=binance-main" }
            - { name: OMS_REQUIRE_VERIFIED_ACCOUNT, value: "true" }
            - { name: OMS_REQUIRE_VENUE_ACCOUNT, value: "true" }
```

Set the two `REQUIRE_*` flags to `"true"` **only because §5 bound the uid**. They
ship `false` deliberately: arming them without a bound account refuses every order,
which is a trading outage dressed as a control. With the binding in place they are
what makes the demo meaningful — the OMS now refuses to trade an unproven account
rather than merely counting it.

```bash
kubectl apply -f /tmp/oms.yaml
kubectl -n kanz-services rollout status deploy/oms --timeout=180s
kubectl -n kanz-services logs deploy/oms | grep -i "venue adapter registered"
# expect: account PROVEN against the exchange   mic=XBIN  account=binance-main
```

If that line says `UNVERIFIED`, stop. The uid did not bind, and every fill below
would be settling against an account nobody has checked.

## 7. The rest of the loop

```bash
for m in compliance-deploy.yaml api-gateway-deploy.yaml tv-sync-deploy.yaml; do
  python3 tools/rig_dev_patch.py --keep-spiffe "kanz/infra/deploy/$m" | kubectl apply -f -
done
```

## 8. Point webhook-ingest at Binance

The committed `webhook-ingest-config` maps `AAPL → AAPL.XNAS` and routes
`fund-alpha` to venue `XNAS` — the **simulator's** MIC. Against a real adapter those
orders would route to a MIC with no endpoint, which is a hard error at the router.
Replace the config so both sides name `XBIN`.

`BINANCE_SYMBOLS` in the adapter maps `BTC-USD=BTCUSDT`, so the instrument is
`BTC-USD.XBIN`:

```bash
kubectl -n kanz-services create secret generic webhook-ingest-config \
  --from-literal=config.json='{
    "strategies": {"dev": "dev-only-not-a-real-hmac-secret"},
    "symbols":    {"BTCUSD": "BTC-USD.XBIN"},
    "prices":     {"BTC-USD.XBIN": "60000"},
    "equity":     {"fund-alpha": "1000000"},
    "funds":      {"fund-alpha": [{"venue": "XBIN", "weight": "1"}]},
    "max_size":   "1000000",
    "max_leverage": "10"
  }' --dry-run=client -o yaml | kubectl apply -f -

python3 tools/rig_dev_patch.py --keep-spiffe kanz/infra/deploy/webhook-ingest-deploy.yaml | kubectl apply -f -
kubectl -n kanz-services rollout status deploy/webhook-ingest --timeout=180s
```

## 9. Open the halt gate

**The platform boots halted.** `webhook-ingest` answers `423 {"error":"trading
halted"}` until an operator resume FACT (`platform.mode.changed`) crosses the
`PLATFORM` stream. This is not a bug to route around.

A bare `kubectl run` **cannot** do this on an mTLS bus: the pod would have no SPIFFE
volume and no identity `verify_and_map` can map, so it is refused at the handshake.
Use the repo's Job, which carries its own namespace, ServiceAccount, NetworkPolicy
and SPIFFE mount:

```bash
sed -e "s|operator:CHANGE_ME|operator:$(whoami)|" \
    -e 's|--reason=CHANGE_ME|--reason=demo bring-up|' \
    -e 's|- "--reason=|- "--resume"\n            - "--reason=|' \
    kanz/infra/operator/halt-job.yaml > /tmp/halt-resume.yaml
python3 tools/rig_dev_patch.py --keep-spiffe /tmp/halt-resume.yaml | kubectl apply -f -
kubectl -n kanz-operator wait --for=condition=complete job/kanz-halt --timeout=180s
kubectl -n kanz-operator logs job/kanz-halt
# RESUMED — system mode NORMAL, by operator:...
```

`halt-job.yaml` ships with `--by=CHANGE_ME` / `--reason=CHANGE_ME` and no `--resume`;
the `sed` above supplies all three. Re-running it needs a
`kubectl delete job -n kanz-operator kanz-halt` first — a completed Job is immutable.

## 10. Fire an alert and watch the fill

```bash
kubectl -n kanz-services port-forward deploy/webhook-ingest 18090:8090 &

BODY='{"strategy_id":"dev","fund_id":"fund-alpha","symbol":"BTCUSD","action":"buy","size":"0.001","size_type":"absolute_qty","order_type":"limit","limit_price":"50000","nonce":"'"$(uuidgen)"'"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac 'dev-only-not-a-real-hmac-secret' -r | cut -d' ' -f1)

curl -sS -X POST localhost:18090/webhook/tradingview \
  -H 'Content-Type: application/json' -H "X-Signature: $SIG" -d "$BODY"
# expect: 202
```

Four things bite here, all previously paid for:

- **`size` and `limit_price` are STRINGS.** A JSON number returns
  `400 malformed webhook body: cannot unmarshal number into ... of type string`.
- **Send a LIMIT order with a `limit_price`.** A MARKET order needs a reference
  mark on the spine or is refused `PRICE_UNAVAILABLE`.
- **A fresh `nonce` every attempt** — a halt or a reject still burns it.
- **Sign the RAW body.** Any re-serialisation between signing and sending
  invalidates the signature.
- **The MICs on both sides must agree.** The committed `webhook-ingest-config` routes
  `fund-alpha` to venue `XNAS`, but the OMS's simulator defaults to
  `OMS_SIM_VENUE_MIC=XSIM`. Mismatched, the order is admitted, published, and then
  **rejected** with `VENUE_NOT_CONFIGURED: target venue "XNAS" is not configured on
  this OMS` — an outcome visible only as a FACT on the bus, since HTTP already
  returned 202. For the simulator path, make them agree:
  `kubectl -n kanz-services set env deploy/oms OMS_SIM_VENUE_MIC=XNAS`.
  For the Binance path this is moot — §6 and §8 both say `XBIN`.

Read the result off the bus. **A plain `kubectl run nats-box` cannot connect** —
the `nats` CLI needs the SVID as PEM files, which means the spiffe-helper init
container. Reuse the bootstrap Job's shape, swapping only its command:

```bash
python3 - <<'PY' > /tmp/nats-diag.yaml
import yaml
docs=[d for d in yaml.safe_load_all(open("kanz/infra/nats/bootstrap-job.yaml")) if d]
job=[d for d in docs if d.get("kind")=="Job"][0]
job["metadata"]["name"]="nats-diag"
job["spec"]["template"]["spec"]["containers"][0]["command"]=["sh","-c",
  "nats --server $NATS_URL stream subjects EXECUTION; "
  "nats --server $NATS_URL stream get EXECUTION --last-for order.order.filled | strings | tail -20"]
print(yaml.safe_dump(job))
PY
kubectl delete job -n kanz-messaging nats-diag --ignore-not-found
kubectl apply -f /tmp/nats-diag.yaml
kubectl -n kanz-messaging logs job/nats-diag -c bootstrap
```

A healthy simulator run shows seven subjects on `EXECUTION`:
`strategy.signal.received`, `order.order.submit`, `.accepted`, `.routed`, `.filled`,
`.outcome` (and `.rejected` if you hit the MIC mismatch above first).

`order.order.filled` carries an `order.v1.OrderFilled` (the message body is a
marshalled `envelope.v1.EventFrame` — envelope plus payload bytes, not a bare
payload). Cross-check the order at
<https://testnet.binance.vision> — the point of this demo is that both agree.

> The `positions` table will **not** advance: that projection is `risk-engine`'s
> job and it is off this deployment. `positions` and `tv_facts` are also
> tenant-scoped under `FORCE ROW LEVEL SECURITY` — `SET app.tenant_id = '<fund_id>'`
> before any SELECT or the query errors `tenant scope missing`. Note that orders
> land under `OMS_TENANT` (`__system__`), not the envelope's fund, because the
> store pool pins one tenant.

## 11. Optional — let TradingView reach it

Only needed for real TradingView alerts. TradingView will not POST to a bare IP or
to plain HTTP.

```bash
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/main/deploy/static/provider/cloud/deploy.yaml
# point signals.eighred.com (or your host) at the VDS's public IP, then:
kubectl apply -f kanz/infra/deploy/webhook-ingest-ingress.yaml
```

The Ingress declares `host: signals.eighred.com` — edit it to your DNS name.
Terminate TLS with cert-manager or a manually-provisioned certificate; the
HMAC signature authenticates the *payload*, not the transport, so TLS is not
optional once this is reachable from the internet.

`WEBHOOK_INGEST_IP_ALLOWLIST` is empty, which means **any source IP passes**.
Before exposing this publicly, set it to TradingView's published egress ranges.

---

## 12. Teardown / re-run

- After a host reboot: **re-run the NATS bootstrap Job first** (§4), then
  `kubectl -n kanz-services rollout restart deploy` for the crash-looped services.
- Rebuilding one service: `docker build -f kanz/services/<svc>/Dockerfile -t ghcr.io/kanz-eng/<svc>:latest .`
  from the **repo root** (the build context needs `kanz-schemas/gen/go` beside
  `kanz/`), then re-import and `kubectl rollout restart`. `tools/rig-images.sh --build`
  rebuilds all 23 and is rarely what you want mid-loop.
- The halt gate closes again on a fresh `PLATFORM` stream — re-run §9 after any
  bootstrap re-run.

## 13. What must change before this is production

Not a wishlist — each is a specific deviation this file introduced:

1. **Vault** replaces every hand-created Secret; `rig_dev_patch.py` disappears from the path.
2. **NATS mTLS**, so the SPIFFE sockets stripped in §4 are real and the bus is authenticated.
3. **OIDC** at the gateway, replacing the HS256 dev key.
4. **`risk-engine`** deployed (needs the Argo Rollouts controller) so `positions` projects.
5. **`BINANCE_BASE_URL`** flipped to the live exchange — a deliberate, reviewed edit, never a default.
6. **A second replica story for `venue-binance`.** It is pinned to `replicas: 1` by an
   exchange constraint, not a code one: the REST weight budget (1200) is enforced per
   process but Binance rate-limits per **API key**. Two pods spend 2400 against one
   key's 1200 and get the account rate-limited or banned. Scaling means sharding by
   symbol with a key per shard — never just raising the replica count.
7. **Persistent storage.** `postgres-dev.yaml` is an `emptyDir` — order state does
   not survive a pod restart. Production is CNPG with real volumes.
8. **`WEBHOOK_INGEST_IP_ALLOWLIST`** populated.
9. **CI green.** `release.yml` has never run; trivy/cosign/SBOM are unproven, so the
   images deployed here are unsigned and unscanned.
