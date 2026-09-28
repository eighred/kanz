# Browser boundary

The BFF serves the SPA and its cookie-authenticated API on one origin. Before
routing any unsafe request, it checks Go's `http.CrossOriginProtection` and,
when present, requires `Origin` to equal the request's scheme and Host exactly.
Sibling origins on the same site are refused too. Ambiguous, opaque and
malformed origins are refused before reading the body or changing a session.

The production scheme is HTTPS when secure cookies are enabled, including the
Cloudflare tunnel's HTTP loopback hop. The tunnel must preserve the public Host;
`Forwarded` and `X-Forwarded-*` cannot override the origin decision. Local HTTP
development uses `WEB_BFF_INSECURE_COOKIES=1`; Vite preserves the browser Host.
Headerless non-browser clients still require normal authentication. Safe GET,
HEAD and OPTIONS requests retain their routing behavior; OIDC callbacks retain
their state and PKCE checks.

All responses receive the same CSP, framing, MIME-sniffing, referrer, permissions
and cross-origin isolation headers. The CSP permits same-origin scripts, styles,
images, fonts and connections, without inline script or eval exceptions. HSTS
is enabled only with secure cookies and applies only to the current host.
Authentication and API responses are `no-store`; gateway responses cannot replace
the BFF's browser policy or grant CORS access to its sessions.

Regression tests cover rejection before body/session/upstream access, concurrent
requests over real TLS, local HTTP, OIDC redirects, static assets, errors and
hostile upstream headers. When changing CSP, also build `kanz-web` and exercise
login, authenticated navigation and logout in a real browser against the compiled
BFF and identity service, checking for `securitypolicyviolation` events.

## Session authority

Memory mode remains the default and requires the existing singleton/Recreate pin.
A restart invalidates those sessions. Both modes bound active sessions (10,000),
pending OIDC logins (1,000), and sessions per subject/tenant/provider (20).
`WEB_BFF_MAX_SESSIONS`, `WEB_BFF_MAX_PENDING_LOGINS`, and
`WEB_BFF_MAX_SESSIONS_PER_SUBJECT` override these limits. Capacity exhaustion
refuses new sessions; it never evicts another active operator. Session lifetime is
bounded by the token and `WEB_BFF_SESSION_TTL` (default 1h, maximum 24h).

`WEB_BFF_SESSION_MODE=postgres` enables shared authority. Supply
`WEB_BFF_SESSION_DSN_FILE` and `WEB_BFF_SESSION_KEY_FILE` through the existing
secret mount mechanism. The key must decode from standard base64 to exactly 32
random bytes and be identical across replicas. Apply every SQL migration in this
service's `migrations/` directory before startup. Use a dedicated schema and a
NOSUPERUSER/NOBYPASSRLS role; pool construction uses the canonical four-connection
service budget. Production role grants, pool sizing and backup placement must be
completed under #1288 before activation. Do not log or check in the key or DSN.

The first replica records key fingerprint, TTL and capacity policy. Other replicas
refuse mismatches. Updating these settings requires a coordinated maintenance
window, invalidating existing session/pending slots and resetting the policy row
before all replicas start with the new settings. Do not delete session audit.
Treat any database restore as invalidating all browser authority: before admitting
traffic, clear restored slots, reset policy and rotate the encryption key. Reusing
restored authority can resurrect a previously revoked cookie. Database failover
must preserve acknowledged commits; asynchronous replication alone does not prove
that revocation survives a region loss. #1288 tracks the required restore drill.

Shared mode stores only SHA-256 cookie/state hashes and AES-256-GCM encrypted
bearer, refresh and PKCE payloads. Session ownership is subject + tenant + provider.
Forced RLS protects owner records and canonical decision audit. Expiry slots are
a global pre-auth directory containing no plaintext identity or credentials.
Writes serialize capacity, replacement and revocation in one transaction; audit
failure rolls the mutation back. This lock is on the login control path, not the
financial execution path. Audit retention is independently managed evidence, not
part of the bounded active-session quota.

There is no local shared-session cache or outage fallback. Database failure returns
503, retains the browser cookie for retry, and makes readiness fail. Requests
already admitted before revocation may finish; subsequent lookups refuse the
cookie. Each store operation has a three-second deadline. Cleanup runs once a
minute and joins on shutdown. Memory mode has no durable cross-process audit.

`GET /auth/sessions` returns metadata and non-bearer revocation IDs only.
`POST /auth/sessions/{id}/revoke` rechecks current and target ownership atomically.
The browser Sessions page uses these endpoints. Native and OIDC sessions remain
separate owners even when subject/tenant text matches. OIDC ID tokens must verify
against the configured issuer and client audience and carry subject and tenant;
callbacks also require the initiating browser's HttpOnly state cookie.

Run real store and separate-process failover tests with a non-superuser test DSN:
`TEST_POSTGRES_URL=... go test -p 1 -count=1 ./services/web-bff/...`.
The process test builds the production BFF or uses `TEST_MFA_BFF` when provided.
Keep production singleton manifests pinned until #1288's rollout proof is complete.
