# Valkey first installation and retained replay-state migration

The generic rig starts Valkey 9.1.3 at the pinned image digest in
`kanz/infra/messaging/redis.yaml`. The service DNS name, protocol, secret
paths and PVC identity remain compatible with its gateway and webhook
consumers. Each acknowledged AOF write is fsynced. The Tokyo data overlay
retains its existing Redis image and command.

Before applying dev Secrets or network policy, `tools/rig-apply.sh --deploy`
checks the cache StatefulSet and its ordinal-zero PVC. A first install creates
both by name and records the PVC's Kubernetes UID on the StatefulSet. A reapply
requires that original claim and the same pinned Valkey image. An existing
Redis deployment, unmarked Valkey installation, orphaned or missing PVC, or
failed API read stops deployment. A partial install can leave a reserved PVC.
Inspect its ownership and retained data before any recovery decision (#1541).

The first-install fixture renders the same dev-secret adaptation as
`tools/rig-apply.sh`. It starts that workload command on a new private
volume, checks its effective persistence configuration, and executes both
authenticated probes with an ephemeral password mounted in memory. Missing,
empty and incorrect passwords must each produce a nonzero probe exit status.
Restoring the correct secret must restore both authenticated checks. It requires an empty keyspace across databases, then claims
independent tenant keys with different absolute expiration times. After a clean
Valkey restart, the surviving claim must reject a duplicate without extending
its deadline. The short-lived claim must become reusable at its original
deadline. The fixture removes the volume before the retained-state regression.
This exercises the backend operations on synthetic keys. The two-process
risk-engine consumer qualification is tracked in #111.

The generic rig applies `kanz/infra/messaging/valkey-clients.yaml` before the
cache. On a CNI that enforces NetworkPolicy, only the gateway and webhook
app labels in `kanz-services` can reach its TCP port 6379. The
`rig-cache-ingress` CI job starts a fresh kind/Cilium cluster, deploys the
rendered cache and checks both clients through disposable pods carrying the
rendered workload labels, service accounts and DSN file mounts. It tests an
unapproved peer and a gateway-labelled peer in a different namespace before
and after applying the policy. The pods authenticate with a disposable
password substituted for the rig value. Both client Secrets still map to the
same cache credential. NetworkPolicy filters pods by their labels and
namespace; it does not enforce independent Valkey ACL users.

The retained-state fixture starts the Tokyo Redis image on a second fresh
volume and checks Redis → Valkey → restarted Valkey → Redis using that same
volume. Both fixtures use the Valkey image selected by application compatibility
CI. They accept no endpoint, credentials, volume name or deployment target.
Generated Docker containers have no network, host ports, host mounts or privileges.

Redis holds security-sensitive short-lived replay claims, not merely optional
cached data. Clearing that state reopens replay admission. The fixture compares
exact values and absolute expiration timestamps across every phase, checks tenant
key separation and rejected duplicate claims, then proves Valkey-written claims
and counter changes survive rollback. It also checks authentication, fsync-always,
no eviction, loaded image identity, clean shutdown and verified resource removal.
The receipt records each container ID and server run ID, the loaded image and
version, and the unique generation label on each owned volume. All six server
lifetimes must differ. The first-install and retained-state runs each keep
their own volume across restarts.

The initial Redis 7.4.5 AOF includes its normal RDB preamble. An actual attempt
to load it in Valkey 9.1.3 failed with `unsupported_rdb_version`
([run 42130099700](https://github.com/eighred/kanz/actions/runs/42130099700));
cleanup was verified. Generic Redis protocol compatibility does not imply this
on-disk format is compatible. Redis must first rewrite its AOF with
`aof-use-rdb-preamble no` while it still owns the data, with exact state/expiry
verified afterward. Valkey then loads that command-format AOF and explicitly
rewrites it with the same setting before rollback.
The rollback reads the current volume, including Valkey writes; it does not
restore an old snapshot and silently lose newer claims. This proves only the
string/counter operations used in this fixture, not arbitrary Redis modules or
new Valkey data types. The [Valkey migration guide](https://github.com/valkey-io/valkey-doc/blob/main/topics/migration.md)
and [persistence documentation](https://valkey.io/topics/persistence/) describe
forward loading and AOF behavior; actual pinned-runtime evidence is required for
this rollback path.

Run `python3 -B -m unittest discover -s tools/valkey_migration -p 'test_*.py'`
for offline refusal tests. The workflow runs `proof.py` on an isolated Linux CI
runner with Docker and records the checked-out commit. A missing runtime, failed
migration, changed expiry or unknown cleanup is a failure, never a skip.

First-install acceptance and the caller inventory are tracked in #1541. If an
installation with retained Redis state is migrated later, inventory actual key
types, quiesce writers, verify a recoverable backup, preserve the PVC, service
and authentication identities, and qualify the rendered workload and recovery
scripts before cutover.
