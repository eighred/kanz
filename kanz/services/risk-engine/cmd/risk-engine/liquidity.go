package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/liquidity"
	"github.com/eighred/kanz/internal/risk/liquiditysource"
	"github.com/eighred/kanz/services/risk-engine/internal/config"
)

// THE LIQUIDITY MEASURES (#509) — the last of the dark compute seams.
//
// compute.RegisterLiquidityRisk had no caller anywhere in the module, so
// LiquidationHorizon and LVaR99 were implemented, benchmarked (#471) and
// unreachable: engine.filterMeasures drops unknown names, so a client asking for
// LiquidationHorizon got 200 with the measure absent — indistinguishable from a
// book the engine decided held nothing worth measuring.
//
// # It lives in its own file, and is a function, because main.go is untested
//
// Composition-root wiring escapes every unit test in this repository, and this
// service has twice shipped a crashing root with a green suite. So the wiring is
// a function with a return value rather than forty inline lines, and
// liquidity_test.go grades every decision below against a real store.Memory —
// the same shape as market-data's rollupJobs, for the same reason. That file
// also parses main.go and asserts runEngine still CALLS this.
//
// THAT LAST ASSERTION IS NOT REDUNDANT, and the measurement is worth recording
// because it is finer than it looks. Deleting the call from runEngine was tried:
//
//   - test/arch's TestNoMeasureSeamIsDarkAndUntracked stays GREEN. It resolves
//     callers of compute.RegisterLiquidityRisk and THIS FILE IS ONE, so a seam
//     called only by another unreachable seam still counts as live — the exact
//     weakness that guard's own doc names, now confirmed rather than theorised.
//   - test/arch's TestEveryRegisteredMetricHasAWriter DOES fail, because the
//     only writer of kanz_risk_liquidity_unresolved_total is inside this
//     function and nothing else references it.
//
// So the estate is not blind to it — but it is caught SIDEWAYS, by a metric
// guard, and only because this wiring happens to register a metric. That is a
// property of this file, not of the rule, and it would not survive someone
// moving the counters. The AST assertion checks the thing itself.
//
// # ONE MEASURE IS REGISTERED, NOT TWO, AND THAT IS THE HONEST OUTCOME
//
// LiquidationHorizon reads ADV only, and ADV is measured off the durable bar
// series — a real statistic over a real tape.
//
// LVaR99 needs a SPREAD, and nothing in this repository persists a bid or an ask
// (the evidence is enumerated in liquiditysource's package doc: the ingest path
// folds market.v1.Quote to a mid and discards both sides, no migration creates a
// bid/ask/spread column, marketedge/book is in-memory in one process). Under
// WithNoSpread every spread is zero, CostFraction is zero, and LVaR99 would equal
// VaR99 exactly on every book forever — so compute declines to register it and
// says so once through the observer. An absent LVaR99 is the correct answer; a
// present one that never adjusts is a measure whose name is a claim it cannot
// make.
//
// # WHY NO ENV VAR FOR ASSUMED SPREADS, WHICH WOULD MAKE LVaR99 APPEAR
//
// liquiditysource.WithAssumedSpreads exists and takes desk-entered per-instrument
// spreads. It is deliberately NOT exposed here, and the reason is not squeamishness
// about config:
//
//   - A PARTIAL TABLE IS SILENT AND IT DAMAGES THE MEASURE THAT IS CURRENTLY
//     HONEST. With WithAssumedSpreads alone, an instrument ABSENT from the map is
//     refused outright (ReasonNoAssumedSpread) — so it leaves LiquidationHorizon
//     too, which needs no assumption at all. A desk that enters spreads for its ten
//     largest names would silently reduce the whole book's reported liquidation
//     horizon to the horizon of those ten. Adding WithNoSpread alongside repairs
//     that, but then ServesSpread() is true on the strength of ONE entry, LVaR99 is
//     registered, and it adjusts only the typed names while reading as a whole-book
//     adjustment. compute.SkipNoLiquidationCost cannot catch it — it fires only on
//     EXACT equality with VaR99, and a partial adjustment is not exact. Both
//     spellings of "half-filled table" are wrong in the direction that flatters,
//     and this layer cannot tell which one an operator meant.
//   - THE UNIT CANNOT BE MADE SAFE FROM HERE. liquiditysource.MaxAssumedSpread
//     refuses a value at or above 1.0 precisely to catch basis points entered where
//     a fraction was wanted. A composition root that parsed "INSTRUMENT=5" as 5bps
//     would divide by 10,000 first, which puts every value a human could plausibly
//     type below the bound — disabling the library's only defence against that
//     typo in order to make a config file read nicely.
//   - NOTHING IN THIS ESTATE PRODUCES THE NUMBERS. No manifest would set the var
//     and no job would fill it, so it would ship as configuration whose FIRST use
//     is unrehearsed — and its first use decides whether a measure exists at all.
//
// When a deployment genuinely has externally-sourced spread assumptions, the
// change is an option here plus the evidence of where the numbers came from. Until
// then LVaR99 stays absent and kanz_risk_liquidity_skipped_total{reason=
// "no_spread_source"} is what says the family shipped half-served on purpose.

// registerLiquidityRisk wires the LIQ-01d measures over the durable bar series,
// or reports why it did not.
//
// It returns an error only for a configuration that ASKED for liquidity and
// cannot have it — a named venue with no bar store, or a provider construction
// liquiditysource refuses. Those are startup failures by design: every one of
// them produces the same runtime symptom (every position skipped, uniformly,
// forever) which at the measure is indistinguishable from a portfolio that holds
// nothing. An unset venue is not an error — it is a deployment that did not ask —
// and it returns nil after a WARN.
func registerLiquidityRisk(
	ctx context.Context,
	cfg config.Config,
	registry *compute.Registry,
	bars liquiditysource.BarStore,
	reg prometheus.Registerer,
	logger *slog.Logger,
) error {
	if cfg.LiquidityVenue == "" {
		// NOT SILENT. Two implemented measures are absent and the reason is a
		// missing config value rather than a defect. There is no default to fall
		// back to: summing ADV across venues reports a liquidation horizon
		// shorter than any single book supports, so a guessed venue would be an
		// unwind assumption made on the operator's behalf.
		logger.Warn("LIQ-01d NOT registered: LiquidationHorizon needs a venue to measure ADV from, "+
			"and RISK_ENGINE_LIQUIDITY_VENUE is unset. There is no default because ADV summed "+
			"across venues asserts a simultaneous multi-venue unwind and reports a horizon three "+
			"times shorter than any single book supports — the direction that makes a limit check "+
			"pass",
			"fix", "set RISK_ENGINE_LIQUIDITY_VENUE to the MIC whose candles this book trades on",
			"gauge", "kanz_risk_measure_live{family=\"liquidity\"}=0")
		return nil
	}
	if bars == nil {
		// THE OPERATOR ASKED AND THERE IS NOTHING TO READ. Degrading here would
		// leave a deployment that named a venue looking exactly like one that
		// never wanted liquidity, which is the shape AGENTS.md forbids. Nothing
		// sets this variable today, so the only way to reach this line is to have
		// just added it — where a crash-loop is cheap and a silent dark family is
		// not.
		return fmt.Errorf("RISK_ENGINE_LIQUIDITY_VENUE=%q names a venue to measure ADV at, and "+
			"RISK_ENGINE_MARKETDATA_DATABASE_URL is unset so there is no bar series to measure it "+
			"from. Set the market-data DSN, or unset the venue and accept a dark liquidity family",
			cfg.LiquidityVenue)
	}

	// THE COUNTERS ARE CREATED HERE, NOT BESIDE fiSkipped, and only on the path
	// that actually registers a measure. A zeroed counter next to an unregistered
	// measure reads as "wired, and quiet" — the state this whole family of metrics
	// exists to distinguish from "not wired at all". When the venue is unset the
	// series are ABSENT and kanz_risk_measure_live is the signal.
	//
	// TWO COUNTERS, NOT ONE, because the two reason sets answer different
	// questions and share no values. The measure-level one is about the BOOK (is
	// anything liquid, did the adjustment come out zero); the provider-level one
	// is about the SERIES (is anything ingested, at this venue, far enough back).
	// Merging them would put "the rollup is still backfilling" in the same number
	// as "this book has nothing liquid in it".
	skipped := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_risk_liquidity_skipped_total",
		Help: "Liquidity-measure evaluations that reported zero for something that is not a flat " +
			"book, by reason. no_spread_source = LVaR99 was never registered because nothing here " +
			"persists a bid or an ask (fires once, at startup). no_liquid_horizon = the whole book " +
			"resolved to nothing liquid, so LiquidationHorizon reports 0.0000 days — the MOST " +
			"LIQUID answer the measure can give, for a book about which nothing is known. illiquid " +
			"= one name's ADV was non-positive and it was dropped from the weighted horizon, so a " +
			"book holding one unliquidatable name still reports a comfortable number (#509).",
	}, []string{"reason"})
	unresolved := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_risk_liquidity_unresolved_total",
		Help: "Per-instrument liquidity resolutions the bar series could not answer, by reason. " +
			"Upstream every one of these is ANONYMOUS — liquidity.Profile just drops the position " +
			"— so no_bars (nothing ingested, or the wrong venue), insufficient_history (a new " +
			"listing), unusable_volume (a stored-data defect) and store_error arrive at the measure " +
			"as one absent position with four different fixes. coarse_series_empty and " +
			"coarse_series_short are NOT refusals: a preferred rollup could not answer and the " +
			"finer series was tried, which SUCCEEDS — read them as 'series skipped', never as " +
			"'instruments refused', or the day the 1d rollup is deployed will read as a total " +
			"outage. spread_unavailable accompanies a SUCCESS and is the one with no downstream " +
			"symptom at all (#509). window_not_whole ALSO accompanies a success and is the " +
			"newest of these (#591): the ADV was computed over days the bar series does not " +
			"wholly cover, so it is understated — conservative for a liquidation horizon, and " +
			"still a number measured over less than it claims.",
	}, []string{"reason"})
	reg.MustRegister(skipped, unresolved)

	// EVERY REASON GETS A SERIES AT ZERO, matching fiSkipped and factorSkipped in
	// main.go. A counter that only appears on its first increment reads as no-data
	// to an alert, so the alert cannot fire on the transition from none to some —
	// which is the transition that matters.
	for _, r := range []string{
		compute.SkipNoSpreadSource, compute.SkipIlliquid,
		compute.SkipNoLiquidHorizon, compute.SkipNoLiquidationCost,
	} {
		skipped.WithLabelValues(r).Add(0)
	}
	for _, r := range []string{
		liquiditysource.ReasonNoAsOf, liquiditysource.ReasonNoAssumedSpread,
		liquiditysource.ReasonSpreadUnavailable, liquiditysource.ReasonNoBars,
		liquiditysource.ReasonCoarseSeriesEmpty, liquiditysource.ReasonCoarseSeriesShort,
		liquiditysource.ReasonInsufficientHistory, liquiditysource.ReasonUnusableVolume,
		liquiditysource.ReasonStoreError, liquiditysource.ReasonWindowNotWhole,
	} {
		unresolved.WithLabelValues(r).Add(0)
	}

	provider, err := liquiditysource.FromBars(bars, liquiditysource.Config{
		Venue: cfg.LiquidityVenue,
		// A PREFERENCE, NOT A PIN, and never both — FromBars refuses a config
		// that sets Resolution as well, because two spellings of one decision
		// mean the decision was not made.
		//
		// Pinning 1d would be a bet that the rollup exists, is populated and has
		// backfilled the whole 28-day window. Lose that bet and every instrument
		// is refused, liquidity.Profile drops every position, and
		// LiquidationHorizon reports 0.0000 days — an ACTIVE claim of perfect
		// liquidity for a book nobody measured, with no downstream symptom: the
		// measure is served, it is in range, and it is the most flattering value
		// it can take. market-data began writing the 1d series only recently
		// (#524), so a fresh environment is exactly the half-built case.
		//
		// CoarsestFirst degrades the other way. A missing or still-backfilling
		// rollup costs one index seek that returns nothing, and the ADV comes off
		// the 1m base series — the SAME NUMBER (dailyTotals counts distinct UTC
		// days, so the statistic is invariant under the resolution it is measured
		// from), at the old read cost, with coarse_series_empty /
		// coarse_series_short counting how often the optimisation was skipped.
		ResolutionPreference: liquiditysource.CoarsestFirst(),
	},
		// SPELLED, BECAUSE FromBars REFUSES TO DEFAULT IT. See the file doc for
		// why the other posture (WithAssumedSpreads) is not exposed as config.
		// This one makes ServesSpread() false, which is what keeps the degenerate
		// LVaR99 off the wire.
		liquiditysource.WithNoSpread(),
		// COUNTED BY REASON, NEVER LABELLED BY INSTRUMENT. An instrument-id label
		// is unbounded cardinality — one series per instrument the estate has
		// ever held — and the question an operator has is "is the book falling
		// out of the liquidity measures", which a count answers. Which names is a
		// log's job.
		//
		// THE DAY COUNT IS DELIBERATELY DROPPED. It is meaningful only for
		// insufficient_history / coarse_series_short, and turning it into a label
		// would open one series per distinct day count. The question it would
		// answer — how far has the 1d rollup backfilled — wants a histogram, which
		// is a second metric with its own evidence and is not being smuggled in
		// here. Stated so the drop is a decision rather than an oversight.
		liquiditysource.WithObserver(func(_, reason string, _ int) {
			unresolved.WithLabelValues(reason).Inc()
		}),
	)
	if err != nil {
		return err
	}

	compute.RegisterLiquidityRisk(ctx, registry, provider, liquidity.DefaultModel(),
		// nil baseVaR ⇒ the REGISTRY's VaR99, resolved at EVALUATION time. Passing
		// compute.VaR99 explicitly would pin LVaR to the RISK-07 1%×gross
		// placeholder while the engine served the historical-simulation model —
		// two different VaR numbers in one response, with LVaR99 ≥ VaR99 no longer
		// guaranteed. Late binding also makes this call order-independent, so it
		// does not matter that varmodel.Register ran above.
		nil,
		compute.WithLiquidityObserver(func(_, reason string) {
			skipped.WithLabelValues(reason).Inc()
		}))

	// WHICH OF THE TWO SHIPPED, ASKED OF THE PROVIDER rather than re-derived from
	// the config above. compute asks the same question the same way, so the log
	// and the registry cannot disagree — a second copy of the posture here is how
	// a future edit would announce a measure the engine does not serve.
	if compute.ServesSpread(provider) {
		logger.Info("LIQ-01d: LiquidationHorizon + LVaR99 registered off the durable bar series",
			"venue", cfg.LiquidityVenue, "resolution_preference", liquiditysource.CoarsestFirst(),
			"window", liquiditysource.DefaultWindow, "min_days", liquiditysource.DefaultMinDays)
		return nil
	}
	logger.Info("LIQ-01d: LiquidationHorizon registered off the durable bar series",
		"venue", cfg.LiquidityVenue, "resolution_preference", liquiditysource.CoarsestFirst(),
		"window", liquiditysource.DefaultWindow, "min_days", liquiditysource.DefaultMinDays)
	logger.Warn("LVaR99 NOT registered: this deployment serves no spread, so a liquidity-ADJUSTED "+
		"VaR would equal VaR99 exactly on every book forever. Nothing in this platform persists a "+
		"bid or an ask — market.v1.Quote is folded to a mid at ingest and both sides are "+
		"discarded — so the only honest source is a desk-entered assumption, which this "+
		"composition root deliberately does not accept from the environment",
		"absent_measure", compute.MeasureLVaR99,
		"counter", "kanz_risk_liquidity_skipped_total{reason=\""+compute.SkipNoSpreadSource+"\"}")
	return nil
}
