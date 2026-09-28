# Verified mailbox and password recovery

Apply migration `0006_recovery.sql` after migrations 0001–0005 before enabling
recovery. Existing accounts start without a recovery mailbox; email-shaped
subjects and the invitation domain allowlist do not prove mailbox ownership.
The existing manual invitation handoff is unchanged.

Recovery is opt-in. The agreed Stalwart transport configuration is:

```text
IDENTITY_RECOVERY_ENABLED=true
IDENTITY_SMTP_ADDRESS=mail.eighred.com:587
IDENTITY_SMTP_TLS=starttls
IDENTITY_SMTP_FROM=noreply@eighred.com
IDENTITY_RECOVERY_ORIGIN=https://kanz.eighred.com
```

Configure `IDENTITY_SMTP_USERNAME` for the authorized submission account and
mount `IDENTITY_SMTP_PASSWORD_FILE` through the deployment's secret mechanism.
Do not put a password in manifests or command arguments. Both authentication
fields must be configured together; leaving both absent supports an explicitly
trusted SMTP relay, not the authenticated Stalwart deployment. The sender address
does not itself grant submission authority. Permit identity's egress only to the
approved submission endpoint. `IDENTITY_ADMIN_ROLE=kanz-identity-admin` must
already be configured so the service has a token verifier.

TLS is mandatory: `starttls` requires the server's STARTTLS extension; `tls`
uses implicit TLS. Certificate chain and hostname validation are never disabled.
Every delivery connection has a 15-second deadline, including DNS, connect,
TLS, authentication and DATA, and closes on cancellation. Configuration rejects
malformed sender addresses, incomplete authentication and non-HTTPS public origins.
An enabled worker that cannot access its durable queue stops the service rather
than leaving an apparently working recovery endpoint with no delivery worker.

## Account flows

From **Change password → Set up a recovery mailbox**, an active user supplies a
current password and email address. Identity verifies both the credential and
current session, applies the configured invitation-domain policy, and rechecks
the verified credential snapshot under the account lock. This creates a mailbox
challenge; it does not immediately change the recovery destination. Only an
explicit confirmation of the emailed link commits the verified address.

**Forgot password?** accepts an account subject, never a destination address.
Unknown, disabled, unenrolled and cooldown-limited accounts all receive the same
202 response. SMTP runs asynchronously, outside this public request. Password
recovery sends only to the stored verified address and never enables a disabled
account. A successful reset changes the password, advances `session_epoch` and
the token watermark, increments the access revision, and writes a canonical
DecisionLog in one transaction. Audit failure rolls everything back, including
proof consumption. Late login rehashes remain fenced by the credential CAS.

Old tokens lose authority at identity immediately and at downstream verifiers
through the existing bounded, fail-closed revocation feed. Already admitted
requests are not retroactively cancelled. BFF session records other than the
resetting browser's record expire through the existing session lifecycle; their
bearer tokens no longer authorize new operations after revocation refresh.

All challenges contain 256 bits of random entropy, store only a SHA-256 digest,
expire 20 minutes after the request, and are purpose- and account-epoch-bound.
They cannot be exchanged between verification and password recovery. Credential
or access changes invalidate outstanding proofs; changing the verified mailbox
also retires a previously sent reset link. The browser receives the proof only
from an email URL fragment, strips that fragment, and submits it in a bounded
POST body after explicit confirmation. There is no automatic sign-in after reset.

## Delivery semantics and diagnosis

PostgreSQL holds at most one request slot per account and purpose, with a
database-enforced one-minute request cooldown. One sender per replica claims
work with `FOR UPDATE SKIP LOCKED`; LISTEN/NOTIFY wakes committed work, and a
30-second lease scan recovers missed notifications and crashed workers. Each
attempt generates a new token in memory. No raw proof is stored in the queue.

An SMTP DATA acknowledgement marks the request `sent`, meaning **accepted by
SMTP**, not delivered to the inbox. Rejection or an ambiguous connection outcome
retries after one minute, up to three attempts. Each retry invalidates the prior
attempt's token; an earlier delayed email can therefore contain an unusable
link. A request/attempt fence prevents a late worker from overwriting replacement
state. An abandoned third attempt becomes `failed` after its lease expires.
Failed and expired requests require a new explicit request. There are no
unbounded goroutine queues or retained plaintext retry payloads.

The authenticated mailbox status endpoint shows only the caller's verified
address and latest verification destination/state. It never returns a proof or
digest. Worker failures log a request ID and attempt number, never SMTP replies
or message content. Diagnose persistent delivery failures using the submission
server's operational logs and the configured sender's permissions. Do not infer
inbox delivery from the HTTP 202 response or the `sent` state.

The BFF exposes only `/auth/mailbox`, `/auth/mailbox/verify`, `/auth/recovery`,
and `/auth/recovery/consume`; all unsafe browser requests use the existing
same-origin guard. Recovery routes remain absent at identity when disabled.
Existing MFA, central lifecycle audit export and shared-session inventory work
remain tracked separately in #1227; this feature establishes no MFA assurance.

## Verification

With a disposable **NOSUPERUSER** `TEST_POSTGRES_URL` configured:

```sh
go test -p 1 ./internal/identity ./services/identity/internal/server ./services/identity/internal/delivery ./services/identity/internal/config ./services/web-bff/internal/server -count=1
```

The delivery tests start a real SMTP server with ephemeral in-memory TLS keys
and exercise STARTTLS, implicit TLS, SASL, DATA refusal, untrusted certificates,
cancellation, the durable worker, mailbox proof and recovery. PostgreSQL tests
cover concurrent consumption, stale epochs, expiry, retry fencing and audit
rollback. Without `TEST_POSTGRES_URL`, database tests skip and are not evidence
of transaction correctness. Linux CI supplies the race-detector proof.

For the optional Chromium proof, build the BFF and SPA, install Playwright and
its Chromium runtime in your test environment, and set `TEST_RECOVERY_BFF` to
the compiled BFF, `TEST_RECOVERY_STATIC_DIR` to the SPA's `dist` directory,
`TEST_RECOVERY_NODE` to Node, and `TEST_RECOVERY_BROWSER_SCRIPT` to
`kanz-web/e2e/recovery.cjs`. `TEST_RECOVERY_PLAYWRIGHT` can name the installed
Playwright module when it is outside Node's normal module search path. With the
same disposable database configured, run:

```sh
go test -p 1 ./services/identity/internal/delivery -run TestWorkerDeliversDurablePostgresChallengeThroughRealSMTP -count=1
```

This boots the real identity HTTP handlers and a compiled BFF against the test
database and SMTP server. Chromium completes enrollment and recovery using the
captured SMTP messages, then proves old-session, old-password and replay refusal.
The test inbox is loopback-only and exists solely for this disposable fixture.
