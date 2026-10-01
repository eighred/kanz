# optimization service (OPT-01e)

Portfolio construction & optimization — the portfolio-manager's forward workflow
(ROI #29). A **stateless construction service** over `internal/optimization`: it
optimizes target weights (mean-variance / risk-parity) under the SAME mandate
constraints COMP-01 enforces, builds a human-in-the-loop `RebalanceProposal`, and
materializes an approved proposal into OMS-01 `SubmitOrder` commands.

## Boundary

`internal/optimization` lives **outside** `internal/risk`, so per the RISK-02 arch
boundary it does not import the risk impl packages — the MODEL-01e covariance is
an **input** (`MarketInputs.Covariance`), and the package ships its own
`SampleCovariance` estimator. It **does** reuse `internal/compliance` directly
(no boundary there): the constraint layer projects target weights into a
compliance `Book` and runs the COMP-01 engine, so a book you can't hold you can't
optimize into — one source of truth. The arch test confirms no new edge.

## Objectives

`MaxReturn` (LP, optional mean-variance blend), `MinVariance`, `MaxSharpe`
(tangency), `RiskParity` (equal risk contribution). A small dependency-free
solver (projected gradient over a budget-box feasible region + the tangency
closed form + a damped risk-parity fixed point) — no gonum/QP library.

## Endpoints

| Method / path     | Body                                                                 | Returns |
|-------------------|----------------------------------------------------------------------|---------|
| `GET /healthz`    | —                                                                    | liveness |
| `GET /readyz`     | —                                                                    | readiness |
| `GET /metrics`    | —                                                                    | Prometheus |
| `POST /v1/propose` | retired float contract | HTTP 410 with migration guidance |
| `POST /v2/propose` | exact financial values plus statistical estimates | read-only exact recommendation |
| `POST /v1/orders` | `proposal`, `issuer`                                                 | the proposal's trades as issuer-bound order commands (DTO) |

`/v1/orders` is the OPT-01e bridge: it maps an **approved** proposal to
`order.v1.SubmitOrder` commands, issuer-bound on `CommandMetadata.issuer`. A real
deployment wires the bus publisher + COMP-01 pre-trade gate behind the bridge
seams (`Publisher`, `Gate`) at the composition root, so each materialized order is
re-checked (deny-by-default) and published; the default boot dry-runs the mapping.

## Config

| Env                           | Default | Meaning |
|-------------------------------|---------|---------|
| `OPTIMIZATION_LISTEN`         | `:8080` | HTTP listen address |
| `OPTIMIZATION_LOG_LEVEL`      | `info`  | slog level |
| `OPTIMIZATION_OTLP_ENDPOINT`  | —       | OTel collector for span export |

## Human-in-the-loop

A proposal is **proposed**, never auto-executed (the AUTO-01 conservative stance).
Materialization into live orders is a separate, issuer-bound step gated on
approval and a pre-trade compliance re-check.

## Exact proposal contract (v2)

The gateway exposes `POST /v2/model-portfolios/propose` with read authority and
forwards to `/v2/propose`. Its optimization upstream URL is the service origin,
without a version suffix. Legacy v1 proposal clients receive 410; there is no
float compatibility conversion. The independent v1 order endpoint does not
accept v2 recommendations or grant approval from a feasibility result.

Financial values are JSON strings (`dec.Exact`), never JSON numbers: `nav`, every
`current_weights` and `prices` value, and `threshold`. Currency and threshold are
required. NAV and prices use the declared uppercase currency; quantity is in
instrument units. A missing holding in an explicitly supplied map is zero;
missing price, NAV, currency, map, or threshold is unknown and refused. All
instruments require positive prices. This contract supports long-only fully
invested holdings (sum exactly one) or an explicitly empty initial book. Cash
allocation and short financing are not inferred. Holdings/prices outside the
supplied universe are refused. Exact rational quantities remain strings, e.g.
`"1/3"`; no lot-size rounding or execution authority is implied.

`expected_returns`, `covariance`, objective parameters and the Black-Litterman
prior/views are approximate statistical estimates. BL `market_weights` is a
statistical prior, distinct from the exact current holdings. Solver targets are
rounded to 12 decimal places, nearest-even; the budget residual is assigned to
the largest absolute solver weight, ties broken by instrument ID. The response
states this policy and checks the resulting financial values exactly. `Targets`
are desired weights; `EvaluatedWeights` are holdings after the emitted trades,
including unchanged holdings whose differences do not exceed the exact
threshold. Return/risk estimates describe `EvaluatedWeights`, not an ideal book
that omitted trades would never produce.

Optional `constraints` uses `LongOnly:true` and exact string `MinWeight`,
`MaxWeight`, `MaxTurnover`, `Bounds` (`Min`/`Max` per instrument), and `SectorCaps`.
Absent limits are unconstrained within the long-only unit budget; explicit zero
is zero. Box approximations guide the solver, but original exact constraints
are checked afterwards against both target and proposed holdings. Sector caps
and turnover are acceptance checks, not a claim of a globally optimal grouped
solution. Missing sector classification or a violated constraint refuses the
request. Mandate lookup is scoped to the authenticated tenant and portfolio.
A governing mandate receives exact canonical Decimal candidate values or the
request is refused when they cannot be represented without loss. `UNCHECKED`
means no mandate resolved; `INFEASIBLE` means a real mandate breached; `FEASIBLE`
is only hypothetical-book feasibility, never human approval.

The contract caps the universe and BL views at 128, concurrent computations at
four per process, request bodies at 8 MiB, numeric representations at dec.Exact's
bounds, and JSON nesting at 16. Unknown fields, duplicate/case-alias keys,
trailing documents, invalid dimensions and non-finite estimates are refused.
Capacity exhaustion returns 503. All exact-input failures return 400; mandate
source/identity failures retain their explicit 422/503 disposition.

```json
{"portfolio_id":"PF","instruments":["A"],"expected_returns":[0.1],"objective":{"Type":0},"current_weights":{},"nav":"9007199254740993","prices":{"A":"0.000000000001"},"threshold":"0","currency":"USD"}
```
