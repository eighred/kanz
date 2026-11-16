# Bond curve-risk shadow kernel

This process evaluates an immutable capture from the existing Go
`pricing.Bond.CurveRisk` path. It has no network client, database, event publisher,
order SDK or admission authority. Go remains authoritative and owns the captured
terms, payment-date projection, curve resolution, coverage and comparison.

Build with `KANZ_BUILD_VERSION` set to the reviewed source revision. The binary
refuses to run without that compile-time stamp; runtime environment cannot
override it. One raw `quant.v1.BondRiskRequest` is read from stdin through EOF;
one `BondRiskResult` is written to stdout before exit. Each is bounded to 1 MiB.
Malformed wire input exits nonzero with a constant diagnostic; typed failures
contain no values. The Go supervisor bounds pipes and process duration. The
executable is trusted code; OS resource and network isolation remain deployment
requirements, not properties of this subprocess wrapper.

`go_bond_schedule_v1` means the existing Go backward `AddDate` schedule, full
fixed coupons including the initial stub, payments strictly after valuation,
and redemption at maturity. Rust does not recompute calendar dates. It receives
the original canonical `BondTerms`, not decimals reconstructed from float inputs.
Canonical face and coupon values convert through exact `BigRational` and its
round-to-even binary64 conversion. This matches Go's canonical decimal bridge.
Amounts are derived internally using Go's operation order; no monetary double
fields cross the protobuf boundary.

The working curve contains the exact captured continuous-zero analytics and
their interpolation enum, not rounded rates reconstructed from an artifact.
Linear-zero and log-linear-discount interpolation use flat-zero tails. Parallel
bumps of +/-1 bp apply to every pillar before interpolation. Discount time is
ACT/365F with Go's `Duration.Hours` division order regardless of the bond's
accrual day count. Price, duration, convexity and DV01 retain the existing
symmetric-bump definitions. Decimal outputs use 17 significant decimal digits
and a checked binary64 round trip; values outside canonical exponent bounds
are refused instead of rounded to an arbitrary fixed scale.

Limits: 1,201 payment dates, 256 positive ascending curve pillars, 100-year curve
tenors and a 36,600-day bound on issue/maturity and valuation/maturity distances.
Required scalar presence, timestamp validity, identity, currency, supported
conventions, payment ordering/flags, finite inputs/results and positive discount
factors are checked. A matured bond with no remaining payments returns measured
zeros. An active bond with no projection fails. Missing curve metadata and
coverage remain UNKNOWN; the result's capture digest binds them without inventing
records. The caller verifies the echoed digest; this kernel does not authenticate
the invoking process or independently resolve the capture.

Unit oracles use independent zero-coupon/coupon PV moments with `sinh` identities
for the finite 1 bp bump. Source compatibility additionally requires the fresh Go
process corpus in CI; unit tests alone establish neither integration nor promotion.

The existing risk-engine connects this worker only when
`RISK_ENGINE_RUST_BOND_SHADOW_ENABLED=true`, with an absolute `EXECUTABLE`,
`EXPECTED_VERSION` equal to the embedded stamp, and optional `TIMEOUT` (same
prefix, default 5s, range 1s to 1m). Partial disabled configuration refuses
startup. Enabled operation requires the resolved market-data database, NATS and
scheduled curve calibration, so a configured worker cannot silently remain unwired.
One worker and one waiting capture limit concurrency; overload is diagnostic and
does not delay pricing. Only DV01 evaluation submits a capture, avoiding repeated
jobs for the same bond across the four FI measures.

Go records the actual input-capture time before enqueueing, detaches canonical
terms, preserves unavailable replay provenance and compares identity, digest,
tenant and binary version before numerical values. Diagnostic comparisons do not
replace Go results or approve models. Immutable worker packaging, retained
comparison evidence and controlled-environment acceptance remain open in
[migration issue #1413](https://github.com/eighred/kanz/issues/1413).
