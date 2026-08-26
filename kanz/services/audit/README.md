# audit service (AUDIT-01)

Tamper-evident reconstruction of every decision, command, outcome, and
data-quality event. Consumes the event backbone and materializes it into an
append-only, hash-chained, queryable audit log, then serves lineage + reporting.

The lineage substrate already exists on every event (correlation_id /
causation_id, DecisionLog, CommandOutcome, FACT-grade DataQualityEvent,
AUTH-01d authz DecisionLog) — this service *consolidates* it and makes it
tamper-evident.

## Layers

| Task | Package | What |
|---|---|---|
| AUDIT-01a | `internal/audit` | append-only projection: a bus consumer classifies + materializes every event into the `Store` (Memory + Postgres) |
| AUDIT-01b | `internal/chain` | hash chain over the log + DB-layer WORM (`migrations/`); see `internal/chain/README.md` |
| AUDIT-01c | `internal/lineage` | given an event_id, walk causation_id to the root + assemble the full correlation tree |
| AUDIT-01d | `internal/report` | configurable report templates (verified attestation per report) + retention/legal-hold policy |

## How events are classified

The projector records **every** delivered event (a dropped event is an audit
gap), enriching the ones it recognizes via ordered classifiers
(`internal/audit/classify.go`):

1. `platform.authz.decision` (AUTH-01d) → **authz_decision** (DecisionLog)
2. OBSERVATION on the `data` domain → **data_quality** (DataQualityEvent)
3. FACT with `.outcome` event_type → **command_outcome** (CommandOutcome)
4. COMMAND class → **command**
5. OBSERVATION whose `payload_schema_ref` names DecisionLog → **decision**
6. anything else → **event** (generic, envelope-level summary)

Classifier #5 keys on `payload_schema_ref`, not a blind decode: a
`MetricObservation` is wire-compatible with `DecisionLog`, so decoding alone
would misread a metric as a decision.

## API

```
GET /v1/audit/events?correlation=&tenant=&kind=&event_type=&since=&until=&limit=
GET /v1/audit/events/{event_id}
GET /v1/audit/lineage/{event_id}     # AUDIT-01c reconstruction
GET /v1/audit/verify                 # AUDIT-01b attestation (409 if tampered)
GET /v1/audit/reports/{template}?format=json|csv   # AUDIT-01d (authz-decisions, command-outcomes, data-quality, full-log)
GET /healthz /readyz /metrics
```

## Run

```sh
# Durable WORM Postgres log (production): apply the migration, set the DSN.
psql "$AUDIT_DATABASE_URL" -f migrations/0001_audit_log.sql
AUDIT_DATABASE_URL=... AUDIT_NATS_URL=nats://... go run ./cmd/audit

# Local/dev: in-memory store. It is DISCARDED on restart, so the service refuses
# to start on it unless you say out loud that you accept an ephemeral compliance
# record — and then warns and reports kanz_audit_log_durable=0 for as long as it runs.
AUDIT_ALLOW_EPHEMERAL_LOG=true AUDIT_NATS_URL=nats://localhost:4222 go run ./cmd/audit
```

Config (env, `_FILE` secret variant for the DSN): `AUDIT_LISTEN` (`:8083`),
`AUDIT_NATS_URL`, `AUDIT_SUBJECTS` (default `>` — audit everything),
`AUDIT_DATABASE_URL`, `AUDIT_ALLOW_EPHEMERAL_LOG` (default false — see above),
`AUDIT_OTLP_ENDPOINT`.

## Tests (AUDIT-01e)

`go test ./services/audit/...` — the three board behaviors run end-to-end in
`internal/audit/e2e_test.go` over the real projector + store + lineage + report +
chain: reconstruct a decision to its inputs in <1min, a tamper attempt is caught
by chain verification, and a sample regulatory report is generated end-to-end.

## Compliance runbooks

Two process wrappers around the evidence this service produces. Neither computes
anything: the artifacts are machine-generated and signed, and these pages drive
the human loop that certification requires.

### SOC 2 Type II continuous evidence (PARITY-06d)

Severity **ticket**.


A SOC 2 **Type II** audit asks whether controls *operated throughout the period*,
not merely that they exist. The AUDIT-01 observation stream is an append-only,
hash-chained WORM log of exactly the operating events auditors sample — so the
evidence collects itself. The `soc2` package
(`services/audit/internal/soc2`) maps the Trust Services Criteria to the record
`Kind`s that evidence them and produces a per-control evidence bundle over any
window.

#### Control mapping (what the stream evidences)

| Control | Category | Evidenced by (audit `Kind`) | Meaning |
|---|---|---|---|
| CC6.1 | Security | `authz_decision` | Every logical-access decision is recorded (AUTH-01d) |
| CC7.2 | Security | `data_quality` | Anomaly monitoring — DATA-05 quality events are detected + logged |
| CC8.1 | Security | `command`, `command_outcome` | Changes are authorized and their outcomes recorded |
| PI1.1 | Processing Integrity | `decision`, `command_outcome` | Processing is complete + accurate — model/strategy decisions logged |

These are the controls the stream can HONESTLY evidence. Controls that need
out-of-band artifacts — HR onboarding/offboarding, vendor security reviews,
physical/data-center access, pen-test cadence — are collected by the ops process
below, NOT fabricated from the stream. The mapping is data (`DefaultControls`); a
deployment extends it for its own control matrix.

#### Pull the evidence bundle

```sh
# A calendar-quarter window; `to` defaults to now. 409 ⇒ a mapped control had
# insufficient evidence in the window (a Type II exception a monitor alerts on).
curl -fsS "$AUDIT/v1/soc2/evidence?from=2026-04-01T00:00:00Z&to=2026-07-01T00:00:00Z" | jq .
```

The bundle lists, per control, the supporting `count`, sample `event_id`s (the
records an auditor pulls to verify the chain via `/v1/audit/events/{id}` +
`/v1/audit/verify`), and `satisfied`; the top-level `gaps` array names any control
below its `min_per_window`.

#### Continuous-evidence pipeline (the running Type II file)

`soc2.CollectFromStore` is the collection primitive. Run it on a schedule so the
evidence file is continuous, not reconstructed at audit time:

- **Nightly**: a cron/Job calls `/v1/soc2/evidence` for the trailing 24h and
  archives the JSON to the immutable evidence bucket (same retention plane as the
  DR backups). A non-200 pages the compliance on-call — a control that stopped
  producing evidence is a control that stopped operating.
- **Per-quarter**: the audit-period bundle is the artifact handed to the external
  auditor, alongside a `/v1/audit/verify` attestation proving the chain of the
  sampled records is intact (tamper-evidence, AUDIT-01b).

#### Out-of-band controls (ops-owned, not stream-derived)

Collect these into the same evidence bucket on their own cadence, cross-referenced
by control id: access reviews (quarterly), vendor reviews (annual), pen-test +
remediation (annual, see `PARITY-06f`), change-approval records not routed through
the command bus, and BC/DR test results (the DR drill’s quarterly output (`infra/dr/README.md`) — RTO/
RPO evidence for the Availability criteria).

#### Confirm the pipeline is healthy

```sh
# The evidence endpoint is reachable and the trailing-24h window is satisfied.
curl -fsS -o /dev/null -w '%{http_code}\n' "$AUDIT/v1/soc2/evidence?from=$(date -u -d '24 hours ago' +%FT%TZ)"
# 200 = all mapped controls produced evidence in the last day; 409 = a gap to chase.
```

### External audit & regulator engagement cycle (PARITY-06f)

Severity **ticket**.


The certification gate: turn the platform's internally-generated evidence (signed
filings, the tamper-evident audit chain, the SOC 2 continuous-evidence bundle)
into a completed third-party review and a first-client sign-off. This is the
process wrapper around the machine-produced artifacts of PARITY-06a–e — it does
not compute anything, it drives the human loop that certification requires.

#### Engagement calendar

| Engagement | Cadence | Artifacts handed over | Owner |
|---|---|---|---|
| SOC 2 Type II audit | Annual (with a 6–12 month observation window) | `/v1/soc2/evidence` quarterly bundles + `/v1/audit/verify` attestations + out-of-band controls | Compliance |
| Penetration test | Annual + on major arch change | Scope, findings, remediation evidence | Security |
| Regulator filing review | Per filing cycle (FRTB/Form PF/AIFMD/TCFD/SFDR) | Signed `RegReport`s + the reconciliation worksheets (`FRTBResult`, `AIFMDResult`) | Regulatory |
| BC/DR attestation | Quarterly | DR drill measured RPO/RTO (`infra/dr/README.md`) | Platform |

Every artifact above is machine-generated and signed; the engagement is the act
of packaging, submitting, and closing findings — not re-deriving numbers.

#### Findings remediation backlog

Every finding — from the auditor, the pen-tester, or the regulator — enters ONE
register — a GitHub Issue labelled `compliance`, see below. The register
is the single source of truth for "what is open against us."

Severity → SLA (time-to-remediate, from acceptance):

| Severity | SLA | Examples |
|---|---|---|
| Critical | 7 days | A cross-tenant leak, a broken audit chain, an unsigned filing path |
| High | 30 days | A control with recurring evidence gaps, a missing reconciliation |
| Medium | 90 days | A hardening gap, an incomplete runbook |
| Low | Next release | Cosmetic, documentation |

Lifecycle: `open → accepted → in-remediation → fixed → verified → closed`. A
finding is **verified** only by re-running the evidence that would have caught it
(the same discipline as a GameDay verify abort): a cross-tenant finding is closed
by the tenant-onboarding `verify` step passing (`infra/tenancy/README.md`); a filing finding by the
`Reconcile` gate passing against the regulator's worked example; a chain finding
by `/v1/audit/verify` returning 200. No finding closes on assertion alone.

#### First-client UAT sign-off

The gate that everything above is true and stable for one real tenant. Run
against a production-shaped environment with the client's own (or representative)
data:

- [ ] Tenant provisioned per `infra/tenancy/README.md`; the isolation `verify` step passed.
- [ ] Each capability the client contracted for returns correct results on their book (risk, IBOR NAV incl. multi-currency, the relevant regulatory filing).
- [ ] A signed regulatory filing reconciles against the client's independently-computed benchmark within tolerance (`Report.Reconcile`).
- [ ] The audit chain verifies (`/v1/audit/verify` → 200) over the UAT window, and the SOC 2 bundle for the window is `satisfied`.
- [ ] A DR drill (or the last quarterly drill's evidence) shows RPO/RTO within objective.
- [ ] Every finding raised during UAT is at `verified`/`closed` or has an accepted, SLA'd remediation plan the client signed off on.
- [ ] Sign-off recorded (signed) as an audit event — the UAT completion is itself evidence.

A UAT with an open Critical or High finding does **not** sign off; it converts to
a remediation sprint against the register and re-runs the affected checklist items.

#### Closing the loop

Findings feed the backlog (`/forge-tasks` turns accepted findings into board
tasks); remediations land as normal changes gated by CI + the arch test; the fix
is verified by re-running the evidence. The register's "open Critical/High count"
is the headline certification-readiness metric — zero is the bar for go-live.

#### The findings register

Every finding — auditor, pen-tester, regulator, or first-client UAT — is a
**GitHub Issue** on `eighred/kanz`, labelled `compliance`. That is the register:
one issue per finding, its severity as the priority label, its remediation as
the linked PR, and its verification as the evidence re-run that closes it.

It was a `docs/compliance/findings-register.md` table until 2026-08-26, holding
exactly one row — an `_example_`. A markdown table is a second tracker that goes
stale beside the one people actually use, and the lifecycle it described
(`open → accepted → in-remediation → fixed → verified → closed`) is what issue
state and labels already carry.

**A closed finding's issue is audit evidence — never delete one.** The SLAs from
acceptance are unchanged: Critical 7 days, High 30, Medium 90, Low next release.
The certification-readiness metric is the count of open Critical + High, and the
go-live bar is zero.
