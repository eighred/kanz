# Bond-risk conformance

This offline tool projects canonical `reference.v1.BondTerms` through the same
terms conversion and payment-schedule helper used by Go. It exports deterministic
`quant.v1.BondRiskRequest` protobuf bytes (JSON base64) alongside Go `Bond.CurveRisk`
results. It does not consult providers, install prices, publish facts or grant
financial authority.

From `kanz`:

```sh
go run ./tools/bond-risk-conformance > /tmp/bond-risk-corpus.json
go test -p 1 ./tools/bond-risk-conformance -count=1
```

Fourteen fixtures cover zero and coupon bonds, negative and measured-zero rates,
settlement exactly on a coupon date, full initial-stub coupons, Go's normalized
backward month arithmetic, leap dates, measured zeros at maturity, all supported
accrual day-count enums, and both curve interpolation conventions. Curve discount
time stays ACT/365F regardless of coupon accrual convention. Explicit payment dates
and analytical finite-bump sums independently check Go before any Rust comparison;
the oracle does not call the production schedule or subtract bumped prices.

The fixed bump is one basis point. Dirty price and DV01 are nominal price units
per bond (face value 1000 in these fixtures), not portfolio totals; DV01 is the
absolute price difference for one basis point. Duration is in years and convexity
in years squared. Tolerances were fixed before candidate results:

| Metric | Absolute | Relative |
|---|---:|---:|
| Dirty price | 1e-9 | 1e-11 |
| DV01 | 1e-9 | 1e-9 |
| Effective duration | 1e-7 | 1e-9 |
| Convexity | 2e-5 | 1e-8 |

The predicate is absolute error ≤ absolute tolerance + relative tolerance ×
absolute reference value. Convexity allows the cancellation inherent in Go's
central 1bp difference. Tolerances must not be widened to fit a candidate.

For actual Rust process proof, build `kanz-bond-risk-shadow` with a reviewed
`KANZ_BUILD_VERSION` and set `KANZ_BOND_RISK_BIN` to its absolute path plus
`KANZ_BOND_RISK_VERSION` to that exact embedded stamp. Then run:

```sh
go test -p 1 ./tools/bond-risk-conformance -run '^TestRustBondRiskProcessConformance$' -v -count=1
```

That test executes the real binary with raw protobuf stdin/stdout, verifies all
four values and capture ID/digest/tenant/build-version binding, checks ten typed
invalid inputs and rejects malformed/oversized wire. Child IO and time are bounded;
test diagnostics never print worker output. Missing binary configuration explicitly
skips locally. Required CI must verify the test passed without skipping. A local
Go oracle pass does not establish Rust execution, model approval or production use.
