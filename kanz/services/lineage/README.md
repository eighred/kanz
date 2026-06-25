# lineage service (LIN-01)

Automated, queryable data lineage + PII governance over the event backbone. The
lineage substrate already rides every event (correlation_id / causation_id /
source / payload_schema_ref, EVT-17c); this service *aggregates* it into a
dataset-level graph, emits it as OpenLineage, and governs access to sensitive
lineage.

It is the dataset-level sibling of the audit service (AUDIT-01): audit
reconstructs the *event* causal chain (which event_id caused which); lineage
answers "where did this *number* come from" at the *dataset* granularity.

## Layers

| Task | Package | What |
|---|---|---|
| LIN-01a | `internal/openlineage`, `internal/harvest`, `internal/graph` | harvest envelope lineage into an OpenLineage emitter + an in-memory dataset graph |
| LIN-01b | `kanz/infra/catalog/` | DataHub catalog: datasets, schemas (EVT-16 registry), ownership (CODEOWNERS), classification |
| LIN-01c | `internal/governance` | PII tagging + deny-by-default access via the AUTH-01b authorizer, every PII access logged (AUTH-01d), MT-01d tenant scope |
| LIN-01d | `internal/query` | "where did this come from" provenance over the graph, governed |

## Dataset identity

`kanz.{domain}` / `{MessageType}` — derived from the envelope domain + the
payload schema ref's message name, the *same* identity the lake-sink tables
(LAKE-01a) and the catalog (LIN-01b) use.

## API

```
GET /v1/catalog/datasets                          # catalog listing (metadata)
GET /v1/lineage/event/{event_id}                  # provenance for an event (governed)
GET /v1/lineage/dataset/{namespace}/{name}        # provenance for a dataset (governed)
GET /healthz /readyz /metrics
```

Provenance over a PII dataset is deny-by-default (403 unless the principal holds
`lineage.pii.read`); upstream PII a requester can't read is returned redacted.
Every PII access decision — allow or deny — is logged into the observation stream
(feeds AUDIT-01). The principal comes from the AUTH-01a OIDC identity on the
request context (populated by the gateway / auth middleware).

## Run

```sh
LINEAGE_NATS_URL=nats://localhost:4222 \
LINEAGE_POLICY_FILE=config/policy.example.json \
LINEAGE_GOVERNANCE_FILE=config/governance.example.json \
go run ./services/lineage/cmd/lineage
```

Config (env): `LINEAGE_LISTEN` (`:8086`), `LINEAGE_NATS_URL` (unset ⇒ read API
only), `LINEAGE_SUBJECTS` (default `>` — lineage is comprehensive), `LINEAGE_POLICY_FILE`
(unset ⇒ all PII access denied), `LINEAGE_GOVERNANCE_FILE` (unset ⇒ nothing PII),
`LINEAGE_OPENLINEAGE_URL` (unset ⇒ log emitter), `LINEAGE_CONSUMER_GROUP`,
`LINEAGE_SOURCE`, `LINEAGE_OTLP_ENDPOINT`.

## Tests (LIN-01e)

`go test ./services/lineage/...` — any output's full upstream lineage is
queryable (a harvested cascade resolves the leaf's transitive provenance); PII
access is governed (deny-by-default without the role) + logged (the recorder
captures every PII decision); upstream PII is redacted for the unauthorized;
provenance over a PII target is forbidden + logged.
