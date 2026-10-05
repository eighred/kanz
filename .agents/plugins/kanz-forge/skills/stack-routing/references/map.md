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
