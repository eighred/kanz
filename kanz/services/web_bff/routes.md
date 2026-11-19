# Browser HTTP route contracts

The table describes the patterns registered by the [BFF router](internal/server/server.go).
It includes every method-specific route and both delegated subtrees. Go's GET patterns also match HEAD.
A required cookie must resolve to an active session. Optional permits a request without a current session.
The `oidc`, `metrics` and `static` registrations require the corresponding server option.
These conditions describe registration and are independent of an upstream service's availability.

Every row runs behind the [browser origin policy](internal/server/browser_security.go).
Unsafe cross-origin requests can receive a 403 JSON error before the BFF reads a body.
A **session** is the opaque cookie resolved through the [session manager](internal/session/session.go).
Identity and gateway operations use the bearer held by the server. Their owners perform the
resource-specific authorization. **BFF error** is a JSON object with a string `error`. Session
failures use the status and message from `sessionError`. Identity pass-through operations retain
the identity HTTP status and body. Authentication and API responses set `Cache-Control: no-store`.

| ServeMux pattern | Enabled | BFF handler | Session cookie | Authority | Request | Success | Refusal |
| --- | --- | --- | --- | --- | --- | --- | --- |
| GET /auth/mfa | always | handleMFA | required | Session and identity MFA actor | No body | 200 projected [MFAStatus][mfa] | [MFA projection][mfa-bff] or session BFF error |
| POST /auth/mfa/register/begin | always | handleMFA | required | Session and identity verifies registration credential | [mfaBegin][mfa-service] name and password | 200 projected [registration ceremony][ceremony] | [MFA projection][mfa-bff] or session BFF error |
| POST /auth/mfa/register/finish | always | handleMFA | required | Session and identity verifies registration challenge | [mfaFinish][mfa-service] id and WebAuthn credential | 200 session identity and rotated cookie | [MFA projection][mfa-bff] or session BFF error |
| POST /auth/mfa/login/finish | always | handleMFA | optional | Identity verifies login challenge | [mfaFinish][mfa-service] id and WebAuthn credential | 200 session identity and cookie | [MFA projection][mfa-bff] or session BFF error |
| POST /auth/mfa/stepup/begin | always | handleMFA | required | Session and identity MFA actor | [mfaBegin][mfa-service] JSON | 200 projected [authentication ceremony][ceremony] | [MFA projection][mfa-bff] or session BFF error |
| POST /auth/mfa/stepup/finish | always | handleMFA | required | Session and identity verifies step-up challenge | [mfaFinish][mfa-service] id and WebAuthn credential | 200 session identity and rotated cookie | [MFA projection][mfa-bff] or session BFF error |
| POST /auth/mfa/remove | always | handleMFA | required | Session and identity authorizes factor removal | [mfaRemove][mfa-service] factor id | 200 session identity and rotated cookie | [MFA projection][mfa-bff] or session BFF error |
| GET /healthz | always | handleHealth | optional | Public probe | No body | 200 status ok | [HTTP routing][router] |
| GET /readyz | always | handleReady | optional | Public probe | No body | 200 status ready | 503 status not ready |
| GET /metrics | metrics | Metrics | optional | Configured metrics handler | HTTP scrape | [Prometheus handler][metrics] response | Metrics handler response |
| POST /auth/login | always | handleCredentialLogin | optional | Identity verifies credential | Subject and credential JSON | [completeLogin][router] 200 session identity and cookie or 202 MFA challenge | Opaque authentication, throttling, JSON decode and session BFF errors |
| POST /auth/redeem | always | handleRedeem | optional | Identity validates invite and credential | Token and credential JSON | [completeLogin][router] 200 session identity and cookie or 202 MFA challenge | Validation, authentication, throttling and session BFF errors |
| GET /auth/login | oidc | handleLogin | optional | Public OIDC initiation | No body | 302 identity-provider redirect and state cookie | [OIDC flow][router] or session BFF error |
| GET /auth/callback | oidc | handleCallback | optional | State cookie, pending verifier and verified OIDC identity | Code and state query or provider error | 302 SPA redirect and session cookie | [OIDC flow][router] or session BFF error |
| POST /auth/logout | always | handleLogout | optional | Caller cookie when present | No body | 200 signed-out status and cleared cookie | Session BFF error |
| POST /auth/credential | always | handleCredentialRotation | required | Session and identity credential policy | [Current and new credentials][rotation-service] | 204 empty and cleared cookie | [Credential projection][rotation] or session BFF error |
| POST /auth/mailbox | always | handleRecovery | required | Session and identity verifies credential | [Address and credential][recovery-service] | 202 status accepted | [Recovery projection][recovery] or session BFF error |
| GET /auth/mailbox | always | handleRecovery | required | Session and identity mailbox owner | No body | 200 projected [MailboxStatus][recovery] | [Recovery projection][recovery] or session BFF error |
| POST /auth/mailbox/verify | always | handleRecovery | optional | Identity verifies mailbox challenge | [Verification token][recovery-service] | 204 empty | [Recovery projection][recovery] |
| POST /auth/recovery | always | handleRecovery | optional | Public recovery initiation | [Subject][recovery-service] | 202 status accepted | [Recovery projection][recovery] |
| POST /auth/recovery/consume | always | handleRecovery | optional | Identity verifies recovery challenge | [Token and replacement credential][recovery-service] | 204 empty and cleared cookie | [Recovery projection][recovery] or session BFF error |
| GET /auth/me | always | handleMe | required | Session | No body | 200 subject, tenant and RFC3339 expires_at | Session BFF error |
| GET /auth/sessions | always | handleSessions | required | Session manager resolves caller | No body | 200 sessions array of [Summary][session] | Session BFF error |
| POST /auth/sessions/{id}/revoke | always | handleRevokeSession | required | Session manager checks actor and target atomically | Target session id in path | 200 revoked boolean | Session BFF error |
| GET /api/identity/invites | always | handleInvites | required | Session and identity operator | No body | 200 array of [invitationJSON][provision] | Identity response or BFF error |
| POST /api/identity/invites | always | handleInvites | required | Session and identity operator in tenant | [createInviteRequest][provision] | 201 [issuedInvitation][invites] | Identity response or BFF error |
| POST /api/identity/invites/{id}/revoke | always | handleInvites | required | Session and identity operator in tenant | Invite id and [revision][invites] | 200 [invitationJSON][invites] | Identity response or BFF error |
| POST /api/identity/invites/{id}/reissue | always | handleInvites | required | Session and identity operator in tenant | Invite id and [revision][invites] | 201 [issuedInvitation][invites] | Identity response or BFF error |
| GET /api/identity/users | always | handleIdentityAccess | required | Session and identity operator | Optional after query | 200 [users and next_cursor][access] | Identity response or BFF error |
| PUT /api/identity/users/{subject}/access | always | handleIdentityAccess | required | Session and identity operator in tenant | Subject and [revision, roles, portfolios][access] | 200 [accessJSON][access] | Identity response or BFF error |
| POST /api/identity/users/{subject}/disable | always | handleIdentityAccess | required | Session and identity operator in tenant | Subject in path and empty administrative body | 200 [account status change][status] | Identity response or BFF error |
| POST /api/identity/users/{subject}/enable | always | handleIdentityAccess | required | Session and identity operator in tenant | Subject in path and empty administrative body | 200 [account status change][status] | Identity response or BFF error |
| GET /auth/permissions | always | handleIdentityAccess | required | Session and identity verifies current actor | No body | 200 [subject, tenant, identity_admin][access] | Identity response or BFF error |
| GET /api/preflight | always | handlePreflight | required | Session | No body | 200 [preflightStatus][preflight] including unavailable evidence | Session BFF error |
| /api/ | always | handleProxy | required | Session, gateway signing when configured and upstream route authorization | Gateway method, suffix, query and bounded body | Gateway HTTP status, media type and body, including streams | Gateway response or BFF error |
| / | static | staticHandler | optional | Public SPA assets | HTTP asset or navigation request | [Static handler][static] file or SPA shell | Static file response or 404 for missing assets and reserved paths |

Session identity responses contain the subject, tenant and RFC3339 expiry. The session cookie is
HTTP-only. Credential, token and challenge handling is defined by the linked handlers and their
upstream authorities. The generic gateway delegate describes its transport contract. Each
downstream operation has its own payload and authorization at the gateway.

[router]: internal/server/server.go
[mfa]: ../../internal/identity/mfa.go
[mfa-bff]: internal/server/mfa.go
[mfa-service]: ../identity/internal/server/mfa.go
[ceremony]: internal/identityclient/mfa.go
[metrics]: cmd/web_bff/main.go
[rotation-service]: ../identity/internal/server/rotation.go
[rotation]: internal/server/rotation.go
[recovery-service]: ../identity/internal/server/recovery.go
[recovery]: internal/server/recovery.go
[session]: internal/session/session.go
[provision]: ../identity/internal/server/provision.go
[invites]: ../identity/internal/server/invite_lifecycle.go
[access]: ../identity/internal/server/access.go
[status]: ../identity/internal/server/status.go
[preflight]: internal/server/preflight.go
[static]: internal/server/static.go
