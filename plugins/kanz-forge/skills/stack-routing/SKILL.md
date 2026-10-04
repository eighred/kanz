---
name: stack-routing
description: Route Kanz work to its execution, control, or intelligence owner and preserve typed boundaries when changing eighred/kanz.
---

# Kanz stack routing

This reconstructs the retired Kanz Forge routing role from historical
references and the current `AGENTS.md`. The original private skill text is
unavailable; the checkout's rules and code are authoritative.

| Work | Owner |
|---|---|
| Orders, fills, routing, exchange protocol/auth | Execution: OMS, router, venue adapter |
| Risk, compliance, mandates, margin, collateral, cash admission | Control: owning domain service |
| Agent reads, analytics, operational investigation | Intelligence: authorized read APIs |

State changes are commands admitted through the fixed control and order path,
then durable FACTs. Do not put execution behind MCP or a dynamic tool. Keep
OMS-to-adapter transport typed `venue.v1` gRPC/mTLS and exchange REST/WS inside
the adapter. Keep credential material at that boundary.

Use the canonical domain model in the core and venue-specific shapes at the
adapter. Keep shared implementation in a service's `internal/` until another
consumer justifies promotion. NATS is the FACT source and Kafka its derived
archive; do not recreate the retired two-bus reconciler.

Treat Web replacement of a TUI operation as a migration with verified operator
parity. Route discovered work to an Issue and implementation proof to a PR,
never to a second project-state or architecture document.
