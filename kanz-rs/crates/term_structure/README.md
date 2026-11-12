# Zero/par term-structure compatibility kernel

This bounded #1534 family covers continuously compounded zero curves, conversion
from annual/semiannual quotes, sequential par bootstrapping, flat-zero
extrapolation, linear-zero/log-discount interpolation, forward and par queries.
The pure Rust library owns scalar analytics only. Go retains quote resolution,
PIT storage, strip coverage, capture/artifact identity, model approval, canonical
Decimals and financial authority. No writer, new service or activation is added.

The existing default-off bond shadow now uses `Curve::zero_with_bump`; its copied
interpolation was removed. Its canonical request/result schemas, 256-pillar,
100-year and 1,201-payment limits, timestamp validation, schedule identity,
tenant attribution, process supervision and Go-owned selection remain intact.
Pure-library tests do not prove those boundaries; the actual bond supervisor's
separate CI job remains required.

## Methods and failure semantics

Tenors are already resolved years; rates are annual fractions, with continuous
zeros internally. A one-basis-point parallel bump is `0.0001` before interpolation.
Calendar/day-count selection and multi-curve collateral/projection conventions
are not inferred here. Par inputs explicitly use the declared pillar dates as
their common coupon schedule; mixed deposit/future/swap calibration remains Go.
Its solver convergence/near-zero calibration is not qualified by this family;
that remaining #1534 work needs its own repricing and residual controls.

- Finite strictly positive ascending tenors are required after sorting. Annual
  rates must exceed -1, semiannual rates -2; continuous negative rates are valid.
  Unknown conventions refuse. A positive discount greater than one is valid at
  negative rates; interpolation alone does not certify arbitrage-free inputs.
- Compounding uses `log1p`. Par bootstrap uses a retained prefix annuity and the
  difference of two `log1p` terms; par repricing uses `expm1`. These preserve tiny
  nonzero rates that `log(1+r)` or `1-DF` erases. See the primary
  [Go math implementation](https://go.dev/src/math/log1p.go), checked 2026-11-12.
- The par recurrence is O(n), replacing repeated O(n^2) prefix sums. Sorting is
  still part of total construction cost. Forward queries analytically integrate
  each interpolation segment rather than subtract nearly equal log-discounts.
  This fixes a controlled adjacent-tenor example returning 6.25% for a 5% forward.
- Go now refuses invalid definitions at construction; scalar unavailable queries
  return NaN at its existing float API. Rust returns `Result` without a candidate.
  Empty/invalid coupon schedules, reversed/nonfinite forward intervals, discount
  overflow/underflow-to-zero and nonfinite arithmetic are unavailable. For valid
  curves, finite nonpositive discount times retain the legacy known value 1.
- Rust permits at most 256 pillars and 4,096 coupon dates. These are candidate
  work bounds, not a new market maturity policy; the Go zero/par APIs are not
  newly capped. Go mixed-swap calibration separately refuses nonfinite quotes
  and annual schedules exceeding 4,096 periods before generating dates.

Before-repair Go tests reproduced refusal of a valid -1% par strip, loss of a
`1e-20` rate, the adjacent-tenor forward error, NaN-query panic and successful
construction with unknown/invalid inputs. The repair also tests that failed
refresh cannot install/capture a new curve or displace its prior PIT version.
That is an in-process store/callback check, not durable publication proof.

## Independent qualification

The negative annual par example has exact discounts `100/99` and `10000/9801`.
`reference/reference.py` independently solves all par equations by dense pivoted
Gaussian elimination, uses barycentric interpolation and high-precision direct
log-discount differences. It does not call production pricing helpers, retain a
prefix annuity, or copy piecewise-forward integration. Ten references at 80 and
110 Decimal digits must agree within `1e-45*(1+abs(value))`; 40 significant digits
are retained. Near-zero cases also have explicit relative-accuracy controls.

The source-bound Go corpus contains **864 unique cases: 820 values, 29 refused
definitions and 15 unavailable queries/results**. Cases cover both constructions,
both interpolations, three compounding conventions, negative/zero/tiny/high rates,
unsorted/irregular dates, 256 pillars, short/long extrapolation, adjacent forward
endpoints, bumps, absent schedules and IEEE invalid values. Every sorted pillar,
zero, discount, bumped zero, forward and par result is checked.

Comparison budgets are fixed before execution: exact tenor bits; discount error
at most `1024*EPS*pillar_count*abs(expected)`; rate-unit error at most the same
factor times the maximum of pillar-zero scale and expected magnitude (including
the known `1e-4` bump when applicable), plus `64*MIN_POSITIVE`. This accounts for
binary64 accumulation/conditioning; it is not money rounding or economic error.
No residual-based tolerance changes are permitted. Missing/relabelled source,
altered zero, duplicate ID and truncated inventory must refuse.

```sh
cd kanz
go run ./services/risk-engine/tools/curve_reference -source "$SOURCE_SHA" > "$CORPUS"
cd ../kanz-rs
python3 -B crates/term_structure/reference/reference.py --check
cargo test --locked -p kanz-term-structure --test numerical
cargo test --locked -p kanz-term-structure --test go_conformance missing_or_malformed_capture_refuses -- --exact
KANZ_CURVE_CORPUS="$CORPUS" KANZ_CURVE_SOURCE="$SOURCE_SHA" \
  cargo test --locked -p kanz-term-structure --test go_conformance fresh_go_oracle_is_required -- --ignored --exact --nocapture
```

An ordinary Cargo invocation ignores the fresh-oracle gate; that is not Go
compatibility evidence. The dedicated `term-structure` CI job explicitly runs it
and retains reference/corpus/lock hashes, source/tree, toolchains and all results.
The existing bond job exercises the changed actual worker through Go supervision.

[Aladdin's public capabilities](https://www.blackrock.com/aladdin/platforms/products/aladdin-risk)
(checked 2026-11-12) describe integrated multi-asset analytics, data quality,
scenario analysis and governance. They define useful capability benchmarks, not
an available specification for proprietary algorithms. This family establishes
specific numerical/compatibility evidence; it does not establish BlackRock
equivalence, superior pricing, production performance or whole-platform parity.
Those claims require representative model/market-data/portfolio and runtime
benchmarks. Existing data/terms/model owners and Go/OpenSSL delivery holds remain.
