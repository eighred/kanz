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
