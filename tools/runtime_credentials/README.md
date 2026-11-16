# Runtime and migration credential preparation (#1747)

This component prepares **accounting, audit and identity**. It does not activate
the transaction, certify the deployed estate, close #1747 or satisfy #1331's full
exemption acceptance. The canonical base/Tokyo Deployments, SecretProviderClasses,
image digests and GitOps overlays remain unchanged. Runtime startup source gains
an explicit catalog check before starting effects; migration pools retain their
separate role. The check is startup evidence, not continuous administrator-ACL
monitoring or a certification of every PostgreSQL capability outside the selected
application schema.

## Boundary and remaining consumers

One Vault KV v2 read returns the whole record, including historical versions.
The old `kanz/<service>` record contains both application and migration DSNs;
`identity-signing-key` also reads that same identity record. A CSI field projection
cannot confine an SA that is entitled to the migration field in the same record.

The shared canonical SPC inventory has 13 non-TV mixed-record classes: the three
selected here; Tokyo's OMS, regulatory, risk-engine, venue-binance and venue-okx;
and base-only schema-registry, wealth, datamaster, alternatives and market-data.
Legacy tv-sync is the fourteenth class; its qualified Tokyo runtime-only seam
remains the implementation to reconcile with the legacy base path. These eleven
remaining consumers stay open under #1747. Venue credential/policy writers and
capital deployment conversions are not silently activated by this phase.

| Identity | Record after authorized activation | Resource/authority |
|---|---|---|
| Existing `<service>` SA / Vault role | `kanz/<service>-runtime` | Existing app DSN; identity also retains the exact signing key |
| New `<service>-migrate` SA / Vault role | `kanz/<service>-migration` | Migration DSN only, no app DSN/signing key |
| Existing postgres-provisioner | Existing protected role-password record | Bootstrap/migration authority; never mounted in the runtime Pod |
| Existing Tokyo postgres-migrations Job | Existing protected bootstrap path | All-eight-service migration ordering remains intact |

New runtime records must contain no migration DSN in **any** readable version;
migration-only records must contain no app DSN/signing key in any readable version.
The bounded history check accepts 1–16 versions and fails closed on missing,
deleted, destroyed, unknown or excessive history. An operator must review a new
record/history transaction rather than guess around that refusal. No old record
is deleted and no DB password or signing key is rotated by these commands.
Preparation reserves its copy/state writes and both future activation markers;
activation reserves its two markers for all three services before the first
policy changes. An accepted prepared record can reach active within version16,
and ordinary base bootstrap does not append markers to an already active record.

## Prepare a reviewable transaction

First obtain accepted-main source proof, immutable release/migration provenance
and separate operator authorization for the actual estate. The fixture workflow
is designed to run actual pinned Vault 1.21.4 and PG16 with dummy Kubernetes
principals and a dummy TokenReview server. Its result does not prove real CSI/Kubernetes/CNI,
SPIFFE/NATS continuity, rollout, or credential rotation. Ordinary applicable
build/vet/lint/architecture/security checks still apply to the exact commit.
The workflow and privileged parent supervisor are part of the reviewed fixed-head
trusted computing base. Their source review is a prerequisite to execution; a
receipt does not certify a maliciously changed workflow or parent.
The pinned Go image builds the standard `cmd/test2json` converter offline in the
executable build sandbox. The runtime worker executes parent-qualified read-only
ELFs from `/tools`, keeping `/scratch` noexec. Failure diagnostics retain fixed
process/event observations and required test identities; raw output is discarded.
The same offline boundary generates one public dummy Kubernetes CA, with its
signing key only in memory. The parent qualifies its bounded single-certificate
PEM and mounts only that read-only file at Vault's standard in-pod CA path. It
does not supply a service-account token or change the canonical bootstrap.
The later owned TokenReview TLS CA, reviewer JWT and verification keys remain
independent. Fixed bootstrap observations describe recognized error markers;
an unclassified failure never becomes an invented cause or accepted proof.

Render externally with the reviewed commit/tree and the approved PyYAML 6.0.3
dependency. For base, omit `--input`. For Tokyo, supply the exact rendered Tokyo
overlay from that source (retain its digest/provenance; do not substitute base
images). The renderer is inactive until its output is explicitly adopted:

```sh
python3 tools/runtime_credentials/manifests.py \
  --source-sha "$reviewed_commit" --source-tree "$reviewed_tree" \
  --input "$external_tokyo_render" --output "$new_external_staging_directory"
```

The receipt binds source assertions, normalized input and output digests and
resource identities. Independently check commit/tree against Git; CLI arguments
are not source verification. Inspect both output files and the migration image,
directory, environment, mount, SA, namespace, signing projection and release
annotation before authorizing any remote action. Existing runtime SA names stay
unchanged so this does not invent a new SPIFFE/NATS workload identity.

Preparation uses the existing in-Vault bootstrap helper, with a deliberately
separate mode. `Invoke-VenueSecrets.ps1 -Target DataPlane -Mode Bootstrap
-PrepareRuntimeIsolation` stages the scoped remote command and requires exact
reviewed main/account/instance inputs. **This is a remote operator action**:
`-StageOnly` in that wrapper still uploads through SSM. Neither command is an
offline review or authority to act; do not run it before separate authorization.

The `runtime-isolation-prepare` mode copies only the approved fields *inside the
Vault process*. Partial or different target records refuse overwrite. It creates
new unused migration policies/roles, marks `v1-prepared`, and leaves existing
runtime policies and workloads untouched. Never extract a record or token to an
operator file, CLI argument, transcript or artifact.

## Authorized base/Tokyo ordering

1. Preserve backups/recovery authority and capture non-secret release, policy,
   SA, schema-owner and role-membership evidence. Confirm existing app roles are
   NOSUPERUSER/NOBYPASSRLS and cannot SET ROLE to an owner, CREATE schema/database
   objects, TRUNCATE, install triggers or execute security-definer code in the
   selected schema. A startup refusal must be repaired at its owning input.
   Runtime current/session identities must be identical; downscoping an owner
   connection with SET ROLE does not remove RESET ROLE authority. The guard
   checks the conservative MEMBER-role superset for assumable privileges, not
   only the currently inherited ACL.
2. After the separately authorized in-Vault prepare, apply only reviewed
   `migration-prerequisites.yaml`. Each service has its own migration SA, one
   migration-only CSI projection, a bounded independent Job and scoped database
   egress. Do not mount the identity signing key or app credential in that Job.
   Rendered NetworkPolicy adds a database allowance; effective CNI permissions
   are the union of applicable policies and remain operator-unverified.
   In Tokyo, first retain/wait for the existing postgres-provisioner and
   postgres-migrations Jobs; these establish roles and the all-eight-service
   schema baseline. Base also requires its existing independently approved
   provisioner/DSNs; this component does not invent a database bootstrap path.
3. Wait for all three new Jobs to complete and independently verify current
   schema checksums. A Kubernetes Job completion alone is not runtime authority
   separation. Record only non-secret outcomes. Re-run/recreate a fixed-name Job
   only after inspecting its immutable template/image and prior result; do not
   treat an old completed Job as proof for a different release.
   Before any runtime restart, apply the reviewed owner-side
   [hardening SQL](harden-migration-bookkeeping.sql) to each selected database
   with its **explicitly verified** runtime role. Tokyo's existing provisioner
   maps accounting/audit/identity to `accounting_app`, `audit_app`, `identity_app`;
   base must supply and verify its actual role instead of guessing those names.
   Existing broad default grants include `schema_migrations`: revoke effective
   INSERT/UPDATE/DELETE, including column and PUBLIC grants. The template leaves
   SELECT unchanged, refuses absent/owner runtime identities and rolls back if
   assumable group writes remain; it does not modify a shared group implicitly.
   Repeat this prerequisite after database recreation/recovery or any grant
   change. It does not rewrite applied migrations or the shared migration CLI.
   New runtime releases deliberately refuse still-writable bookkeeping. The
   isolated fixture is designed to execute this same DO body with a bound dummy
   role and assert writable table/column controls versus owner-revoked success.
   Separately, an authorized parameter administrator must apply the staged
   [replication-parameter restriction](harden-runtime-replication.sql) with that
   explicit role. Remove effective and assumable SET/ALTER SYSTEM rights to
   `session_replication_role`, including PUBLIC or shared-role grants; the
   template refuses remaining shared-role authority rather than guessing its
   ownership. Inspect server, database, role and per-database role defaults and
   reconnect as the runtime: PostgreSQL16 `origin` and `local` run ordinary WORM
   triggers; `replica` suppresses them and is refused even without SET authority.
   Revocation does not fix an existing session or an administrator-supplied
   replica default. The fixture is designed to assert a SET LOCAL replica bypass
   of the current audit trigger inside a rolled-back transaction and local-mode
   WORM enforcement. Acceptance needs the exact-revision CI results. Applied
   trigger migrations remain unchanged.
4. Quiesce the affected runtimes according to the service's existing drain and
   outbox/session continuity procedures. Existing audit/accounting state must not
   be discarded. Identity must retain signing material, session epochs and audit
   delivery. Do not regenerate a key to make startup succeed.
5. Inside the controlled Vault process, an authorized operator explicitly invokes
   `runtime-isolation-activate` with `RUNTIME_ISOLATION_CUTOVER_APPROVED=yes`.
   The flag is an intentional-action guard, **not authorization**. This switches
   existing service policies to new runtime-only paths; the existing SA cannot
   authenticate to `<service>-migrate`. Each switch is marked `v1-activating`
   before policy publication and `v1-active` afterward. Partial failure leaves a
   refusal; base bootstrap cannot silently restore legacy access.
6. Adopt reviewed `runtime-cutover.yaml` through the separately authorized
   GitOps transaction before restarting affected workloads. It removes only the
   selected privileged init containers, changes their DB SPCs to app-only new
   records, and moves identity signing projection to its new runtime record.
   The workflow does not build/promote new release images. The operator must use
   independently accepted images containing the startup check and correct schema.
7. Verify actual SA-to-role denial/allow controls through real CSI and enforcing
   CNI: runtime may read its current app/signing record and run required DML;
   runtime cannot read old mixed records at any version, migration-only records,
   sibling records or log in to a migration role. Migration SA must migrate but
   must not read runtime signing/app material. Wrong namespace/account must deny.
   Verify schema owner, inherited/SET ROLE rights, direct/PUBLIC privileges,
   audit append/query continuity, accounting RLS/book continuity and identity
   login/session/audit delivery. No real venue/order/capital action is implied.
8. Revoke/expire old Vault tokens and rotate every migration credential that may
   already have been obtained by a runtime, with protected provisioner and new
   migration record updated atomically under its existing maintenance process.
   A policy update does not revoke an already extracted PostgreSQL credential.
   Prove old token/old KV-version denial **and** old-DSN authentication refusal;
   prove the new owner credential can perform authorized migration afterward.
   Do not grant the runtime legacy-record access to repair a failed Job.

## Recovery and rollback

Before activation, discard/review unused staged resources without changing
runtime policy; old workloads remain on their original contract. A partial
record or history failure requires operator inspection and an explicit new
preparation transaction, not deletion of evidence or a forced overwrite.

After activation, rollback application images only to a reviewed version that
works with the same app-only runtime files and schema. Preserve the new policy,
separate migration identity and identity signing key. A legacy image whose init
expects `migrate-database-url` in a runtime Pod is not a safe rollback target.
If policy publication stopped at `v1-activating`, keep runtimes quiesced, repair
the reviewed migration/runtime records and re-run the authorized activation to
finish; unknown markers never fall back to the old mixed record. No automatic
rollback restores old tokens, old owner passwords, broad policies or shared-SA
privileged init. Any exceptional rollback of that security boundary requires
its own explicit decision and leaves #1747/#1331 acceptance pending.

Full closure requires the remaining consumers, accepted-main source/CI proof and
the operator evidence above. Preparation or a green dummy fixture is not closure.
