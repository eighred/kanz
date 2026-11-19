# Infrastructure and operational runbooks

This directory holds the platform's per-component manifests (`nats/`,
`security/`, `tenancy/`, `deploy/`, ...). It does not own cluster
provisioning: production clusters come from `terraform/` (EKS), and the
local dev loop comes from `kanz/dev/` (Docker Compose). Start at the component
README before applying manifests. These commands require an explicitly selected
non-production environment and operator authority. The notes below are
for a single-node **k3s** cluster stood up by hand — used when standing up
the full manifest set (SPIRE + NATS + the trading loop) outside both of
those paths.

## Accounting database topology

Provision a dedicated accounting database with the migration role as schema
owner. An existing shared database using named migration sets is compatible
when the accounting target schema is the only schema containing a
`ledger_entries` table. Keep accounting schema changes under the migration
runner's advisory lock.

On each invocation, `kanz-migrate` executes the read-only preflight bundled
with accounting migration 0003 before recording or applying any migration.
The historical SQL enumerates policies by table name across the database.
A second ledger table in another schema causes the preflight to refuse with
SQLSTATE `55000`. Check the selected database and migration role's
`search_path` before retrying. Keep both schemas and their policy histories
intact while resolving the database assignment.

## Adding a node during single-server k3s maintenance

Schedule node additions outside maintenance on the sole control-plane node.
The provisioning Job runs on the control plane because it mounts the cluster
join token and the host's SSH bootstrap key.

When the operator sees the control-plane node is cordoned, it rejects an
authorized `POST /v1/control/nodes` before creating the provisioning Job or
its Secret. The gateway returns HTTP 409, names the node, and explains how to
uncordon it. If the operator itself is offline during a server upgrade, node
provisioning is unavailable for that window. After maintenance, confirm the
operator is available and the node is Ready and schedulable, then retry.

## Driving a local gateway with a pre-minted bearer

The browser is the operator interface. Independent administrative tools and
load-test fixtures still use gateway bearers. Run the following local-fixture
example from `kanz/`; never use its development signing configuration for a
production gateway.

The load stack's gateway validates **HS256 JWTs** (`API_GATEWAY_JWT_SECRET`,
together with the explicit `API_GATEWAY_ALLOW_DEV_HS256=true` that arm now
requires) and is not an OpenID provider, so it serves no discovery document:

```sh
export KANZ_GATEWAY_URL=http://localhost:8080
export KANZ_TOKEN="$(go run ./cmd/kanz_devtoken --secret load-secret --tenant load-test --role kanz-user)"
```

For the WEB app, sign-in is a credential exchange against the identity service
through the BFF (`POST /auth/login`) — the browser never holds a token at all,
so none of the variables above apply to it. See #371.

## Building any image requires `docker login ghcr.io` first

Every Dockerfile's base now comes from `ghcr.io/eighred/base/*` (#161), and
that namespace is **private** — company policy forbids publishing internal
packages or mirrors. So a base pull needs a credential, and a build without
one fails on its very first layer:

```
failed to solve: ghcr.io/eighred/base/golang:1.26.5: unexpected status: 403 Forbidden
```

That message names the registry and not the reason, so it reads like a broken
Dockerfile rather than a missing login. Before `make up` from `kanz/`, the load stack,
or any `docker build` in this repository:

```sh
echo "$GHCR_PAT" | docker login ghcr.io -u <your-github-username> --password-stdin
```

The PAT needs `read:packages` only. CI does this for itself — `build.yml`,
`latency.yml`, `preview.yml` and `release.yml` each authenticate before
building, and `test/arch/supplychain_test.go` fails the build if one of them
stops doing so.

**A pull_request from a fork cannot build images at all.** Its `GITHUB_TOKEN`
carries no read access to this organisation's packages. `build.yml` fails such
a PR in its first step with a message saying so. Push the branch to
`eighred/kanz` and open the PR from there — which is the workflow anyway.

## k3s install: `--disable traefik` is required, not taste

`kanz/infra/deploy/webhook-ingest-ingress.yaml` declares `ingressClassName:
nginx`. k3s's bundled Traefik would claim ports 80/443 and then ignore that
Ingress, which presents as "the webhook URL 404s" with no error anywhere.

```sh
curl -sfL https://get.k3s.io | sh -s - --disable traefik --write-kubeconfig-mode 644
```

Install `ingress-nginx` separately if the Ingress path is needed.

## `rig_dev_patch.py` needs `--keep-spiffe` against an mTLS bus

`tools/rig-apply.sh --deploy` uses the simulator's plaintext bus by default.
With the SPIRE-authenticated broker, use `tools/rig-apply.sh --deploy --keep-spiffe`.
Both modes require the gateway's cluster-local OIDC identity Service for JWKS
and revocations. The five-workload rig omits the optional operator control
routes because it does not provision that upstream. Run
`tools/rig-apply.sh --check-deploy` with the same cluster and bus options
to check the prerequisites before deployment. To inspect the exact gateway
manifest offline, use `tools/rig-apply.sh --render-workload api-gateway-deploy.yaml`.

`tools/rig_dev_patch.py` strips `SPIFFE_ENDPOINT_SOCKET` by default, which is
correct only against a plaintext broker. `kanz/infra/nats/nats.yaml` serves
`tls { verify: true, verify_and_map: true }` — mTLS-only — so against it
every `rig_dev_patch.py` invocation needs `--keep-spiffe`, or the socket is
stripped, the client still meets a TLS server, and every workload dies with:

```
nats connect: nats: tls error: x509: certificate signed by unknown authority
```

— an error that reads like a missing CA and is actually a missing env var.
