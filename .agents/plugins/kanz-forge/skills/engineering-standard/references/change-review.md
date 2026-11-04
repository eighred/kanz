# Review the change at its failure boundary

Use the rows reached by the actual diff and callers. They are implementation
questions, not a requirement to run every kind of test for every change.

| Changed behavior | Resolve before editing | Evidence that can reject the design |
|---|---|---|
| API, RPC or event contract | Existing producer/consumer versions, optional versus required fields, error semantics and rollout order | Old/new consumers, malformed input, missing critical field, and the owning schema compatibility gate |
| Transaction or outbox | What commits atomically, who drains committed work, retry identity and crash recovery | Crash between durable commit and publication; duplicate delivery must preserve the same domain result |
| External side effect | Timeout before/after send, stable request identity, observation/reconciliation path | Ambiguous response must remain unresolved; a retry must not manufacture a second action or success |
| Tenant/authentication boundary | Principal source, resource scope and where authorization is enforced | Forged identity, another tenant's identifier, omitted scope, and indirect callers through the real boundary |
| Migration or persisted format | Compatibility while versions coexist, existing-row backfill, lock cost, resumability and rollback limits | Existing data and mixed-version reads/writes; new schema creation alone is insufficient |
| Shared state or caching | Correctness owner, ordering, invalidation, bounded growth and degradation | Restart, concurrent writers, stale/missing entries, unavailable backend; a cache hit must not create authority |
| Policy or operational control | Authoritative policy/version, effective time, permitted actor and durable decision record | Denial and unknown inputs before effects; replay must explain the rule and inputs used then |
| Deletion or deduplication | Runtime registrations, generated consumers, build tags, operator use and semantic differences | Every required consumer still works; lexical absence alone cannot establish dead code |
| Performance or token optimization | Comparable baseline, workload, correctness constraints and measurement boundary | Repeated comparable samples plus unchanged relevant behavior; smaller output alone is not a quality measure |

For database changes follow the owning service's migration history. Prefer an
expand/backfill/transition/contract sequence when running versions need it;
do not rewrite an applied migration to make a fresh database pass. If rollback
cannot restore lost data or external effects, state that limitation before
execution and choose a recoverable rollout. A code rollback is not a data repair.

For retryable operations identify the idempotency key and its tenant/operation
scope, where the outcome is persisted, and whether duplicates can arrive after
restart. Bound retry duration and distinguish permanent refusal from transient
failure. Repeating an external request is safe only when the receiver or durable
reconciliation protocol makes it so; an exception handler is not that protocol.

For controls, preserve denied attempts and the distinction between requested,
approved, submitted and observed outcomes. Logs can aid diagnosis but cannot
replace required durable audit facts. Review redaction and retention at the
owning boundary; never place raw credentials or unrelated tenant data in a test.

Use existing typed seams and domain error conventions. A wrapper is useful only
when it owns a real translation, policy or lifecycle; a pass-through wrapper that
hides errors or duplicates another owner adds a second repair site. Prefer no
new abstraction until current consumers establish one.

Example: `kanz/internal/execution/grpc_venue.go` distinguishes an empty fill
result from an execution error. Its tests in `grpc_venue_test.go` separately
exercise those outcomes. Converting timeout into empty success would change the
domain outcome even if the caller stopped returning errors. Trace the consumer
before deciding whether retry or recovery is the appropriate repair.
