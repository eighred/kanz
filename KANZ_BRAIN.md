# KANZ BRAIN

> Living context doc. Last reconciled 2026-05-14 against the architecture docs and KANZ_TASKS.md.

## Current System Understanding
- Event system is foundation layer
- Risk engine consumes FACT events
- Go handles infra, Python handles inference

## Key Decisions
- Kafka + NATS hybrid architecture chosen
- Envelope-based event design required
- Causality tracking is mandatory
- Modular monolith core (Go) + isolated edge processes for market ingestion, AI inference, and batch compute
- Prediction layer is hybrid: async streaming path (default) + sync gRPC path for interactive calls, with circuit breaker + degraded fallback
- Envelope vs payload schemas are asymmetric: Envelope is additive-only forever (a constitution); payload schemas may break, via a new `vN` package + dual-write migration (EVT-06)
- Payload schemas hold full compatibility (backward + forward) within a package version; backward compat must hold for the full Kafka retention window because replay reads old logs with current code
- Two payload-version fields, not redundant: envelope `schema_version` (uint32) = major version, bumps only on a breaking change, == package `vN`; `payload_schema_ref` registry version bumps on every change incl. additive
- NATS JetStream provisioned as the live spine (EVT-08): one stream per domain (`{domain}.>`), file storage, R3, `limits` retention; retention tiered — 24h hot domains, 7d platform/data, 1h observability, 30d DLQ; 2m dedup window for broker-side idempotency keyed on envelope `idempotency_key`. Replay streams are per-run, not standing (EVT-20)
- Kafka provisioned as the durable log of record (EVT-09): 3-node KRaft cluster (no ZooKeeper), RF3 / min-ISR2, auto-create disabled — topics provisioned explicitly. Topic = `{domain}.{entity}`; retention tiered — 7d high-volume, 30d most FACT/COMMAND, infinite for `platform.model`; `.snapshot` + `platform.config` compacted; a `dlq.{name}` paired with every delete-policy event topic
- New shared proto package `common.v1` (EVT-10): foundational value types every payload schema reuses, owned by event-platform + architecture, evolves under envelope-grade additive-only discipline — prevents cross-domain proto dependencies (e.g. risk schemas importing market). First member: `Decimal` (exact base-10, `coefficient × 10^exponent`); `double` is banned for prices/sizes/money
- Market payload design (EVT-10): `MarketDataEvent` is self-contained (instrument identity + `event_time` inline) so events survive being pulled from a batch; `MarketDataBatch` is per-instrument — all events share one `instrument_id` = envelope `partition_key`, ordered by non-decreasing `event_time`, so per-partition ordering still holds
- Domain/state payload design (EVT-11): `PortfolioState`/`PositionState` are aggregate-level state payloads — portfolio does NOT embed positions (those are their own events); `PortfolioSnapshot` is the STATE_SNAPSHOT and carries a `LogPosition` for gap-free bootstrap; exposures are emitted as a coherent `ExposureSet`, never piecemeal. `common.v1` gained `Money` (Decimal + ISO 4217) and `LogPosition` (durable-log coordinate)
- Command layer design (EVT-12): `command.v1` is the generic command layer — what `envelope/v1` is to events. `CommandMetadata` (issuer, target, valid_until, reason) is embedded as field 1 by every concrete domain command; `command.v1` holds no domain-specific command body and no `Any` wrapper, so each concrete command keeps its own `payload_schema_ref` for clean registry tracking. `CommandOutcome` is the single universal outcome-FACT payload; the command it reports on is identified by envelope `causation_id`, not restated
- Payload timestamp convention (clarified EVT-13): a payload restates a domain timestamp only when events can be bundled under one envelope (batch/snapshot) so a single envelope `event_time` cannot represent them all — `MarketDataEvent.event_time`, `PositionState.as_of`. Standalone, never-bundled events (lifecycle FACTs) omit the timestamp; envelope `event_time` is authoritative. They still carry a principal (`*_by`) because envelope `source` is the emitting service, not the acting principal

## Assumptions
- Interactive (sync) inference path target <50ms — percentile (p50/p99) TBD; async streaming path has no 50ms bound
- Per-partition event ordering preserved (partition_key + producer_sequence); no global ordering guarantee
- All state changes must be event-driven

## Open Questions
- How to handle cross-region event replication?
- Is the <50ms interactive inference target measured at p50 or p99?

## Anti-Decisions (important)
- No microservices sprawl before the domain is understood
- No single undifferentiated backend process — market ingestion, inference, and batch compute must be process-isolated
- No polling-based architecture
- No silent failure handling
