package main

import (
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/oms/internal/riskview"
)

// THE RISK HALF OF THE PRE-TRADE GATE (#438), AND THE THREE THINGS IT REFUSES.
//
// # Why the OMS holds risk state at all
//
// Order admission never asked the risk engine anything: risk was a downstream
// OBSERVER of orders, never a gate in front of them, so an order could be inside
// every mandate rule and still take the portfolio through its VaR limit. This
// folds the measures risk already publishes so the gate can check them as
// arithmetic on a local map — no call, no added latency, and a degraded risk
// engine makes this view STALE (a refusal) rather than making the OMS wait on it
// (an outage).
//
// # Why this is a file rather than ninety lines in runConsumers
//
// Three refusals now share this view, and each one is a DIFFERENT INCIDENT with a
// different fix and a different person. Merged, they read as "the risk limit is
// refusing" with nothing to act on; the counters below are what separate them,
// and the paragraph attached to each counter is the operational half of the fix.
// test/arch/composition_root_length_test.go ratchets runConsumers, and it refused
// the third one — the same way it paid for #713's and #1007's wiring — so the
// wiring moved here instead of the budget going up.
//
// # The three refusals, and why none of them is an annotation
//
//   - STALE. A risk number that cannot be shown current is not a risk number.
//   - UNRESOLVED (#509, #527). The engine computed the measure over an incomplete
//     book, so the value is an unknown rather than a smaller one — dropping
//     positions shrinks a sum but RAISES a ratio or a portfolio quantile.
//   - PLACEHOLDER (#1037). The engine computed over the WHOLE book, with a model
//     that is an illustrative constant.
//
// All three leave the measure UNKNOWN, which the mandate rules already fail
// closed on. That is deliberate and it is the same ruling each time: a rule
// comparing a limit against a number nobody can vouch for is not conservative, it
// is arbitrary.
func newRiskFold(reg prometheus.Registerer, logger *slog.Logger) *riskview.View {
	// A MEASURE THE ENGINE COULD NOT COMPUTE IS COUNTED, NOT JUST DROPPED (#509).
	//
	// The view declines to fold a measure whose coverage says it was computed over
	// an incomplete book, so a risk limit over it refuses. That refusal otherwise
	// looks identical to an engine that never published — and the two need
	// different people: a stale view is a risk-engine incident, this is a
	// REFERENCE-DATA one. The contract-terms store has no production writer today,
	// so the fixed-income measures are the population this counts.
	//
	// Zero on registration, so "no unresolved measure has ever arrived" is a
	// reading rather than an absent series.
	unresolvedMeasures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_risk_measures_unresolved_total",
		Help: "Announced risk measures this OMS refused to fold because the engine computed " +
			"them over an incomplete book (domain.v1.RiskMeasure.coverage). Non-zero means a " +
			"risk-limit mandate over that measure is REFUSING orders, and the fix is upstream " +
			"reference data, not the risk engine.",
	})

	// A MEASURE FROM A PLACEHOLDER MODEL IS COUNTED, NOT JUST DROPPED (#1037).
	//
	// The engine can be healthy, current and answering over the whole book, and
	// still be answering with an illustrative constant: compute.VaR99 is
	// 0.01 × GrossExposure, and it is what every risk-engine pod without
	// RISK_ENGINE_MARKETDATA_DATABASE_URL serves — which is every manifest in
	// infra/. The view refuses to fold it, so a mandate's VaR limit refuses.
	//
	// A SEPARATE SERIES FROM THE ONE ABOVE, because a separate person fixes it.
	// Unresolved is a reference-data incident; this is a DEPLOYMENT one, and the
	// repair is a manifest that points the risk engine at market-data's price
	// store. Merged into one counter, the fix nobody could find would be the one
	// that costs money: before this refusal existed the number folded silently and
	// admitted orders a real VaR would have refused.
	//
	// Zero on registration, so "no placeholder measure has ever arrived" is a
	// reading rather than an absent series.
	placeholderMeasures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_risk_measures_placeholder_total",
		Help: "Announced risk measures this OMS refused to fold because the risk engine produced " +
			"them with a PLACEHOLDER model (domain.v1.MeasureProvenance.method). Non-zero means a " +
			"risk-limit mandate over that measure is REFUSING orders, and the fix is the risk " +
			"engine's deployment configuration — not its data and not this service.",
	})
	reg.MustRegister(unresolvedMeasures, placeholderMeasures)

	risk := riskview.New(
		riskview.WithOnStale(func(portfolioID, measure string, age time.Duration) {
			logger.Warn("oms: a risk measure is too old to gate on — orders under a risk-limit "+
				"mandate will be REFUSED for this portfolio until the engine publishes again",
				"portfolio_id", portfolioID, "measure", measure, "age", age.String(),
				"subject", riskview.Subject)
		}),
		riskview.WithOnUnresolved(func(portfolioID, measure string, excluded uint32) {
			unresolvedMeasures.Inc()
			logger.Warn("oms: a risk measure was announced having been computed over an "+
				"INCOMPLETE BOOK and will not gate anything — orders under a mandate naming it "+
				"will be REFUSED until the engine can resolve its inputs",
				"portfolio_id", portfolioID, "measure", measure, "excluded_positions", excluded,
				"subject", riskview.Subject)
		}),
		riskview.WithOnPlaceholder(func(portfolioID, measure, method string) {
			placeholderMeasures.Inc()
			logger.Warn("oms: a risk measure was announced from a PLACEHOLDER MODEL and will not "+
				"gate anything — the number is an illustrative constant, not a calibrated risk "+
				"figure, so orders under a mandate naming it will be REFUSED until the risk "+
				"engine is configured with a real model",
				"portfolio_id", portfolioID, "measure", measure, "method", method,
				"subject", riskview.Subject,
				"fix", "set RISK_ENGINE_MARKETDATA_DATABASE_URL on the risk-engine deployment")
		}),
	)

	// HELD vs CURRENT, because the difference is what an operator needs BEFORE a
	// risk limit starts refusing everything. A portfolio held but not current is
	// one whose gate is about to fail closed, and that is a different incident
	// from one the engine has never computed.
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_risk_portfolios_held",
		Help: "Portfolios whose risk measures this OMS has folded at all.",
	}, func() float64 { held, _ := risk.Stats(); return float64(held) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_risk_portfolios_current",
		Help: "Portfolios whose risk measures are within OMS freshness — the ones a declared " +
			"risk limit can actually be checked against. held − current is the population whose " +
			"orders a risk mandate will refuse.",
	}, func() float64 { _, live := risk.Stats(); return float64(live) }))

	return risk
}
