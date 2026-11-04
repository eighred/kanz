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

For `/repo` and `/trace`, construct the smallest source-backed dependency graph
that answers the question. Resolve definitions and references with the available
language server; use package metadata such as `go list` for imports. `rg` finds
candidates, not resolved calls. For each relevant edge record the relation
(calls, imports, publishes, subscribes, reads, writes or protects), source and
destination, revision, `file:line`, and whether it is resolved or inferred.
Trace interface implementations and wiring; a symbol with no lexical caller
may still be reached through registration, reflection, tags or deployment.

Follow both directions: upstream inputs and callers show compatibility impact;
downstream consumers, stores and guards show validation scope. Event producers
and consumers connect through their typed subject/schema, not matching names.
Label an unresolved edge and the check needed to resolve it. No results from a
search or optional parser mean unknown coverage, not proof of no dependency.

Output the owner, contract, observed callers, and any unresolved authority in
a compact path or table. Include a diagram only when it makes the boundary
clearer. If two planes must change, state the interface between them and test
each side at the boundary that can actually fail. For file placement, follow
the nearest owning component and existing second-consumer rule in AGENTS.md;
directory tidiness alone is not a reason to move code.

This is on-demand source traversal, not an installed Graphify index. If an
optional graph tool is already available, check its indexed revision, language
coverage and edge provenance before using it; verify decisive edges in source.
Do not upload source, add a persistent index or install a graph service merely
to answer a local dependency question.
