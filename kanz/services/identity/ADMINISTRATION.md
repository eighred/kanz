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

The active GitOps manifest and image digest deliberately remain unchanged. The
staged `deploy/admin-role-patch.yaml` is outside every synced workload path;
apply it only together with a verified image digest from this change.

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
the re-enabled administrator must log in again (tokens issued in the revocation
second are conservatively refused).

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
