# Native MFA and privileged step-up

Native login combines the existing password with WebAuthn user verification.
Open **Governance → Authentication** to enroll a passkey or security key using
the current password. Enroll a spare while the original is available. Accounts
may hold five factors; removing the last factor is refused. Additional enrollment
and removal require verification within the preceding five minutes. Enrollment
also requires the current password. The service requests user verification and
checks the signed UV flag, RP ID and exact browser origin. It does not require
hardware attestation or assert that a synced passkey is hardware-bound.

## Deployment

Apply migration `0007_mfa.sql` after migrations 0001–0006, with old identity
writers stopped. Deploy the updated identity, BFF, gateway and web client
together before allowing enrollment. Old binaries do not understand MFA claims
or the enrollment flag and must not serve enrolled accounts.

| Setting | Meaning |
|---|---|
| `IDENTITY_MFA_ENABLED=true` | Mount native enrollment, login completion and step-up routes. Requires authenticated provisioning. |
| `IDENTITY_WEBAUTHN_ORIGIN=https://kanz.example.com` | Exact public browser origin, no path, query or fragment. HTTPS is mandatory. |
| `IDENTITY_WEBAUTHN_RP_ID=kanz.example.com` | Must equal the origin hostname. Choose the durable application hostname before enrollment. |
| `IDENTITY_MFA_REQUIRED=true` | Require fresh MFA for identity administration, including administrative reads. Requires MFA enabled. |
| `API_GATEWAY_MFA_REQUIRED=true` | Require fresh MFA for every gateway capability except Read. |

Flags default to false. Deploy support first, enroll administrators and operators
with spare factors, then enable both required-policy flags across every replica.
Required policy refuses privileged operations by unenrolled accounts; enrollment
remains accessible. External OIDC issuers must supply the same signed assurance
contract to satisfy this native policy; arbitrary `amr` values and request headers
are not accepted. This is an opt-in rollout, not automatic production activation.

Once enrolled, native password login returns only a short-lived WebAuthn ceremony.
It cannot create a bearer or BFF session until the assertion is verified. Missing
MFA provider configuration fails enrolled login closed. Enrolled sessions require
fresh assurance for privileged actions even when global policy is off. Read
capabilities retain normal authorization. Use **Verify security key** on Authentication,
then explicitly submit the intended action; the UI never retries a mutation.

## State and recovery guarantees

Postgres holds public credential material and counters, never authenticator
private keys. Ceremonies expire after three minutes and occupy one replaceable
slot per account and purpose. The transaction locks the account, verifies the
signature and counter, checks its epoch, consumes the challenge, and commits
canonical audit evidence atomically. Concurrent finishes across identity replicas
have one winner. Invalid origin, absent UV, counter regression, stale epoch,
expired, superseded or replayed ceremonies grant no session.

Enrollment and removal increment the account session epoch and access revision.
Identity rejects stale sessions immediately. Gateway revocation follows its
existing bounded-refresh, fail-closed feed; already admitted requests are not
retroactively cancelled. Enable the global required policy before relying on
MFA protection against older password-only sessions during feed propagation.
The BFF replaces the current opaque session after successful verification and
keeps bearer tokens server-side. Other sessions are not centrally inventoried.

Password rotation and verified-mailbox changes require fresh MFA for enrolled
accounts. Forgotten-password recovery replaces the password and invalidates old
epochs but preserves every MFA factor and the enrollment requirement. It grants
no MFA assurance. The next login still requires an enrolled factor. There is no
email-only, administrator, or last-factor removal bypass. Loss of all factors
requires the independently verified recovery policy tracked in #1283; do not remove rows
or clear `mfa_enabled` as an operational workaround.

Successful registration, login verification, step-up and removal use the existing
forced-RLS identity DecisionLog journal. Audit includes method, verification time,
epoch and a hash identifying the affected public factor, not response bodies or
challenge secrets. Audit failure rolls back factor, counter and consumption
changes. Gateway denials use the existing authorization audit path.

## Verification

With a disposable NOSUPERUSER Postgres role, run:

```sh
TEST_POSTGRES_URL=... go test -p 1 ./internal/identity ./services/identity/internal/server -run TestMFA -count=1
go test -p 1 ./pkg/auth ./services/api-gateway/cmd/api-gateway ./services/api-gateway/internal/authz ./services/api-gateway/internal/middleware ./services/web-bff/internal/server
```

The browser test additionally requires `TEST_MFA_BROWSER_SCRIPT` pointing to
`kanz-web/e2e/mfa.cjs`, `TEST_MFA_PLAYWRIGHT` to an installed Playwright module,
`TEST_MFA_NODE`, `TEST_MFA_BFF` to the built BFF executable, and
`TEST_MFA_STATIC_DIR` to the built web distribution. It starts real identity/BFF
HTTP services against Postgres and drives Chromium's virtual UV authenticator:
enrollment, MFA login, refusal before enrollment, step-up replay refusal, spare
enrollment, factor removal and final-factor protection. Synthetic keys test the
real verifier; they do not certify physical authenticator hardware. Linux CI
provides race-detector coverage.
