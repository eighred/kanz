# Account access administration

## Invitation revocation and reissue

Apply `0005_invitation_lifecycle.sql` with **old identity writers stopped**, then
start the updated identity binary before admitting traffic. An old binary lacks
the lifecycle routes and projections; the database constraint independently
refuses marking a revoked row as redeemed. Migrations 0001–0004
must already be applied. The migration preserves existing tokens/authority and
can itself be applied twice. Do not replay migration 0001 after the upgrade:
its retired index cannot represent multiple revoked historical invitations.

The invitation list reports server-derived `pending`, `accepted`, `expired`, or
`revoked`, plus revision, lifecycle timestamps, revoker and replacement ID.
Expiry is evaluated when the list is read; reload for current state. Token and
token hash are absent. The existing private-channel handoff remains: the browser
shows the initial or replacement link once; the application does not send email
or attest delivery. Dismissing the panel discards its copy, not the clipboard.

`POST /invites/{id}/revoke` and `POST /invites/{id}/reissue` accept only
`{"revision": <reviewed revision>}` and require a current identity administrator
in the invitation's tenant. The BFF exposes those exact paths under
`/api/identity/invites`, with its server-held bearer and same-origin guard.
Unknown and foreign invitations both return 404. A stale revision, accepted
invitation, already replaced source, or conflicting outstanding offer returns
409. A reviewed repeat revoke is a no-op; it adds no duplicate audit entry.

Reissue copies subject, tenant, roles and portfolios from the locked source,
rechecks current domain/authority policy, assigns a fresh ID/token and current
configured TTL, and revokes the original in one transaction. Revoked or expired
offers may be reissued after review. Accepted offers cannot: use account controls
for existing accounts. The original source becomes terminal once reissued. If a
response is lost, reload, locate its replacement and explicitly reissue that
record; the previous secret cannot be recovered and mutations are not retried
automatically.

Redemption's conditional update and lifecycle commands serialize on the same
invitation row. Either redemption wins and the command refuses, or the command
commits and the old token is unusable. The live-subject index excludes revoked
records but keeps expired ones until an explicit revoke/reissue. This prevents
an expired offer from silently becoming a competing authority.

Revoke/reissue commit canonical `identity.invitation.revoke` / `.reissue`
DecisionLog records in the existing forced-RLS, append-only identity journal,
including actor, before/after summaries and the replacement's authority. Audit
failure rolls back all state changes. Neither token nor hash is audited or
logged. Initial creation/redemption retain their existing logging behavior;
complete lifecycle export to central audit and automated invitation delivery
remain separate #1227 slices. Native MFA and privileged step-up are described in
[MFA deployment and recovery](MFA.md). Verified mailbox delivery and forgotten-password
recovery are available through the opt-in [recovery flow](RECOVERY.md).

## Self-service password rotation

After signing in with a platform credential, open **Governance → Change
password** (`/password`). Enter the current password and a different replacement
of 12–1024 Unicode characters, confirm it, and submit once. Successful rotation
clears the current BFF session; sign in again with the replacement. No command,
shell history, token copy, or administrator privilege is required. External
OIDC credentials must be changed at their issuing provider.

The BFF's exact `POST /auth/credential` route forwards the server-held bearer and
resolved client address to identity's `POST /credential`. The request contains
only `current_credential` and `new_credential`; subject and tenant come from the
verified token. Both login and rotation spend the same per-subject/per-source
rate-limit budgets. Rotation is available when authenticated provisioning is
wired, but does not require the identity-administrator role. It consumes at
most 16 KiB (two UTF-8 credentials) with the same five-second body deadline.

Identity verifies the current credential before checking replacement policy.
Missing accounts, incorrect credentials, disabled accounts and stale sessions
receive the same generic 401; policy refusals are 400 and throttling is 429.
Argon2id work happens outside database locks. Inside the existing tenant lock,
the transaction rechecks account status, tenant, session epoch and the exact
verified hash, then atomically replaces the hash, increments session epoch and
access revision, advances the legacy revocation watermark, and inserts canonical
`identity.account.credential.rotate` evidence. No plaintext or hash enters audit
records, logs or responses. Audit failure rolls the whole transaction back.
Competing changes can invalidate a verified snapshot; sign in again and retry.
Login rehash compares the original hash, so it cannot restore an old password
after rotation. A login already in flight keeps its older epoch and is revoked.

Identity rejects old sessions immediately; gateway enforcement follows its
existing bounded-refresh, fail-closed revocation feed. Already admitted requests
are not retroactively cancelled. Other BFF session records may remain until
expiry, but their old bearer loses authority at enforcement. There is no automatic
retry: after an uncertain network result, try signing in with the replacement,
then the previous password if refused. This is an authenticated password change,
not forgotten-password recovery or MFA enrollment.

## Administrative access

Apply all identity migrations, including `0004_access_audit.sql`, before running
the updated service. The new binary fails access writes if the journal is absent;
it never substitutes a log line for durable evidence.

Administrative requests consume at most 8 KiB before authorization can respond,
with a five-second body-read deadline. This prevents unread bytes from resetting
a `Connection: close` response while bounding slow or oversized callers. Oversize
bodies receive 413; timed-out bodies receive 408; neither reaches a mutation.
After authentication, status-only commands accept no body or an empty JSON
object and reject other payloads with 400. Small unauthorized requests retain
their authentication/authorization refusal rather than becoming body-validation
errors. No automatic mutation retry hides a lost response.

`GET /users?after=<subject>` requires current identity-administrator authority and
returns at most 25 tenant-local accounts with `next_cursor`. An empty cursor means
the page is complete; a full final page can be followed by an empty page. Records
include subject, tenant, roles, portfolios, status, revision, session epoch,
creation/update timestamps and invitation creator. Historical creator absence is
explicit. Update time is account activity, not evidence of a login. Credentials
and invitation token hashes are excluded from the projection and journal.

`PUT /users/{subject}/access` accepts `revision`, `roles`, and `portfolios`. The
revision must match the directory record reviewed by the administrator. A stale
edit returns 409; reload and review before resubmitting. Unknown and foreign-tenant
subjects both return 404. Empty/duplicate authority entries and identity-admin
combinations other than `kanz-user` are refused. An empty portfolio list confers
no capital entitlement; read scope follows each endpoint’s existing policy. Role names obtain meaning only through gateway policy.

The transaction acquires the existing tenant administration lock, rechecks the
actor's current status and session epoch, locks the target, preserves the last
active administrator, and updates authority and revocation generation together.
Every successful edit revokes older sessions, including tokens minted late from
an older login snapshot. Existing enable/disable routes use the same lock and
durable journal; re-enable never revives an older session. Gateway enforcement
uses the existing bounded-refresh, fail-closed revocation feed. Identity itself
checks current state on each administrative request. An already admitted request
is not retroactively cancelled.

`identity_access_audit` stores canonical `observation.v1.DecisionLog` JSON with
the authenticated actor, tenant, target, action, time, and credential-free before
and after snapshots. Its insert commits in the same transaction as the change.
Insert failure rolls the entire change back. Forced RLS binds journal reads and
writes to `app.tenant_id`; the mutation sets this scope only for its transaction.
Unscoped login connections cannot read the journal. SQL triggers refuse UPDATE, DELETE,
and TRUNCATE. Database-owner DDL and database backups remain privileged operations;
this is not a claim of tamper resistance against the database administrator.
The additional status log recorder is an operational projection. Central audit
bus delivery is the separate #1227 phase 7 slice; the journal preserves the source
records for that delivery without inventing another event schema.

The browser's **Users and permissions** page reaches explicit BFF routes under
`/api/identity/users`. Tokens remain in the BFF session, and unsafe requests pass
the same-origin boundary. `GET /auth/permissions` proxies identity's
`GET /permissions`, which verifies the token and current account before reporting
identity-administration authority. `GET /api/v1/permissions` reaches the gateway's
authenticated `GET /v1/permissions`; its capabilities and route list are computed
from the same grants and registered routes that enforce requests. It reports the
token's portfolio scope, not authorization for a particular transaction. Domain
controls still decide each action. Neither response is cached. An unavailable
authority is displayed as unavailable, never inferred from a browser role name.
