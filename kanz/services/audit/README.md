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
