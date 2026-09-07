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
| **Bound workload auth to Vault** | Vault OSS does not provide native SPIFFE auth. The CSI provider requests an audience-bound Kubernetes ServiceAccount token for the consuming pod; Vault binds its role to the exact namespace and ServiceAccount. SPIFFE remains the mTLS identity boundary. |
| **Cloud KMS auto-unseal** | A dedicated, rotation-enabled KMS key wraps Vault's barrier key. The EC2 role has only `Encrypt`, `Decrypt`, and `DescribeKey`; the node keeps IMDSv2 hop limit 1 and only the host-networked Vault pod can obtain that role. |

## What plaintext config was removed

Runtime secrets include Postgres/Redis credentials, venue testnet API
credentials, identity signing material, webhook configuration, and any enabled
model-provider key. Peer authentication and NATS/Kafka transport use rotating
SVIDs instead of passwords.

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
| `spire-upstream.yaml` | Undeployed SPIRE HA overlay; its upstream CA design remains blocked pending a cloud-identity redesign for this non-EKS node |
| `../../../../tools/bootstrap-testnet-venues.sh` | Interactive, stdin-only Vault bootstrap and venue-credential rotation payload; contains no credential values |

## Deploy

```sh
kubectl apply -f crds/             # vendored Secrets Store CSI v1.6.0 CRDs
kubectl apply -f csi.yaml          # driver + Vault provider (once per cluster)
kubectl apply -f vault.yaml        # Vault starts sealed and uninitialized
```

Initialize exactly once from an interactive operator session. `operator init`
prints five recovery shares and the initial root token in plaintext. It does
**not** store them in KMS. Distribute at least three shares to separate approved
custodians and put the initial root token in the approved password manager only
for the bootstrap window. Never paste any of these values into a ticket, chat,
shell command line, repository file, or automation log.

```sh
kubectl -n vault exec -it vault-0 -c vault -- sh
export VAULT_ADDR=https://127.0.0.1:8200
export VAULT_TLS_SERVER_NAME=vault.vault.svc
export VAULT_CACERT=/run/spire/certs/bundle.crt
umask 077
vault operator init -recovery-shares=5 -recovery-threshold=3
read -rsp 'Initial root token: ' VAULT_TOKEN; export VAULT_TOKEN; echo

vault audit enable file file_path=/vault/data/audit.log mode=0600
vault secrets enable -path=kv kv-v2

# Kubernetes auth bound to one namespace + ServiceAccount per workload.
vault auth enable kubernetes
vault write auth/kubernetes/config \
  kubernetes_host="https://kubernetes.default.svc:443"
vault write auth/kubernetes/role/risk-engine \
  bound_service_account_names=risk-engine \
  bound_service_account_namespaces=kanz-services \
  audience=vault token_policies=risk-engine-read token_ttl=5m

kubectl apply -f secretproviderclass.yaml
```

Create the least-privilege policies before their roles and revoke the initial
root token after a tested break-glass operator policy exists. A role must read
only its own `kv/data/kanz/<service>` and `kv/metadata/kanz/<service>` paths.
The `vault` ServiceAccount has only Kubernetes TokenReview delegation; it has no
permission to read Kubernetes Secrets.

Seed a secret:

```sh
read -rsp 'DSN: ' dsn; echo
printf '%s' "$dsn" | vault kv put kv/kanz/risk-engine dsn=-
dsn=''
```

### Tokyo venue testnet bootstrap and rotation

From Windows, use the repository-owned operator entry point. It resolves the
current node from Terraform state, stages the secret-free shell payload through
SSM, opens an interactive session, and verifies a completion marker afterward:

```powershell
.\tools\Kanz-Venue-Secrets.cmd
.\tools\Invoke-VenueSecrets.ps1 -Mode Bootstrap
.\tools\Invoke-VenueSecrets.ps1 -Mode Rotate
```

The `.cmd` launcher resolves PowerShell 7 from `PATH`, the Codex bundled runtime,
or Windows PowerShell, in that order. It invokes `Invoke-VenueSecrets.ps1` from
the launcher's own repository directory, so a desktop shortcut does not embed a
second mutable copy of the operator workflow.

Both modes prompt with hidden input. Credential values travel only on the
interactive session's stdin and Vault CLI stdin; they are absent from process
arguments, SSM command documents, Terraform state, Kubernetes objects and local
files. `Bootstrap` also configures audit, KV v2 and the two exact Kubernetes
roles. `Rotate` only creates new KV versions. Until a durable human operator
authentication method is commissioned, the initial root token remains an
offline break-glass credential and must never be pasted into chat or a shell
command.

## Secret-rotation runbook

Rotation is **roll-forward, zero-downtime** — write the new value to Vault, then
let pods pick it up. There is never a window where a human holds plaintext.

### Application DB credential (`kv/kanz/<svc>`)

1. Create the new Postgres role/password (or `ALTER ROLE … PASSWORD`).
2. Read the replacement with hidden input and pipe it to
   `vault kv put kv/kanz/risk-engine dsn=-`; never put it in argv. Vault keeps
   the prior version, so a rollback is `vault kv rollback`.
3. Recycle consumers: `kubectl -n kanz-services rollout restart deploy/risk-engine`.
   Each new pod re-mounts the CSI volume and reads the new DSN at boot. (Enable
   the provider `--rotation-poll-interval` to refresh the file in place without
   a restart; the service must re-read the file — restart is the simple path.)
4. After all pods are on the new credential, drop the old Postgres role.

### Vault unseal / KMS key

KMS-managed: automatic annual rotation is enabled on
`alias/kanz-vault-unseal`. KMS retains prior key material for decrypt, so a
restart can unwrap existing Vault state. Do not replace or schedule deletion of
the key as an ordinary rotation action.

### SPIRE upstream root

The deployed testnet still uses SPIRE's existing local root. Do not apply
`spire-upstream.yaml`: its AWS Secrets Manager upstream requires a dedicated
workload-cloud-identity boundary that this non-EKS node does not yet provide.
Track and review that migration independently from Vault auto-unseal.

### Workload identity (SVID)

Never rotated by hand — SPIRE renews leaf SVIDs on a 1h TTL and JWT-SVIDs on 5m
(SEC-01a). A compromised credential self-expires; force-evict by deleting the
SPIRE registration entry, which the controller-manager recreates clean.

## Follow-ups

- Vault HA: bump the StatefulSet to ≥3 Raft peers (storage is already Raft).
- Dynamic DB creds: move `kv/kanz/<svc>` to the Vault `database` secrets engine
  so DSNs are short-lived and per-pod, eliminating the manual rotation above.
