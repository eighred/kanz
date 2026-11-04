# Domain decisions and implementation proof

Load the relevant recipe, confirm current consumers, and keep calculations in
these owners. Commands run from `kanz/` after schema generation; each is a
starting package scope, not complete system acceptance.

## Pricing and model selection

Owners: `internal/risk/pricing`, `internal/risk/termsource`,
`internal/risk/benchmarks`. Before choosing a pricer, establish instrument terms,
quotation basis, compounding/day-count/calendar conventions and as-of inputs.
Check conversion/provider selection as well as the equation. For an iterative
method define convergence/error behavior; a plausible endpoint is not proof of
convergence. Exercise leap/month ends, zero/reversed intervals where defined,
unknown conventions and invalid outputs.

`pricing/daycount_reference_test.go` uses daily rational accrual to check a
production year-splitting algorithm; `benchmarks/benchmarks_test.go` checks
published analytical examples and validation gates. These illustrate independent
proof, not permission to reuse their tolerances for an unrelated method.

`go test -p 1 -count=1 ./internal/risk/pricing ./internal/risk/benchmarks`

Add the affected pricing subpackage or `./internal/risk/termsource` for conversion
and provider changes. Top-level tests do not cover every pricer or service wiring;
provider database tests may skip without their real dependency.

## Risk limits and governed execution

Trace the measure from `internal/risk/compute` to `internal/compliance`, then
`services/oms/cmd/oms`. Define maximum/minimum and which direction means more
risk before selecting the algorithm. `compute/marginrisk.go` exposes liquidation
proximity to a ceiling rule; substituting distance would reverse the control
unless the rule changes too. Test equality/breach, both position signs, empty
exposure, stale marks and incomplete account/venue coverage.

Read `compliance/gate.go` and relevant guards: the projected post-trade book,
tenant-scoped effective mandate and decision recorder must all be wired. Preserve
the declared denial/warning/transient-failure distinction and guarded exemptions;
do not invent a stricter policy while repairing arithmetic. Add security review
for authorization or mandate-publishing changes. Start with
`go test -p 1 -count=1 ./internal/compliance`, then test the changed measure and
OMS caller. Rule tests alone do not prove durable admission or approved policy.

## Performance and execution cost

Owners: `internal/performance` and `internal/execution/tca`; consumers include
`services/performance/internal/server` and
`services/oms/internal/order/attribution.go`. Define flow timing, reporting
currency, benchmark/classification horizon and buy/sell cost sign. Reconcile
return effects to active return/NAV movement and attribution legs to realized
shortfall. Account for fees/rebates and partial fills; an unavailable benchmark
or decomposition is not zero cost or complete coverage.

`go test -p 1 -count=1 ./internal/performance ./internal/execution/tca`

Use `TestSlippageVsVWAP_AbsentBenchmarkIsNotZero` for missing VWAP semantics and
`TestAttribute_OneUnobservableSliceRefusesTheWholeDecomposition` for partial
coverage. Inspect/test the OMS consumer before claiming terminal attribution is
recorded. A measurement failure stays observable without blocking real fill
state/fact recording.

## Accounting and reconciliation

Owners: `services/accounting/internal/ledger`, `internal/nav.go` within that
service, and `internal/custody`. Trace source-event identity, durable posting,
position/cash fold, publication and served view. Design for idempotent replay,
corrections, fee revisions and cash-versus-settlement timing at the actual tenant,
portfolio and account grain. A retry must not create a second economic event.

Check component conservation in the stated currency/time, including foreign cash
and accruals. Preserve unmatched records as named breaks; do not insert balancing
plugs or guessed FX. A balanced result can still omit a source, so verify coverage
declarations and producer wiring. Start with
`go test -p 1 -count=1 ./services/accounting/internal ./services/accounting/internal/ledger`.
Add the affected custody/consumer/server tests. Report Postgres skips; local folds
do not prove transactional outbox, crash recovery or cross-tenant isolation.

## Replay and strategy implementation

Owners: `tools/backtest` and `internal/lake/dataset`. Freeze universe (including
delistings), source revisions, event/knowledge times, ordering and seeds. Keep
training cutoff, trial set and holdout separate when implementing a fitted model.
`backtest_test.go` distinguishes repeatable ordering from corrections visible
only after knowledge time; `calibration_test.go` counts unresolved outcomes so
missing data cannot silently improve calibration.

`go test -p 1 -count=1 ./tools/backtest`

Decision replay alone does not prove execution realism or predictive value. If
those are requirements, implement and verify costs, capacity, partial/rejected
orders and outcome coverage explicitly. A simulation result cannot bypass risk,
compliance or mandate gates; it is evidence for development, not trading authority.
