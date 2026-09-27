# Identity administration and recovery

Identity administration uses the reserved `kanz-identity-admin` role. It may
accompany `kanz-user`, but no trading, approval, infrastructure, or other role.
Use separate accounts for those duties. Gateway capability mappings cannot use
the reserved role. This separates account administration from node/credential
operations; it does not prove that two accounts belong to different people.

`IDENTITY_ADMIN_ROLE=kanz-identity-admin` enables provisioning. Empty disables it.
The former `IDENTITY_OPERATOR_ROLE` setting is rejected, including when both
settings are present. There is no silent legacy authorization fallback.

## Staged rollout: preserve the current administrator

Live cutover and replacement verification are tracked separately in #1255.

The Tokyo overlay applies the dedicated role together with the verified identity,
migrator and gateway images in `identity-authority-cutover.yaml`. Its replacement
administrator verification and operational evidence are recorded in #1255. Other
environments retain their existing configuration: the staged
`deploy/admin-role-patch.yaml` must only be applied with a verified image digest
after the replacement checks below. Account status and roles are not changed by
the deployment patch.

Do not roll out the changed deployment configuration before completing the
replacement steps. Applying the new configuration to old accounts alone would
remove their provisioning authority. No migration automatically changes roles,
disables an account, deletes an account, or redeems an invitation.

1. Record an approved change with the tenant, named operator, replacement subject,
   and reason. Retain the current administrator's account and working session.
2. On the existing service, issue an invitation to the verified replacement email
   using only `kanz-user,kanz-identity-admin`. If no administrator can authenticate,
   use the infrastructure bootstrap procedure below. An invitation is not proof
   that the replacement exists or can authenticate.
3. Have the replacement user redeem the invitation and perform a fresh password
   login. Verify the subject, tenant and roles without saving the password, invite
   token, or bearer token in tickets, logs, shell history, or source control.
4. Canary the new identity binary/configuration with the existing signing key and
   database. Remove `IDENTITY_OPERATOR_ROLE` and set `IDENTITY_ADMIN_ROLE`.
   Using the replacement's fresh session, verify authenticated `GET /invites`
   succeeds for the correct tenant. Verify the infrastructure-only account is
   refused. Record nonsecret request IDs, time, and results in the change record.
5. Only after those checks, roll out the new identity/gateway configuration.
   Then the replacement may disable the former administrator through the API if
   approved. Keep the old account active until this point. Never replace the
   signing key as part of this role migration. If the canary fails, stop rollout
   and retain the previous deployment and current administrator.

Self-disable returns 409, even if another administrator exists. A different
active administrator must perform offboarding. A tenant transaction lock
serializes status changes and authenticated invite writes; current actor roles,
tenant, status and revocation watermark are checked while holding it. Competing
administrators cannot both disable each other. Enable does not erase revocation:
the re-enabled administrator must log in again to acquire the current session
generation. A login started before disable remains revoked even if signing finishes
afterward. Fresh logins work within the same second and after clock rollback.

## Infrastructure bootstrap / break-glass recovery

Use the existing `kanz-invite` command only through approved infrastructure access
to the identity store. Supply the configured invitation domain policy and a
named `-by` actor tied to an incident/change record. Obtain the database credential
through the existing secret mount/runtime; never paste it into a command argument
or a ticket. Issue only a short-lived invitation with the two roles above and
deliver its one-time secret privately to the named recipient. The recipient
chooses their own password. Record the invitation ID, actor, tenant, subject,
time and approval in the infrastructure audit trail; never the invitation token.

The command's `-by` value is operator-supplied attribution, not authenticated
proof. The privileged infrastructure access log and independent approval are
therefore required recovery evidence. Do not use hand-written SQL to remove
roles, delete users, or disable the final administrator: the application invariant
covers supported API mutations, not an administrator with unrestricted SQL.
There is currently no role-edit/delete API; any future one must join the same
tenant lock and preserve the active-administrator invariant.

Status changes retain the existing atomic token-revocation write and structured
audit logging. Identity audit logs are not yet an atomic tamper-evident outbox;
that separate audit-authority work remains tracked by #1227/#1198/#1207.


## Session-generation rollout (#1256)

Migration `0003_session_epoch.sql` adds a nonnegative BIGINT generation and
backfills every previously revoked account to generation one. Disable increments
it atomically; enable never resets it. The signed `session_epoch` claim and v2
feed encode generations as decimal strings to avoid JSON number precision loss.
Legacy tokens (missing generation) are generation zero: previously revoked users
must log in again. Never-revoked users retain their existing sessions.

The feed kind changes to `kanz.revocations.v2`. Old readers refuse v2 and new
readers refuse v1. Do not mix these versions behind a load balancer. Quiesce
identity login, redemption and administration and drain gateway traffic, apply the
migration, deploy matching identity and gateway image digests, verify v2 feed
priming and fresh login/revocation probes, then reopen traffic. Keep the current
administrator active throughout the separately gated replacement procedure above.
Do not roll identity back alone: an old writer does not increment generations.
Rollback requires maintenance mode and a coordinated identity/gateway version;
retain the migration and revoke outstanding sessions before restoring admission.

Revocation remains bounded by the existing feed polling policy: normally 30 seconds
plus fetch latency, with a 15-minute maximum cached-feed age during an outage.
A token ahead of the cached generation receives 503 until the feed catches up,
preserving its session instead of misclassifying it as revoked.
Generation fencing closes the timestamp and late-mint escape; it does not turn
polling into synchronous revocation. Regressed or incomplete snapshots cannot
renew freshness. Database restore must preserve all published generations; restoring
older identity state requires invalidating old signing keys and draining verifier
caches before serving traffic. Deleting/recreating subjects is not a supported API.
