---
name: stack-routing
description: Find the owning Kanz plane and typed contract when a change crosses a service boundary or exposes a capability.
---

# Route a Kanz change

Use the root `AGENTS.md` for the binding planes and invariants. For the current
change, find its producer, consumer, typed contract, durable store, and guard.
Start with path-limited `rg` and the owning Issue; widen only when a concrete
caller or event crosses the initial boundary. [The route map](references/map.md)
lists useful entrypoints and searches; read it when ownership is unclear.

Check whether a proposed route already exists in schema, gateway, domain API,
or event subjects before adding a parallel one. Preserve execution's fixed
risk → compliance → mandate → OMS → router → venue-adapter sequence. Keep
exchange authentication in the adapter and agent-facing reads behind scoped
domain APIs. NATS carries FACTs; Kafka is the derived archive.

Output the owner, contract, observed callers, and any unresolved authority.
If two planes must change, state the interface between them and test each side
at the boundary that can actually fail.
