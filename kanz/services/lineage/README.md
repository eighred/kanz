# Lineage service (LIN-01)

The lineage service projects envelope causality into a tenant-owned dataset graph,
serves governed catalog/provenance reads and emits attributed OpenLineage records.
It remains parked under #1028. Component evidence does not admit a workload,
deploy a catalog or certify the production network boundary.

| Responsibility | Owner |
|---|---|
| Validated envelope ingestion and atomic projection | `internal/harvest`, `internal/graph` |
| Canonical OpenLineage projection | `internal/openlineage` |
| Same-owner public/PII governance | `internal/governance`, `internal/query` |
| Authenticated HTTP reads | `internal/server` |
| Optional stewardship tooling decision | #1405; `kanz/infra/catalog/` |
| Lakekeeper/Iceberg catalog and retained publication | #1540, independently |

## Identity and ownership

Canonical namespace/name remain `kanz.{domain}` / `{MessageType}`. Storage keys
are `(owner, DatasetID)` and `(owner, event_id)`. Ownership never rewrites the
URN, schema namespace or message name. It comes only from the live envelope
after canonical `bus.Validate`. `LINEAGE_TENANT` remains the authorization
recorder's fallback and cannot assign graph ownership.

Envelope validation checks required fields; it does not cryptographically bind
a broker account to payload tenancy. Account/SVID, subject/import mapping and
actual delivery need separate transport evidence.

Readers select their principal's tenant scope before catalog, existence,
upstream or coverage lookup. Governor compares the record's independently
stored owner before classification, public allowance or RBAC. Unknown ownership
and mismatches deny even with wildcard roles. The gateway remains the sole
end-user identity authority; `auth.RequirePrincipal` and its NetworkPolicy
reachability premise remain necessary (#232).

## API and scoped coverage

```
GET /v1/catalog/datasets
GET /v1/lineage/event/{event_id}
GET /v1/lineage/dataset/{namespace}/{name}
GET /healthz /readyz /metrics
```

Catalog metadata retains its existing same-owner policy and empty-array shape.
PII target provenance requires `lineage.pii.read`. Same-owner upstream PII
remains redacted for unauthorized readers. PII decisions still use the audited
authorizer/observation recorder. Foreign nodes are omitted entirely.

Foreign-only and absent identifiers receive the same UNKNOWN answer and scoped
coverage. Another owner's counts or retention floor cannot be substituted.

| HTTP | `status` | Meaning |
|---|---|---|
| 200 | `retained` | This scope retains an answer; partial causality remains explicit. |
| 410 | `unknown` | The bounded/restarted scoped index cannot establish existence or absence. |
| 404 | `never_observed` | An explicitly declared complete finite history for this owner, with no scoped eviction, establishes absence. |

Production wiring never declares complete history. A durable consumer resumes
at last acknowledgement after restart. An untyped error without captured scoped
coverage refuses rather than fabricating zero coverage. Operator metrics use
separate global statistics of admitted projection state.

## Atomic admission and retained history

Limits are GLOBAL across owners: event index, datasets, edges and retained string
bytes. Topology is retained; capacity refuses new admission without silently
destroying prior nodes or edges. One lock preflights ownership, conflicts,
history floor, new nodes/edges, metadata replacement, eviction credits, string
costs and integer overflow before any retained state changes. A refusal leaves
the graph, index, ring, metadata, counts and coverage unchanged.

Admitted strings are cloned so short inputs cannot retain huge backing buffers.
Dataset keys own canonical owner/namespace/name strings. Event/map/ring
references share those admitted keys. Latest metadata and frozen event
projections own separately charged strings, including `LastEventID`.
Replacement and eviction release their charges. Fixed record/map/ring overhead
is bounded separately by entry counts; string bytes are not an RSS claim.
Reads create no persistent owner/index state.

A retained duplicate returns its original owned snapshot without recounting or
a new parent lookup. Conflicting retained lineage semantics refuse. Emission
uses that same-lock snapshot; a retry cannot acquire a parent that arrived
later. Missing parents contribute to scoped `unlinked_causes` rather than
inventing edges.

Each owner retains its maximum evicted event timestamp. Unknown observations at
or below that floor refuse as retained-history UNKNOWN, including legitimate
late facts. Identical evicted bytes cannot be silently recounted. Recovery
requires an owned replay/rebaseline decision. A reused evicted identity with a
changed later timestamp cannot be recognized as historical conflict; no
unlimited identity ledger is claimed.

Latest schema/domain metadata uses deterministic `(event_time, event_id)` order.
Late admitted facts cannot roll it back. Dataset ordering compares namespace
then name, preserving distinct tuples whose display strings happen to match.

## Delivery and readiness

One logical consumer handles explicit subject scopes. Every scope must complete
its actual `SubscribeReady` callback before readiness becomes true.
Empty/duplicate scopes, failures and unexpected early completion refuse. A
sticky cancellation/failure gate clears readiness before joining all workers;
late callbacks cannot reopen it. This does not claim zero-latency scheduling.

Default in-handler attempts remain one. Capacity and retained-history UNKNOWN
stay transient. DLQ publication succeeds before the original delivery is
acknowledged. Failed publication releases the dedup claim and leaves the broker
delivery unacknowledged. Approved budget/rebaseline changes and canonical
redrive are separate operator actions.

`LINEAGE_SUBJECTS` retains legacy default `>`. The shared bus deliberately
refuses a subscription spanning multiple streams (#698). A provisioned topology
needs explicit actual stream subject scopes; the default is not evidence of
comprehensive ingestion.

## Emitter boundary

The emitter receives an `OwnedRunEvent` built from stored admission. Logs include
that owner while canonical wire identity stays unchanged. This does not make
an unscoped remote catalog tenant-isolated.

A nonempty legacy `LINEAGE_OPENLINEAGE_URL` refuses startup. Direct unscoped
HTTP construction/emission also refuses before network I/O. No header, namespace
prefix or trust flag replaces a reviewed scoped sink/storage contract. #1405
retains that decision and the independent DataHub experiment hold. These tests
do not establish its eight experimental cases.

## Configuration and rollout

`LINEAGE_EVENT_INDEX_MAX` retains default 250,000 and refuses nonpositive values.
The following additional GLOBAL limits have **no production defaults** and must
be explicitly chosen before image adoption, including HTTP-only startup:

- `LINEAGE_DATASET_MAX`
- `LINEAGE_EDGE_MAX`
- `LINEAGE_STRING_BYTES_MAX`

Existing listen/source/consumer-group/policy/governance/OTLP/SPIFFE settings keep
their roles. Missing policy still denies PII; a governance bundle and real
network restrictions remain deployment obligations. Old image configuration
cannot be assumed compatible with the new required budgets. No production
manifest, resource value or deployment authority is supplied here.

## Verification boundaries

Service tests retain #244's million-event cap, scoped misses, eviction horizon,
unlinked causes and PII/audit/principal reconstruction assertions. New controls
cover ownership substitution, equal-role isolation, atomic global bounds,
retained strings, frozen retries, historical UNKNOWN refusal, tuple/metadata
ordering and readiness/join failures. Copilot treats 410 as unresolved, preserving
its bounded citation fallback.

Equal-ID storage/HTTP controls are explicitly adversarial. Canonical live FACTs
have globally unique UUIDv7 identities. The broker component control uses
distinct UUIDv7 FACTs from system OMS and oms-acme SVIDs, actual account
streams/imports, one consumer and production Harvester/query/HTTP code.
The existing admitted audit reader is a fixture principal; this does not admit
parked lineage. Stream configuration and owned frame retention must be observed,
with no scratch-stream substitution.

Ordinary CI, mTLS component/account-bridge proof and deployed authorization are
different claims. No JVM experiment, production DataHub capability, capital
authority or #1753/POS qualification is supplied by these tests.
