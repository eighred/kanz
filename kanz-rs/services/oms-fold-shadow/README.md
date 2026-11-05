# OMS fill-fold shadow

This optional worker compares the existing Go OMS `ApplyFill` calculation. It
cannot authorize an order, call a venue, save state, claim an execution, or publish
a fact. The existing SQL version CAS, fill journal, outbox, admission controls,
recovery decisions and credential-bearing venue adapters remain authoritative.
The local typed process decision is recorded in #1413.

Go captures the live, reconciliation and durable recovery folds after the Go
calculation, including refusals. Each capture binds tenant, exact loaded SQL
version, original input state/fill, actual fold timestamp and capture time. A
capture is a computation observation, not evidence that its transaction committed.
Recovery captures the normalized working state before terminal disposition is
restored and before the complete execution journal rebuilds the final average.

Canonical protobuf input/output is limited to 1 MiB. Captures are size-checked
before cloning; one process runs at a time with one pending capture. Queue pressure
or worker failure never delays/changes authoritative execution. The worker has
no inherited credentials, uses a temporary working directory and bounded stdout,
stderr and wall time through the same shared runner as Julia and bond shadows.
This runner is not an OS sandbox. Deployment must separately enforce no network,
read-only application files and CPU/memory/process limits before enabling it.

The Rust kernel uses generated canonical types and exact rational arithmetic.
The supported input Decimal exponent domain is [-64,64]. Missing price/average
values retain Go ApplyFill's historical zero semantics; this is compatibility of
this specific aggregate, not a general rule for unknown financial data. Conversion
rounds half away from zero at 1e-8, then repeatedly rounds /10 until the coefficient
fits int64. Candidate state preserves every known field except the five fields
changed by ApplyFill. Unknown or noncanonical protobuf wire metadata is refused,
never discarded from a candidate. Go also rejects recursive unknown result fields.

Build with an immutable `KANZ_BUILD_VERSION`. Runtime environment cannot override
that stamp. Configure `OMS_RUST_FILL_SHADOW_ENABLED=true`, absolute
`OMS_RUST_FILL_SHADOW_EXECUTABLE`, exact `OMS_RUST_FILL_SHADOW_EXPECTED_VERSION`, and
optional `OMS_RUST_FILL_SHADOW_TIMEOUT` (default 5s, allowed 1s to 1m). The default is
off; partial configuration refuses startup. Enabling requires the existing durable
OMS database and NATS consumer configuration. No deployment enables this by default.

`kanz_oms_rust_fill_shadow_results_total` has fixed diagnostic status labels;
`kanz_oms_rust_fill_shadow_configured` reports configuration, not worker readiness.
The fresh Go corpus in `services/oms/tools/fill-conformance` checks independent
rational/state expectations. Linux CI executes that corpus and the actual Go
supervisor against the stamped Rust binary, separately from SQL/DR/venue proofs.
