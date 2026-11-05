# Retained replay-state migration

This disposable CI fixture qualifies the existing Tokyo Redis image to the same
Valkey 9.1.3 image used by application compatibility CI. It accepts no endpoint,
credentials, volume name or deployment target. Generated Docker containers have
no network, host ports, host mounts or privileges; only one labelled private
volume survives the Redis → Valkey → restarted Valkey → Redis sequence.

Redis holds security-sensitive short-lived replay claims, not merely optional
cached data. Clearing that state reopens replay admission. The fixture compares
exact values and absolute expiration timestamps across every phase, checks tenant
key separation and rejected duplicate claims, then proves Valkey-written claims
and counter changes survive rollback. It also checks authentication, fsync-always,
no eviction, loaded image identity, clean shutdown and verified resource removal.
Only fixed verdicts and image/version identities enter the evidence artifact.

The initial Redis AOF includes its normal RDB preamble. Valkey loads that data,
then explicitly rewrites its AOF with `aof-use-rdb-preamble no` before rollback.
The rollback reads the current volume, including Valkey writes; it does not
restore an old snapshot and silently lose newer claims. This proves only the
string/counter operations used in this fixture, not arbitrary Redis modules or
new Valkey data types. The [Valkey migration guide](https://github.com/valkey-io/valkey-doc/blob/main/topics/migration.md)
and [persistence documentation](https://valkey.io/topics/persistence/) describe
forward loading and AOF behavior; actual pinned-runtime evidence is required for
this rollback path.

Run `python3 -B -m unittest discover -s tools/valkey-migration -p 'test_*.py'`
for offline refusal tests. The workflow runs `proof.py` on an isolated Linux CI
runner with Docker and records the checked-out commit. A missing runtime, failed
migration, changed expiry or unknown cleanup is a failure, never a skip.

No active Kubernetes manifest, recovery image or client configuration changes.
Before a live migration, inventory actual key types, quiesce every writer, take
and verify a current recoverable backup, preserve the existing PVC/service/auth
identity, and qualify the rendered workload plus recovery scripts. Do not start
old Redis on a Valkey RDB snapshot or treat generic protocol compatibility as
retained-state or rollback proof. Migration ownership remains tracked in #1413.
