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
| `POST /v1/propose`| `instruments`, `expected_returns`, `covariance`, `objective`, `constraints`, `current_weights`, `nav`, `prices` | optimized target weights + minimal trade list (RebalanceProposal) |
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
