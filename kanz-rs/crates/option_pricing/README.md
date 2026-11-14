# European option pricing compatibility candidate

This pure Rust library is one bounded family of [#1534](https://github.com/eighred/kanz/issues/1534).
It has no runtime, network, storage, event publisher, credentials or production
caller. It does not depend on the unmerged accounting/precision branches.

| Existing owner | Family and boundary |
| --- | --- |
| `pricing.PriceGreeks` → `BlackScholesPrice/Greeks` | European BSM price and per-unit analytic Greeks: the family qualified here. |
| `pricing.PriceGreeks` → `BinomialPrice/Greeks` | American CRR remains Go-owned; this candidate returns `UnsupportedModel`. |
| `pricing.Bond.CurveRisk` and existing `bond-risk-shadow` | Separate qualified bond family; no change here. |
| `compute.positionGreekContribution` | Go resolves terms, spot, vol, valuation time and curve, handles missing/expired inputs, scales dollar Greeks and emits canonical Decimals. No Rust substitution occurs. |

Inputs are the existing Go library's already-resolved binary64 working values:
spot, strike, years to expiry, annual continuously compounded rate/yield, and
annual volatility. Vega/rho are per **1.00**, theta is per year **to expiry**,
the negative of desk calendar theta. There is no quantity, Money or canonical
financial wire here. These existing transcendental analytics do not extend
floating point into journals, cash or positions.

The served Go caller currently passes zero yield, derives years with
`Expiry.Sub(asOf).Hours()/24/365`, and retains input/provenance/coverage and
financial authority. The library also preserves the existing general pricing
API's continuous-yield convention. A matching kernel cannot establish that
terms, volatility or source coverage are available.

Finite degenerate inputs (nonpositive time, volatility, spot or strike) preserve
Go's **legacy spot intrinsic price and zero Greeks**. This is compatibility,
not the mathematical discounted zero-volatility limit or an admission default.
The served Go caller already excludes missing spot/vol and expired contracts.
Nonfinite working inputs/results refuse without a partial candidate. Unsupported
models never silently use the European formula.

The normal CDF uses the already locked `libm =0.2.16` complementary error function
([API](https://docs.rs/libm/0.2.16/libm/fn.erfc.html)); no handwritten CDF approximation
or new dependency version is introduced.

## Numerical qualification

Primary published examples: MathWorks [`blsprice`, R2026b](https://www.mathworks.com/help/finance/blsprice.html),
checked 2026-11-12. Inputs `(S,K,r,T,sigma,q)` of `(100,95,.1,.25,.5,0)` give
call/put `13.6953 / 6.3497`; `(910,980,.02,.25,.25,.025)` give `19.6863 / 90.4683`.
Their four-decimal precision fixes an absolute tolerance of `0.00005` price units.

Separate tests integrate the discounted terminal payoff against a Gaussian
density, without the production price, CDF or Greeks. Simpson refinement from
8,192 to 16,384 intervals must agree within `2e-10`; price comparison uses `5e-9`.
Central differences of that independent integral check delta/gamma at `2e-6`
and vega/theta/rho at `2e-4` natural units, with fixed bumps documented in the test.
Put-call price and sensitivity identities are additional controls, not independent
proof of the absolute price level.

The Go exporter under `services/risk-engine/tools/option_pricing_reference`
captures the actual Go `PriceGreeks` implementation afresh. Its private JSON
test capture contains binary64 bit patterns, source SHA and a closed family tag;
it is not a production contract or a replacement for canonical schemas.
The differential gate requires **8,162 unique cases**: 8,120 values, 36 invalid
nonfinite inputs, four nonfinite outputs and two unsupported American cases.
The active grid covers both rights, scales, moneyness, negative rates/yields,
near-expiry/low-vol corners and long maturities. Legacy limits are separate cases.

Conformance tolerance is fixed at 64 binary64 epsilons of discounted input scale
plus 16 epsilons of expected price, and 128 epsilons times `1+abs(expected)` for
each Greek. Price scale accounts for subtraction near zero; this is an analytic
compatibility budget, not a financial rounding convention. Every field and
outcome is compared. Relabelled source, altered delta, duplicate identity and
truncated inventory are mandatory negative controls.

Run from a reviewed clean checkout, with an exact source SHA:

```sh
cd kanz
go run ./services/risk-engine/tools/option_pricing_reference -source "$SOURCE_SHA" > "$CORPUS"
cd ../kanz-rs
cargo test --locked -p kanz-option-pricing --test numerical
cargo test --locked -p kanz-option-pricing --test go_conformance missing_or_malformed_capture_refuses -- --exact
KANZ_OPTION_PRICING_CORPUS="$CORPUS" KANZ_OPTION_PRICING_SOURCE="$SOURCE_SHA" \
  cargo test --locked -p kanz-option-pricing --test go_conformance fresh_go_oracle_is_required -- --ignored --exact --nocapture
```

An ordinary Cargo run reports the fresh-oracle test as ignored; it is **not**
conformance evidence. The dedicated CI job explicitly runs it and retains corpus,
source/tree, toolchain, lockfile digest and all numerical result logs. Source
guards and mutation probes separately protect the pure-library boundary and
mandatory fresh-capture commands. Neither these checks nor a successful kernel
promote a model or transfer the Go owner's financial authority. Served shadow,
provenance/scheduling and controlled rollback remain later #1534 acceptance.
