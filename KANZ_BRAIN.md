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
