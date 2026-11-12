# Tokyo testnet data plane

This overlay is the smallest topology that can exercise the capital path on the
existing 8 GiB single node without presenting replica counts as availability.
It deliberately reports HA as `UNSUPPORTED`:

- one CloudNativePG primary with one logical database and two roles per active
  migration-owning service;
- one file-backed JetStream server using the canonical tenancy and stream
  bootstrap contracts with replica factor one and fsync-before-ack;
- one AOF-backed Redis server with fsync on every write for cross-pod
  idempotency and nonce protection.

Application roles are `NOSUPERUSER` and `NOBYPASSRLS`. Migration roles own their
database and receive DDL rights; application roles receive only runtime object
privileges through default privileges. Passwords and application/migration DSNs
originate in Vault. The only Kubernetes database credential is the random
operator-owned bootstrap secret CloudNativePG requires for initial database
creation; it is mounted as a file only by the bounded provisioner Job and is not
used by any service.

The resource request envelope is 500m/1536Mi for Postgres, 250m/512Mi for NATS,
and 50m/64Mi for Redis, plus bounded sidecars and one-shot jobs. Storage claims
are 20 GiB data + 4 GiB WAL, 4 GiB JetStream, and 1 GiB Redis.
All data-plane images changed by this overlay are immutable digest references.

Deployment order is strict:

1. Install and verify the pinned CloudNativePG controller.
2. Verify Vault paths and Kubernetes auth roles without reading values.
3. Apply this overlay and wait for Postgres, NATS, Redis, and both bootstrap Jobs.
4. Verify all application roles are non-superuser/non-bypass and run migrations.
5. Apply the capital-path workload overlay in dependency order.
6. Run backup/restore and reconciliation certification before any testnet order.

The checked-in recovery command is
tools/Invoke-TestnetDataPlaneRecovery.ps1 -Operation Backup|Restore. It refuses
a backup while application or venue writers are running, streams database,
Redis and NATS artifacts without exposing mounted credentials to the host, and
uploads only through the EC2 instance role to the KMS-encrypted, Object-Locked
Osaka bucket. Restore creates a temporary, network-denied replacement namespace,
checks every database's migration/table/RLS state plus Redis and NATS totals,
reports measured RPO/RTO, and deletes the drill namespace. The wrapper refuses
to execute unless the shell script is the exact blob on origin/main.

The single node, each singleton store, and the local-path volumes are failure
domains. Loss of any one is an outage, so readiness must never be reported as HA.

### Isolated tv-sync restored-state qualification

The ordinary recovery command retains its eight-database inventory. Explicit
`-QualifyTVSync` on backup includes `tv_sync`; on restore it requires that exact
nine-database archive and runs `/tv-sync-replay` against the disposable restored
CNPG cluster. This is source preparation, not permission to invoke the drill.
The reviewed signed release pair must contain the exact-state verifier; the
currently locked v2026.4.0 pair does not, so this mode refuses before SSM until a
new reviewed signed pair and destination custody are established.

The verifier runs as a new SELECT-only, NOSUPERUSER/NOBYPASSRLS tenant role.
Only the init container mounts the disposable owner credential. Both connections
verify the restored PostgreSQL certificate; role provisioning checks isolated
server logging controls before submitting a generated password. No owner secret,
SQL error, init log or record contents are exported. Exact main source, release
root and linux/amd64 child digest, loaded image, archive hash, restored cluster
UID, tenant hash, retention and snapshot hashes are bound in the result. The TV mode requires jq with `have_decnum` support and exact integer
counters; older parsers refuse before mutation. A fresh
nonempty checkpoint/history proof is required; genesis is not qualified.

Cleanup uses namespace UID preconditions and requires local-path Delete reclaim
policy, PVC/PV identity consistency and namespace/PV-object absence. This proves
controller-object cleanup, not physical media erasure. Failed cleanup prevents a
successful receipt. No foreign namespace, PV or finalizer is forcibly removed.
The result qualifies only that isolated restored archive; it is not a current
production database receipt, current runtime credential proof, historical-prefix
completeness proof or activation gate. Existing preparation Job receipts cannot
substitute for this comparison. Live execution, pinned-CNPG role authority,
loaded-image readback and restore cleanup still require operational acceptance.

### CloudNativePG metrics access (#1142)

`postgres-prometheus-metrics` permits only pods labelled `app=prometheus` in
`kanz-observability` to reach this cluster's TCP 9187 exporter. Both selectors
share one NetworkPolicy peer; placing them in separate peers broadens access.
The policy grants no database-port access and changes no Cluster settings.

After a reviewed policy-only rollout, verify the discovered Pod IP, named
`metrics` port and configured HTTP/TLS scheme against the actual instance. An
entry in `/proc/net/tcp6` alone does not establish IPv6-only operation. Confirm
this target stays up for at least three 30-second scrape intervals, and record
Cluster Ready and continuous-archiving status before and after. Do not collect
metrics bodies or credential-bearing resources in the receipt.

Rollback removes only the named `postgres-prometheus-metrics` policy. It restores
the prior metrics denial while retaining database default deny and the existing
application/controller allow rules; it requires no primary restart. Structural
CI guards are not evidence of CNI enforcement or live database health.
