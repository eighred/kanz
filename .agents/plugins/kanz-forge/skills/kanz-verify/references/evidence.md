# Evidence ladder

Choose the lowest level that can disprove the change's failure mode, then add
required higher gates. Report each level reached and the first missing one.

1. **Local logic:** deterministic unit/domain test of the state transition and
   a discriminating bad case.
2. **Real boundary:** disposable NATS, non-superuser Postgres with FORCE RLS,
   Kafka or Redis where the claim depends on them. Record service versions and
   skip counts. `fakeBus` and in-memory stores do not stand in for these.
3. **Build and concurrency:** exact-head build, vet, lint, architecture guards,
   and Linux `-race` when shared mutable state matters. On Windows, use `-p 1`
   for database-gated Go tests and compare reported packages to `go list`.
4. **Deployed behavior:** exact image/config digest and observed result in the
   relevant test estate, with independent source data when reconciliation is
   claimed.
5. **Authority:** human approval, mandate, fee policy, credentials and DR
   posture required for a capital action. No test or green CI supplies these.

The repo's command and environment caveats are in root `AGENTS.md`; CI truth is
`.github/workflows/kanz-ci.yml`. Do not copy their commands here. A result is
`not run` or `skipped` when the dependency did not run. If a guard has an
exemption, passing means the exemption still exists, not that the underlying
gap is repaired.

## Select a discriminating observation

| Claimed repair | Check that could falsify it |
|---|---|
| Tenant isolation | A distinct tenant and an omitted scope against the actual store/API, using a role without superuser or RLS-bypass authority |
| Durable idempotency | Duplicate/reordered delivery and restart across the real transaction/outbox boundary; count durable effects, not handler calls |
| External outcome handling | Timeout/refusal/empty-result cases remain distinct, with no invented fill or confirmed cancellation |
| Compatible schema or migration | Existing persisted data and old/new consumers, not only a fresh database or freshly generated client |
| Financial calculation | Independent reference or exact invariant with source dates, units and tolerance; use financial-verification |
| Guard or detector | A representative forbidden artifact is rejected and an allowed one is accepted; a matcher that sees neither proves nothing |
| Performance/context reduction | Same workload and correctness boundary on comparable baseline/head runs; report sample distribution and what was measured |
| Skill or command workflow | A representative host task reaches the intended skill, produces source-backed evidence and handles missing tools without fabricating success |

For database proof, record role properties and the relevant table policies;
`NOSUPERUSER` alone is insufficient if the role has `BYPASSRLS` or an owner can
bypass a policy. Check the test fixture's real connection rather than assuming
the application's normal role was used.

If a test harness or fixture changes, verify its negative case as well as the
product case: a green result from a harness that drops failures is not evidence.
Perform mutations only through permitted disposable fixtures; a blocked mutation
remains a stated limit, not a reason to edit live state or weaken the guard.
