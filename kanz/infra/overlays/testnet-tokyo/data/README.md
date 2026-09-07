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
