# Filing records and FACTs

The five `/v1/filings/{frtb,formpf,aifmd,tcfd,sfdr}` routes retain the existing
`File*` computations and `filing.Report.Canonical`/decimal rendering. Requests
must now supply `portfolio_id` and `as_of`, and the gateway principal must belong
to this deployment's configured `REGULATORY_TENANT` before lookup or signing.
The principal headers are trusted only behind the gateway-only NetworkPolicy.
No headers or authentication material enter the input manifest.

Request inputs are **filing-request/unverified observations**. These routes do
not establish vendor reference-data authority, dataset approval, financial
admission authority, human approval or submission to a supervisor. A supplied
`as_of` is an effective computation date. `recorded_at` is sampled during the
recording transaction; it is neither an exact commit timestamp nor evidence
that supplied future-dated data was already known at that future date.

Form PF/AIFMD monetary inputs are exact decimal strings. Climate holding market
value and carbon revenue/EVIC are JSON number tokens checked through
`dec.ParseProtoExact` before the existing float analytic edge; their exact
source values are retained alongside the typed computation inputs. A missing
revenue/EVIC stays missing coverage. ESG is either absent or has all four
explicit pillars. Each carbon scope must be explicit. Unknown fields, repeated
case-folded keys, null values, malformed/trailing JSON and unrepresentable money
are refused. Bounds are 4 MiB request, 512 KiB manifest, 768 KiB context body,
2,048 holdings, 24 JSON levels and 50,000 value nodes; the smaller binding bound
may refuse a request below another limit.

One PostgreSQL transaction commits:

1. The original canonical filing/signature.
2. A separate versioned context-chain record binding tenant, portfolio, typed
   inputs, exact source money, actual method/scenario provenance, distinct
   UUIDv7 domain IDs, and the exact original canonical bytes/signature.
3. The immutable artifact/response, publication IDs and canonical outbox rows.

The context preimage excludes only its resulting context hash. Its global chain
record is the explicitly versioned full-preimage byte length plus SHA-256 digest;
the complete context stays in the FORCE-RLS artifact and is revalidated before
publication or retry return. The existing original canonical report links remain
global/non-RLS: this change does not claim confidentiality of those original
chain rows across database credentials. `chain` retains
the original audit-chain signature; `hash` retains the original plain SHA-256
content digest. Neither becomes a cryptographic signer identity or approval.
Both modes require `REGULATORY_DATABASE_URL` and migrations `0001`/`0002`.
The old ephemeral-chain option cannot disable durable recording.

The semantic key binds tenant/portfolio/framework/effective time/input digest.
An identical retry loads its committed response and IDs without recomputing or
signing. The local typed outbox publisher restores envelope identity from those
IDs, verifies the payload against its immutable bound artifact and publishes
through the canonical producer. HTTP success follows a complete flush. Broker
failure returns 503 with the artifact retained; retry/restart drains it. A lost
publish acknowledgement can redeliver the same persisted identity. Broker
dedup is time-bounded; downstream consumers still require idempotency.

Instrument ESG/carbon records use `risk.sustainability.observed`; their actual
portfolio aggregate uses `risk.sustainability.aggregated`; the signed artifact
uses `compliance.filing.recorded`. The existing RISK/COMPLIANCE streams carry
these prefixes. The single archiver, both Kafka topic tables, audit, lake and DR
replay retain their history (`delete`/30 days in Kafka, not compaction).
The system/tenant account bridge preserves the envelope tenant. A system
archiver must not receive an unprefixed tenant envelope.

The shipped system deployment uses its existing regulatory SVID and one scoped
database/broker tenant. `kanz-tenantgen -service regulatory -tenant <tenant>` is
an explicit source render only, excluded from default tenant workloads. It
derives the same ServiceAccount/SPIFFE name admitted by the corresponding
publisher ACL. No rendered tenant workload is committed here. Tenant DB/Vault
authorization, API routing, deployment and operator verification remain separate
activation prerequisites; rendering does not provision credentials or prove a
deployed tenant filing API. Apply the new migration and compatible grants before
upgrading runtime; existing applied migrations are unchanged.

The `Publish` tests separately cover real broker payload readback and optional
real PostgreSQL transaction/restart/RLS controls. PostgreSQL cases require
`TEST_POSTGRES_URL` with a disposable NOSUPERUSER/NOBYPASSRLS role and use an
owned schema. HTTP model tests and queue-boundary models are not durable or
broker acceptance. Source preparation, isolated test execution, CI, merged-main
acceptance and production activation remain distinct evidence.
