# Route map for boundary changes

Read only the row that matches the task, then inspect its current code and
issue. This is a navigation aid, not a competing architecture document.

| Boundary | Entry points to inspect |
|---|---|
| Schema and subjects | `kanz-schemas/proto/`, `kanz/infra/nats/`, generated SDK instructions in `kanz-schemas/README.md` |
| Admission and execution | `kanz/services/oms/`, `kanz/internal/execution/`, `kanz/services/venue-*/`, `kanz/test/arch/` |
| Risk and compliance | `kanz/internal/risk/`, `kanz/services/risk-engine/`, `kanz/internal/compliance/`, mandate owner and schema |
| Book and custody | `kanz/services/accounting/`, migrations, ledger and custody consumers |
| Identity and reads | `kanz/services/api-gateway/`, service authz, NetworkPolicy, `kanz/services/mcp/` |
| Operator and deployment | `kanz/services/operator/`, `kanz/infra/`, relevant runbook and overlay |

Start with `rg -n '<subject-or-symbol>' <likely-paths>` and
`rg --files <owner>`. Trace producer → transport → consumer → store →
projection. A successful test helper is not a production caller. When a
route appears unused, check build tags, manifests, subscriptions, and git
history before removal. The current `AGENTS.md` resolves any disagreement.

## Decide where a file belongs

| Responsibility | Placement decision |
|---|---|
| Service-specific behavior | Existing owning service's `internal/` package; its `cmd/<service>/` composes startup, rather than becoming the domain implementation |
| Reused domain behavior | Promote from its present owner only after a second real consumer needs the same semantics; use `kanz/internal/` or `kanz/pkg/` according to existing import boundaries |
| Wire contract | Edit source under `kanz-schemas/proto/`, inspect all consumers, regenerate the ignored SDK; generated output is not an alternative place to fix the contract |
| Durable schema | Owning service's `migrations/` and store code, with existing-data and rollout checks |
| Deployment identity or connectivity | Relevant manifest/overlay under `kanz/infra/` plus consuming startup configuration; a library mock cannot verify this connection |
| Test or invariant | Nearest owning package for behavior; `kanz/test/arch/` for a cross-component architecture constraint; use existing fixtures rather than importing runtime code into Forge |
| Agent workflow | Existing Forge skill or a conditional reference read by that skill; create a new skill only for a distinct discoverable task, not to split paragraphs |

Inspect neighboring files and imports before applying a row. A path naming a
domain does not establish that it currently owns the behavior.

## Produce an impact graph that can be checked

Keep a compact table in the response: `from → relation → to`, evidence location,
revision, and `resolved` or `inferred`. Mark an unestablished edge `unknown` and
name the source or observation that would resolve it. Include test-only or
generated status when it changes the conclusion. Render a diagram only when it
helps; preserve provenance in the table instead of hiding uncertainty in arrows.

For example, changing `kanz/internal/execution/grpc_venue.go` requires inspection of
its constructor wiring in `kanz/services/oms/cmd/oms/venues.go`, the selected
venue's use in `kanz/services/oms/internal/order/service.go`, and the service
contract in `kanz-schemas/proto/venue/v1/venue.proto`. Source resolves this typed
path; actual endpoint selection and mTLS identity still require estate evidence.
