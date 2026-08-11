# Infra — cluster bring-up notes

This directory holds the platform's per-component manifests (`nats/`,
`security/`, `tenancy/`, `deploy/`, ...). It does not own cluster
provisioning: production clusters come from `terraform/` (EKS), and the
local dev loop comes from `kanz/dev/` (docker compose). The notes below are
for a single-node **k3s** cluster stood up by hand — used when standing up
the full manifest set (SPIRE + NATS + the trading loop) outside both of
those paths.

## Driving `kanz` against a local gateway (no SSO)

`kanz` authenticates through the Eighred SSO device flow. Until that service
ships, `/login` against a local stack returns

```
status 404 on http://localhost:8080/.well-known/openid-configuration
```

which is correct rather than broken: the load stack's gateway validates **HS256
JWTs** (`API_GATEWAY_JWT_SECRET`, together with the explicit
`API_GATEWAY_ALLOW_DEV_HS256=true` that arm now requires) and is not an OpenID
provider, so there is no discovery document to fetch.

Set `KANZ_TOKEN` to a pre-minted bearer instead, and leave `KANZ_IDENTITY_URL`
unset:

```sh
export KANZ_GATEWAY_URL=http://localhost:8080
export KANZ_TOKEN="$(go run ./cmd/kanz-devtoken --secret load-secret --tenant load-test --role kanz-user)"
unset KANZ_IDENTITY_URL
kanz
```

**Setting both is refused**, deliberately, rather than resolved by precedence. A
rule like "signing in wins" would let a `KANZ_TOKEN` exported for one experiment
sit forgotten in a shell profile and then silently take over the day the
identity URL is briefly unset. The refusal is loud and impossible to be on the
wrong side of by accident.

`KANZ_SSO_ISSUER` is **retired** (#364). It named an external Eighred SSO device
flow that was never built; the CLI now signs in against the platform's own
identity provider through `KANZ_IDENTITY_URL`. It is refused by name rather than
ignored, so a shell profile that still exports it says so instead of leaving you
to wonder why it has no effect.

The session is marked in the header every run (`using KANZ_TOKEN from the
environment`) and by `/whoami`, because the failure mode of a static bearer is
forgetting you are on one. It is **never written to the token store**: unset the
variable and the session is gone.

`/login` is unavailable in that mode — there is no issuer to sign in against —
and `/logout` says where the credential actually lives rather than pretending to
clear it.

## Building any image requires `docker login ghcr.io` first

Every Dockerfile's base now comes from `ghcr.io/eighred/base/*` (#161), and
that namespace is **private** — company policy forbids publishing internal
packages or mirrors. So a base pull needs a credential, and a build without
one fails on its very first layer:

```
failed to solve: ghcr.io/eighred/base/golang:1.26.5: unexpected status: 403 Forbidden
```

That message names the registry and not the reason, so it reads like a broken
Dockerfile rather than a missing login. Before `make dev-up`, the load stack,
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

`tools/rig_dev_patch.py` strips `SPIFFE_ENDPOINT_SOCKET` by default, which is
correct only against a plaintext broker. `kanz/infra/nats/nats.yaml` serves
`tls { verify: true, verify_and_map: true }` — mTLS-only — so against it
every `rig_dev_patch.py` invocation needs `--keep-spiffe`, or the socket is
stripped, the client still meets a TLS server, and every workload dies with:

```
nats connect: nats: tls error: x509: certificate signed by unknown authority
```

— an error that reads like a missing CA and is actually a missing env var.
