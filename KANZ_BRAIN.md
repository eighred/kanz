# KANZ BRAIN

> Architectural memory — durable decisions future engineers must know, and the "why" behind them. NOT a changelog: no feature-completion notes, implementation minutiae, routine wiring, or bug fixes (those live in git + `KANZ_TASKS.md`). The codebase is always the ground truth. Last reconciled 2026-07-03.

## System shape

- **Modular monolith core (Go) + process-isolated edges.** One Go module rooted at `kanz/` (`github.com/kanz-eng/kanz`, Go 1.23); services are `package main` under `services/<name>/cmd/<name>/`. Market ingestion, AI inference, and batch compute run as isolated edge processes — never folded into one undifferentiated backend. Shared code starts per-service in `internal/`, promoted to top-level `kanz/internal/` or `kanz/pkg/` only when a second consumer appears. Stdlib `net/http` + `log/slog` by default.
- **Prediction layer is hybrid:** async streaming path (default) + a sync gRPC path for interactive calls, with a circuit breaker + degraded fallback. Interactive target <50ms (async path unbounded); the p50/p99 split is a LATENCY-01 concern.
- **Everything is event-driven.** All state changes flow through events; there is no polling architecture.

## Event platform

- **Kafka + NATS hybrid, by role.** NATS JetStream is the live spine (short-retention hot tier — one stream per domain, tiered retention, 2m broker-side dedup keyed on `idempotency_key`). Kafka is the durable log of record (KRaft, RF3/min-ISR2, explicit topic provisioning, tiered retention with `platform.model` infinite, compacted `.snapshot`/`platform.config`, a paired `dlq.{name}` per delete-policy topic). **NATS is not a system of record** — DR rebuilds it from the Kafka log rather than replicating it.
- **Envelope vs payload asymmetry.** The envelope is additive-only forever — a constitution, never a breaking change, no `envelope_version` bump for new sibling messages. Payload schemas may break, via a new `vN` package + dual-write migration. Payload backward-compat must hold for the full Kafka retention window, because replay reads old logs with current code.
- **Two version fields, not redundant:** envelope `schema_version` (major, bumps only on a break, == package `vN`) vs the registry `payload_schema_ref` version (bumps on every change including additive).
- **Wire framing:** every bus body is a serialized `envelope.v1.EventFrame{ envelope = 1; bytes payload = 2 }`. Sibling layout (not payload-nested-in-envelope) so payload-blind tooling — registry, observability, replay, routing — decodes the envelope without the payload schema.
- **`common.v1` shared value types** (Decimal, Money, LogPosition) exist to prevent cross-domain proto dependencies (e.g. risk importing market). **`Decimal` is exact base-10; `double` is banned for money/prices/sizes** (fine for metrics + statistical scores). Exact money/qty never touch float anywhere — serialized losslessly as `*big.Rat` RatString.
- **Command layer:** `command.v1` is generic (a `CommandMetadata` embedded as field 1 by every concrete command; no `Any` wrapper, so each command keeps its own `payload_schema_ref`). `CommandOutcome` is the single universal outcome-FACT, linked to its command by envelope `causation_id`.
- **Codegen: generated code is never committed to `kanz-schemas`** (gitignored). A tag-triggered `schema-release.yml` runs `buf generate` and publishes each language SDK as its own artifact — Go → companion module repo `kanz-schemas-go`, Python → package index, TS → npm `@kanz-eng/kanz-schemas`. `.proto` carry no `option go_package`; buf managed mode supplies it. A CI guard rejects any tracked generated file.
- **Load-bearing dev seam:** `replace github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go` (+ Py/TS equivalents) until the first published release tag. This is the marker DEBT-01/PARITY-07 strips; it is correct and load-bearing until a publish target exists.
- **Schema registry:** the registry assigns versions (not the caller); a `(schema_id, version)` row is immutable once published. Ref wire form `<schema-id>:<version>`; resolve returns raw descriptor bytes (not base64-JSON) with immutable cache headers; an in-memory backend ships beside Postgres so the same suite runs without a DB.
- **Bus client:** a single `Client` interface (`Publish`/`Subscribe`/`Close`) over both `NATSClient` and `KafkaClient`, so envelope stamping / lineage / dedup / DLQ layer once. Libs: `nats.go` + `segmentio/kafka-go` — kafka-go chosen cgo-free because the container images are distroless. The bus client manages neither streams nor topics (those live in `infra/{nats,kafka}/`).
- **Lineage rides the language's implicit-context mechanism, not parameters:** Go `context.Context`, Python `contextvars`, TS `AsyncLocalStorage`. Load-bearing subtlety: the `causation_id` stashed for the *next* event is the *current* event's `event_id` — that is how the chain links. Empty values never clobber an outer context.
- **Dedup is a performance optimization, not a correctness primitive.** Two reinforcing layers (broker `Nats-Msg-Id` window + a per-instance consumer window); it is per-instance and in-memory. Cross-pod correctness rests on **idempotent handlers**, always. Retry + DLQ live inside the consumer's dispatch; DLQ subject = `dlq.<original>`, with pass-through headers so investigators can re-parse.
- **Cross-language clients (Go/Py/TS) must match on the wire:** EventFrame framing, `Nats-Msg-Id` broker-dedup semantics, `idempotency_key` dedup, `dlq.<subject>` + `Kanz-DLQ-*` headers, per-(event_type, partition_key) monotonic `producer_sequence`, and the same lineage chain — each expressed in its language's idiom.
- **Replay isolation is source-side first.** The Kafka replay reader uses **no consumer-group** (direct partition reads mutate no live consumer's offsets) — the primitive that makes replay safe even before namespacing. Republished events land under `replay.{runID}.*` + a `replay-{runID}` group and are stamped `QUALITY_FLAG_REPLAYED`. Validation is bidirectional: live validators hard-reject REPLAYED (route to DLQ, loud); replay-scoped validators require it. Stamping is owned by the replay Pipeline, not the Producer (which would clobber `event_id`/`producer_sequence`).

## Data integrity

- **Detect-then-emit split.** Detectors (gap / staleness / watermark-late / drift / NATS↔Kafka reconciliation) are pure in-memory classifiers. The envelope owner stamps the matching `quality_flags`; payload-blind tooling reads the flag without re-detecting or parsing the payload. Late data is an envelope `QualityFlag`, not a `DataQualityEvent`; the `DataQualityEvent` oneof is gap/staleness/drift only. **Flags live in the integrity layer, never in `bus.Producer`** — the generic transport must not depend on integrity (no inversion).

## Cross-cutting engineering patterns

_These are the recurring disciplines the whole codebase is built on — the most reusable knowledge here._

- **Seam-the-vendor.** Every external dependency (vendor SDK, protocol, broker/DB binding) sits behind a delivered, narrow interface; the concrete binding wires at the composition root (`cmd/*`). The vendor SDK is itself the seam — the core module carries no vendor import.
- **Build-tag composition-root split.** When the composition root is *in* the module (e.g. `cmd/copilot`), importing a vendor SDK there still bloats `go.mod` — so split `//go:build` files (`model_stub.go` default vs `model_anthropic.go`; `redisadapter` behind `-tags redis`). The default build stays dependency-free; prod builds with the tag.
- **Degrade-not-fabricate.** On an external failure (Redis outage, lineage 403/404, garbage calibration inputs) degrade to the safe/no-op answer — never invent a result. Authorization and promotion gates are deny-by-default.
- **Generated-not-committed decoder seam.** For schemas not yet in `gen/go` (`settlement.v1`, `alternatives.v1`, `wealth.v1`), payload decode/encode is a `Decoder`/`Encoder` seam (default JSON over the Go-native type; the proto binding wires when the SDK generates) — the same stance as the codegen decision, applied to consumers.
- **The in-memory default *is* the test seam.** Every durable `Store` keeps its in-memory implementation beside the Postgres one, with identical fold/replay semantics; DB-backed tests are gated on `TEST_POSTGRES_URL` and skip without a database.
- **Import-boundary discipline, enforced by an arch test.** E.g. `curve`/`volsurface` cannot import `compute`, so the compile-time `Provider` assertions live in `compute`; a `factor`→`compute` cycle is avoided with closures.

## Persistence & durability

- **Event-sourced journals with bitemporal reads** (`effective_time`, `knowledge_time`, entry_id), idempotent append on the entry PK. Postgres behind every Store, RLS-scoped by the `app.tenant_id` GUC, deny-by-default tenant isolation. **Snapshot + tail-replay == full replay** is the determinism contract, enforced per-store (the Go internal-package boundary forbids a central cross-service importer). PITR + cross-region warm standbys via CloudNativePG.

## Security & multi-tenancy

- **Zero-trust.** SPIFFE/SPIRE workload identity + an mTLS mesh; secrets via Vault + CSI mounts, KMS-rooted. **The gateway is the sole identity authority** — it populates the canonical `auth.Principal` at the edge and propagates it over the mesh as trusted `X-Kanz-Principal-*` headers, so downstream services never re-authenticate an end user.
- **Authorization is deny-by-default** (OIDC/JWKS authN + RBAC/ABAC authZ), with a forged-issuer guard; every allow/deny decision is published to the bus so the AUDIT-01 projection is the system of record.
- **Multi-tenancy** is enforced at every layer: envelope `tenant_id` propagated on the bus, broker isolation (NATS accounts / Kafka topic-prefix ACLs), Postgres RLS, and per-tenant gateway quotas/admission.

## Analytics

- **Calibration is deny-on-arbitrage / fail-loud.** SVI fits gate on Gatheral butterfly + cross-expiry calendar monotonicity; curve/CDS bootstraps solve every quote to par; alternatives OLS excludes alpha from the mapping; a failed calibration leaves the prior point-in-time version serving. **Regulatory parameters are data** — the licensed ISDA/BCBS/NGFS tables load at the composition root, not baked into code.
- **Independent validation gate (SR 11-7) is deny-by-default:** missing / failed / expired ⇒ denied. The validation package imports no analytic code — benchmark cases are data, each analytic computes its own worked example, and reports are signed (failures signed as audit evidence).
- **The audit hash-chain primitives are shared, not audit-service-private.** `chain` (payload-agnostic tamper-evidence: `H(prev‖canonical)`) and `signer.ChainSigner` (the one-method `Sign` seam realizing `regulatory.Signer`/`sustainability.Signer` as a chain position) live at `internal/audit/`, not `services/audit/internal/` — promoted when the `regulatory` filing service became the second consumer (the "second consumer ⇒ promote to top-level internal" rule). Any service's composition root can now inject the ChainSigner into report building; the durable link append stays a `LinkSink` wired per-composition-root.

## Anti-decisions

- No microservices sprawl before the domain is understood.
- No single undifferentiated backend — market ingestion, inference, and batch compute stay process-isolated.
- No polling-based architecture.
- No silent failure handling — validators and gates fail loudly (to the DLQ / a denial), so misconfiguration surfaces on the first event.
- No committed generated code; no vendor dependency in the default build; no parallel/competing implementation of a delivered seam.

## Open questions

- Is the <50ms interactive-inference target measured at p50 or p99? (LATENCY-01.)
