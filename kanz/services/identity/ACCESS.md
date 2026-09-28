# Account access administration

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
