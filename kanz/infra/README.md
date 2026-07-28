# Infra — cluster bring-up notes

This directory holds the platform's per-component manifests (`nats/`,
`security/`, `tenancy/`, `deploy/`, ...). It does not own cluster
provisioning: production clusters come from `terraform/` (EKS), and the
local dev loop comes from `kanz/dev/` (docker compose). The notes below are
for a single-node **k3s** cluster stood up by hand — used when standing up
the full manifest set (SPIRE + NATS + the trading loop) outside both of
those paths.

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
