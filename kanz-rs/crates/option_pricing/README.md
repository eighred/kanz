# Option pricing compatibility candidates

This pure Rust library contains bounded families of [#1534](https://github.com/eighred/kanz/issues/1534).
It has no runtime, network, storage, event publisher, credentials or production
caller. It does not depend on the unmerged accounting/precision branches.

| Existing owner | Family and boundary |
| --- | --- |
| `pricing.PriceGreeks` → `BlackScholesPrice/Greeks` | European BSM price and per-unit analytic Greeks: the family qualified here. |
| `pricing.PriceGreeks` -> `BinomialPrice/Greeks` | American CRR candidate in `american::evaluate`; fixed 200 steps, Go-compatible bumps and explicit unavailable-tree refusal. The root European `evaluate` still refuses American exercise. |
| `pricing.Bond.CurveRisk` and existing `bond-risk-shadow` | Separate qualified bond family; no change here. |
| `compute.positionGreekContribution` | Go resolves terms, spot, vol, valuation time and curve, handles missing/expired inputs, scales dollar Greeks and emits canonical Decimals. No Rust substitution occurs. |

Inputs are the existing Go library's already-resolved binary64 working values:
spot, strike, years to expiry, annual continuously compounded rate/yield, and
annual volatility. Vega/rho are per **1.00**, theta is per year **to expiry**,
the negative of desk calendar theta. There is no quantity, Money or canonical
financial wire here. These existing transcendental analytics do not extend
floating point into journals, cash or positions.

The Go compute caller passes zero yield and derives years with
`Expiry.Sub(asOf).Hours()/24/365`, and retains input/provenance/coverage and
financial authority. Its production registration remains held by #1230.
The library also preserves the existing general pricing
API's continuous-yield convention. A matching kernel cannot establish that
terms, volatility or source coverage are available.

Finite degenerate inputs (nonpositive time, volatility, spot or strike) preserve
Go's **legacy spot intrinsic price and zero Greeks**. This is compatibility,
not the mathematical discounted zero-volatility limit or an admission default.
The Go compute path excludes missing spot/vol and expired contracts.
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

## American CRR family

The second #1534 family is exposed separately as `american::evaluate(Input)`.
It uses the same resolved inputs/output units and rejects European exercise.
It owns no runtime or financial records and has no production caller. Its fixed
200-step tree checks exercise at every node, including time zero. This is the
existing discrete approximation, not an assertion of continuous-time convergence
or a universal one-cent accuracy guarantee. [MathWorks CRR documentation](https://www.mathworks.com/help/fininst/crrprice.html)
(checked 2026-11-12) describes backward pricing and unavailable-price semantics;
it is methodological context, not an oracle for the specific conventions here.

Go previously substituted a European Black-Scholes price when CRR probability
fell outside [0,1]. The regression `S=60,K=100,T=1,r=1,q=0,sigma=.001` returned
zero despite the American holder's immediate exercise payoff of 40. Go now
returns NaN for an inadmissible/nonfinite tree; a single failed Greek bump makes
all five Greeks unavailable. The registered Go measure records incomplete
coverage; the revaluation helper returns unavailable and reports
`invalid_pricing_result` through its skip callback. Rust returns a typed error
with no partial
candidate. This is an intentional Go correctness repair, not compatibility with
the erroneous fallback. Ordinary valid trees retain the existing calculation.

Both implementations retain the finite legacy intrinsic/zero-Greeks convention.
Spot bumps are 1%; volatility/rate/time bumps are `.001/.0001/.0001` in annual
natural units. When T is at most `.0001`, theta is centered at `.0002` years.
When the negative volatility bump crosses zero, its price uses legacy intrinsic.
These are explicit compatibility conventions, not missing-input defaults, a new
rounding policy or a claim that bumped theta is the local derivative near expiry.

Independent checks consist of a hand-derived three-period stopping tree
(`u=2,d=1/2,p=1/2,discount=4/5`, put S=8,K=10), with root `348/125`; immediate
exercise's affine sensitivities; and eight 200-step, six-field references.
`reference/american_reference.py` uses only Python's standard-library Decimal,
whole-lattice memoized recursion and log-space nodes rather than the production
rolling row and binary64 powers. Regeneration at 80 and 110 digits must agree
within `1e-45` before retaining 40 significant digits. CI regenerates and checks
the committed reference; the oracle is test-only and grants no pricing authority.
Cases include put/call exercise, negative rates/yields, theta's maturity shift
and volatility's zero crossing.

American comparison budgets are fixed before comparison: price is
`8192*EPS*max(1,abs(S),abs(K))`; with `h=.01*S`, delta uses `price_budget/h`,
gamma `4*price_budget/h^2`, and vega/theta/rho use price budget divided by
`.001/.0001/.0001`. They allow binary64 accumulation/cancellation over 200 levels,
not economic model error or financial rounding. Finite legacy cases use the
price budget for all fields. No tolerance is selected from observed residuals.

Fresh Go capture uses `option_pricing_reference -family american -source
"$SOURCE_SHA"`. The required inventory is **502 unique cases**: 452 values,
36 nonfinite inputs, 12 unavailable trees/outputs/bumps and 2 unsupported models.
The same four capture negative controls are required. Run the independent and
fresh gates from `kanz-rs`:

```sh
python3 -B crates/option_pricing/reference/american_reference.py --check
cargo test --locked -p kanz-option-pricing --lib --test american_numerical
cargo test --locked -p kanz-option-pricing --test american_go_conformance missing_or_malformed_capture_refuses -- --exact
KANZ_AMERICAN_OPTION_CORPUS="$CORPUS" KANZ_AMERICAN_OPTION_SOURCE="$SOURCE_SHA" \
  cargo test --locked -p kanz-option-pricing --test american_go_conformance fresh_go_oracle_is_required -- --ignored --exact --nocapture
```

The separate `american-option-pricing` CI job retains the fresh corpus, reference
and lock digests, source/tree, tools and all result logs. The European inventory
and independently required gate remain intact. Existing Go/OpenSSL runtime holds
remain pending. Served Rust scheduling, snapshot provenance, measured capacity
and governed promotion/rollback remain later #1534 acceptance.

Production wiring is explicitly unproven: `RegisterGreeks`, `NewRevaluer` and
`EvaluateReval` remain named dark seams in `no_dark_measure_seam_test.go`, with
#1230/#1333 owning live option-input/identity wiring and #1232 owning scenario
dispatch. The consumer regressions exercise the real Go registry and revaluation
helper in process; they do not observe production loss or a deployed denial.
The scenario wrapper's existing linear fallback after unavailable revaluation
is unchanged. This slice does not certify complete scenario coverage or activate
these seams; that acceptance remains with the existing owners.
