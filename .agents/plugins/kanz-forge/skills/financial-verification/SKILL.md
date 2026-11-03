---
name: financial-verification
description: Verify a Kanz financial calculation, portfolio attribution, or backtest against dated inputs and independent numerical evidence.
---

# Verify a financial result

Start with the decision the figure informs. Identify tenant, portfolio, instrument, currency, quantity units, event time and when each input became known. Label observations, assumptions, estimates and simulations separately. Missing critical terms mean the result is unavailable.

Route accrual and discount calculations to kanz/internal/risk/pricing; portfolio attribution to kanz/internal/performance; fill-cost attribution to kanz/internal/execution/tca; replay to kanz/tools/backtest. Inspect the consuming service before treating a library function as a served capability. Do not implement another pricing engine in this plugin.

For accrual, identify day-count convention, calendar, accrual boundaries and normalization to civil dates. Compare the Go output to a hand-derived or separately implemented rational reference. State tolerance in the output unit. Exercise month ends, leap years, zero intervals and reversed intervals where defined. From the Go module run go test -p 1 -count=1 ./internal/risk/pricing.

For strategy evidence freeze the point-in-time universe (including delistings), training cutoff, holdout and walk-forward schedule before fitting. Account for spread, fees, impact and rejected trades. Record every parameter trial; multiple searches on one holdout do not yield independent confirmations. Separate simulation from live evidence.

Return the methodology version, source identities and revisions, reproducible command, expected and observed values, tolerance and remaining unknowns. Human owners approve valuation policy, mandates and regulated actions. This skill cannot authorize trades or ledger postings.
