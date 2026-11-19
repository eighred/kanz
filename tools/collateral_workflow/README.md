# Collateral workflow release qualification

The [workflow](../../.github/workflows/collateral-workflow.yml) verifies the Accounting and migration images from Kanz v2026.4.0 under the release workflow's exact tag identity. Both signatures and both signed predicate types are required. The private registry login exists only during acquisition on the hosted runner.

The Linux/amd64 inspection uses the actual image filesystem and reads the shipped program's Go build information. Each image has one native executable. The check rejects JVM archives and class files. The migrator must contain all 20 Accounting SQL migrations with the same filenames and bytes as [release commit f7217a76](https://github.com/eighred/kanz/commit/f7217a76b48d9b6075a0683ddf35be50fbdf37cb).

The rehearsal starts the signed migrator and Accounting service on a disposable hosted runner. PostgreSQL uses distinct migration and nonprivileged runtime roles. JetStream uses the deployed Accounting subject permissions and separate synthetic feed and custody principals. The gateway authenticates two short-lived development identities. The maker proposes a USD 60 instruction and the checker approves it. The rehearsal replaces the worker and restarts both backing servers before a custody confirmation arrives. Repeated source deliveries and approvals must preserve the instruction, settlement and audit history. The runner removes the leased containers, networks and volumes after the check.

[Issue #1544](https://github.com/eighred/kanz/issues/1544) also requires an upgrade and rollback between two release-qualified versions. This rehearsal exercises v2026.4.0.
