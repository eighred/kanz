# Secrets — Vault + CSI mounts, KMS-rooted (SEC-01d)

Closes the secrets gap in the Security epic: every runtime secret comes from
**HashiCorp Vault**, delivered to pods as **tmpfs files via the Secrets Store
CSI Driver**, with **cloud KMS** as the root of trust. No plaintext secret
lives in a manifest, a pod env var, or an etcd `Secret`.

## Why this shape

| Decision | Reason |
|---|---|
| **Vault**, not k8s `Secret`s | k8s Secrets are base64 in etcd (plaintext at rest without KMS etcd-encryption) and readable by anyone with `get secret`. Vault gives versioning, audit, dynamic creds, and one rotation surface. |
| **CSI file mount**, not env vars | env vars leak into `/proc/<pid>/environ`, crash dumps, and child processes. A tmpfs CSI mount is pod-scoped, never persisted, torn down with the pod. |
| **SPIFFE/JWT auth to Vault** | Workloads authenticate with their SPIFFE JWT-SVID (SEC-01a's `default_jwt_svid_ttl=5m`) against Vault's `jwt` backend, validated via the SPIRE OIDC discovery endpoint. Secret access is gated on the **same attested identity** as mTLS transport (SEC-01b) — no second static credential to manage or leak. |
| **Cloud KMS root** | One KMS key both auto-unseals Vault and backs the SPIRE `UpstreamAuthority`. Neither the Vault unseal key nor the SPIRE root CA key ever exists as recoverable plaintext. |

## What plaintext config was removed

The only genuine runtime secret in the platform is the **Postgres DSN** (risk
state PERS-01, schema registry EVT-16a, and the HA SPIRE datastore). Everything
else — peer auth, NATS/Kafka transport — is mTLS, where the SVID *is* the
credential, so there is no password to store.

- `risk-engine` / `schema-registry`: `config.secret()` now reads the DSN from
  `<VAR>_FILE` (the CSI mount) in preference to the plaintext `<VAR>` env var.
- `spire-server`: the plaintext `keys.json` disk root key and the sqlite
  `connection_string` are replaced by the KMS `UpstreamAuthority` + a
  CSI-mounted postgres DSN (`spire-upstream.yaml`).

## Files

| File | Purpose |
|---|---|
| `vault.yaml` | Vault StatefulSet — KMS auto-unseal, Raft storage, SVID-fronted TLS listener |
| `csi.yaml` | Secrets Store CSI Driver + `vault-csi-provider` DaemonSet + `CSIDriver` |
| `secretproviderclass.yaml` | DSN `SecretProviderClass` per consumer + the consumer pod patch |
| `spire-upstream.yaml` | SPIRE server KMS-`UpstreamAuthority` + postgres-DataStore overlay (SEC-01a follow-up) |

## Deploy

```sh
kubectl apply -f csi.yaml          # driver + Vault provider (once per cluster)
kubectl apply -f vault.yaml        # Vault (unseals itself via KMS)
# one-time Vault bootstrap (init writes recovery keys to the KMS-wrapped output):
vault operator init -recovery-shares=1 -recovery-threshold=1
vault secrets enable -path=kv kv-v2

# JWT auth bound to SPIRE identities (step 4):
vault auth enable -path=jwt-spiffe jwt
vault write auth/jwt-spiffe/config \
  oidc_discovery_url="https://spire-oidc.spire-system.svc" \
  default_role="deny"
vault write auth/jwt-spiffe/role/risk-engine \
  role_type=jwt user_claim=sub bound_audiences=vault \
  bound_subject="spiffe://kanz.internal/ns/kanz-services/sa/risk-engine" \
  token_policies=risk-engine-read token_ttl=5m

kubectl apply -f secretproviderclass.yaml
```

Seed a secret:

```sh
vault kv put kv/kanz/risk-engine dsn="postgres://risk:…@pg.kanz-data.svc/risk?sslmode=verify-full"
```

## Secret-rotation runbook

Rotation is **roll-forward, zero-downtime** — write the new value to Vault, then
let pods pick it up. There is never a window where a human holds plaintext.

### Application DB credential (`kv/kanz/<svc>`)

1. Create the new Postgres role/password (or `ALTER ROLE … PASSWORD`).
2. `vault kv put kv/kanz/risk-engine dsn="postgres://…<new>…"` — Vault keeps the
   prior version, so a rollback is `vault kv rollback`.
3. Recycle consumers: `kubectl -n kanz-services rollout restart deploy/risk-engine`.
   Each new pod re-mounts the CSI volume and reads the new DSN at boot. (Enable
   the provider `--rotation-poll-interval` to refresh the file in place without
   a restart; the service must re-read the file — restart is the simple path.)
4. After all pods are on the new credential, drop the old Postgres role.

### Vault unseal / KMS key

KMS-managed: rotate the underlying `alias/kanz-vault-unseal` key in the cloud
console; KMS keeps old versions for decrypt, so running Vault is unaffected and
the next restart unseals against the new version. No Vault action required.

### SPIRE upstream root (KMS-backed CA)

Rotate the `kanz/spire/upstream-ca-*` material in KMS/Secrets Manager, then
`kubectl -n spire-system rollout restart statefulset/spire-server`. Leaf SVIDs
keep rotating on their 1h TTL underneath; the SEC-01b go-spiffe clients track
the new bundle automatically (no client action).

### Workload identity (SVID)

Never rotated by hand — SPIRE renews leaf SVIDs on a 1h TTL and JWT-SVIDs on 5m
(SEC-01a). A compromised credential self-expires; force-evict by deleting the
SPIRE registration entry, which the controller-manager recreates clean.

## Follow-ups

- Vault HA: bump the StatefulSet to ≥3 Raft peers (storage is already Raft).
- Dynamic DB creds: move `kv/kanz/<svc>` to the Vault `database` secrets engine
  so DSNs are short-lived and per-pod, eliminating the manual rotation above.
