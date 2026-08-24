package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/execution"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/spotsource"
	"github.com/eighred/kanz/internal/venuemargin"
	"github.com/eighred/kanz/services/risk-engine/internal/config"
)

// THE MARGIN MEASURE (#408 control 4) — the last of the set's four controls, and
// the one that turns margin from a report into a control.
//
// compute.RegisterMarginRisk had no caller anywhere in the module, so
// LiquidationProximity was implemented, tested and unreachable:
// engine.filterMeasures drops unknown names, so a mandate naming it produced a
// RiskLimitRule refusal ("measure unknown or not current") indistinguishable
// from a risk engine that had never announced the portfolio at all. The measure
// existed; nothing could gate on it.
//
// # The three facts this join needs, and why no one package holds them
//
//   - WHICH ACCOUNTS BACK THIS PORTFOLIO. execution.AccountBindings, deploy-time
//     configuration. A venue adapter holds one API credential and therefore IS
//     one exchange account; it cannot know which portfolio that account backs,
//     so the mapping cannot arrive with the observation.
//   - WHAT THE EXCHANGE SAYS ABOUT THAT ACCOUNT. venuemargin.View, folded from
//     the same accounting.margin.observed FACT the OMS's pre-trade gate reads
//     (control 1). Not a second poll of the venue, and never a recomputation
//     from our own book: our reconstruction of the exchange's margin maths is
//     exactly the "stale book" #408 is built on.
//   - WHAT THE POSITION IS WORTH NOW. spotsource, reading the market-data store
//     EXACTLY — the mark is a denominator here rather than a pricing input, so
//     it is a *big.Rat and never a float.
//
// # It lives in its own file, and is a function, because main.go is untested
//
// The same reasoning liquidity.go records: composition-root wiring escapes every
// unit test in this repository, and this service has twice shipped a crashing
// root with a green suite. margin_test.go grades every decision below without a
// broker or a database, and asserts runEngine still CALLS this.
//
// # THE MARK IS READ AT THE OBSERVATION'S OWN TIME, NOT AT NOW
//
// The proximity is one price over another, and two prices from different moments
// are not a ratio of anything. The venue's liquidation price was true at the
// instant the exchange reported it, so the mark is read as of that same instant —
// which the bitemporal store answers exactly, bounding knowledge time too, so a
// vendor correction stamped afterwards cannot leak backwards into a number an
// operator has already acted on.
//
// # WHAT THIS DELIBERATELY DOES NOT DO
//
// It does not fall back to a mark of any age, and it does not substitute a
// liquidation price of our own for a venue that did not report one. Either would
// produce a number for every position on every book — and a proximity computed
// over inputs nobody had is the "confident zero" this estate has repaired four
// times in two days, on the one measure where being wrong costs the fund its
// collateral.

// marginFold is the read this file needs from the venue-margin fold, declared at
// the consumer so margin_test.go needs no bus.
//
// ONE METHOD, AND IT RETURNS THE COVERAGE WITH THE POSITIONS. venuemargin makes
// that structural for an enumerating caller: positions from one observation
// beside a completeness record from another would be the stale book with an
// extra step, and this seam cannot express that pairing even by accident.
type marginFold interface {
	Liquidations(venue, account string) ([]venuemargin.Liquidation, venuemargin.Coverage, bool)
}

// markResolver is the exact-decimal mark read, satisfied by *spotsource.Provider.
//
// ok=false is UNKNOWN and the caller must leave the mark nil — never zero, which
// would put the liquidation boundary infinitely far away and report the one
// position whose price could not be resolved as the safest thing on the book.
type markResolver interface {
	Mark(ctx context.Context, instrumentID string, asOf time.Time) (*big.Rat, bool)
}

// marginProvider is the production compute.MarginProvider: it resolves a
// portfolio's venue accounts, reads what the exchange said about each, and pairs
// every open position with a contemporaneous mark.
type marginProvider struct {
	tenant   string
	bindings *execution.AccountBindings
	fold     marginFold
	marks    markResolver
}

var _ compute.MarginProvider = (*marginProvider)(nil)

// AccountMargins implements compute.MarginProvider.
//
// ok=false means the platform cannot say WHICH accounts back this portfolio, and
// the measure excludes the whole book on it. That is the honest answer for an
// unbound portfolio: with no binding the OMS may still route its orders against
// whatever account the adapter holds, alongside every other unbound portfolio —
// a shared collateral pool, in which no account's margin can be said to back
// THIS portfolio. #408 calls that control 2 becoming load-bearing the moment
// margin is switched on, and this is where it bears.
func (p *marginProvider) AccountMargins(ctx context.Context, portfolioID v1.PortfolioID) ([]compute.AccountMargin, bool) {
	if p == nil || p.fold == nil {
		return nil, false
	}
	accounts := p.bindings.AccountsFor(p.tenant, string(portfolioID))
	if len(accounts) == 0 {
		return nil, false
	}
	out := make([]compute.AccountMargin, 0, len(accounts))
	for _, acct := range accounts {
		out = append(out, p.accountMargin(ctx, acct))
	}
	return out, true
}

// accountMargin reads one account. EVERY REFUSAL IS A STATE ON THE RETURNED
// VALUE, never a dropped element: an account missing from the slice is one the
// measure never considers, and no exclusion record would name it.
func (p *marginProvider) accountMargin(ctx context.Context, acct execution.VenueAccount) compute.AccountMargin {
	am := compute.AccountMargin{Venue: acct.MIC, Account: acct.Account}
	positions, coverage, current := p.fold.Liquidations(acct.MIC, acct.Account)
	if !current {
		// Read stays false: never observed, no margin source at the venue, or
		// aged past venuemargin.DefaultMaxAge. All three are one answer here for
		// the reason venuemargin gives — a caller that could tell them apart
		// would eventually pass one of them.
		return am
	}
	am.Read = true
	am.CoverageReported = coverage.Reported()
	am.ExcludedCount = coverage.ExcludedCount()
	// LENGTH-ZERO AND NON-NIL ARE THE SAME ANSWER HERE, and it is a positive one:
	// a read, current, complete observation with no positions is the exchange
	// saying this account holds nothing leveraged, on which an exact zero is
	// honest. Every other combination carries a state above that refuses.
	am.Positions = make([]compute.LiquidationRef, 0, len(positions))
	for _, pos := range positions {
		am.Positions = append(am.Positions, p.liquidationRef(ctx, pos))
	}
	return am
}

// liquidationRef pairs one reported position with its mark.
//
// AN UNATTRIBUTED POSITION IS KEPT, WITH NO MARK. The venue reported a leveraged
// position whose symbol this deployment's map does not carry; it is real, it can
// be liquidated, and dropping it here would remove from the measure exactly the
// position nobody is watching — while the account's coverage, which the adapter
// filled with unmapped_venue_symbol, described an account that was otherwise
// fine. Kept, it reaches the measure as an exclusion naming the venue symbol.
func (p *marginProvider) liquidationRef(ctx context.Context, pos venuemargin.Liquidation) compute.LiquidationRef {
	ref := compute.LiquidationRef{
		InstrumentID: v1.InstrumentID(pos.InstrumentID),
		VenueSymbol:  pos.VenueSymbol,
		Liquidation:  pos.Price.Value(),
	}
	if pos.InstrumentID == "" || p.marks == nil {
		return ref
	}
	// AS OF THE OBSERVATION, NOT NOW — see the file doc. Quantity is what makes
	// this possible without a second lookup: the price cannot be read apart from
	// the instant the venue reported it.
	if mark, ok := p.marks.Mark(ctx, pos.InstrumentID, pos.Price.ObservedAt()); ok {
		ref.Mark = mark
	}
	return ref
}

// registerMarginRisk wires LiquidationProximity over the exchange's own margin
// observations, or reports why it did not.
//
// It returns the fold the caller must subscribe to venuemargin.Subject, or nil
// when the measure was not registered — so a deployment that binds no accounts
// starts no subscription, and there is no folded state nobody reads.
//
// It returns an error only for a deployment that ASKED for the margin measure and
// cannot have it: bindings naming accounts with no price store to mark their
// positions against. That is a startup failure by design, and it is the judgement
// liquidity.go makes for the same reason — the runtime symptom would be every
// position excluded, uniformly, forever, which at the measure is
// indistinguishable from a book holding nothing leveraged.
//
// NO BINDINGS IS NOT AN ERROR. It is a deployment that did not ask, and it
// registers NOTHING (compute.RegisterMarginRisk with a nil provider), which is
// what keeps kanz_risk_measure_live{family="margin"} at 0. A registered measure
// that refuses on every portfolio forever reads as live, and an operator would
// have no signal that nothing was ever wired.
func registerMarginRisk(
	ctx context.Context,
	cfg config.Config,
	registry *compute.Registry,
	prices spotsource.PriceStore,
	reg prometheus.Registerer,
	logger *slog.Logger,
) (*venuemargin.View, error) {
	// RE-PARSED, NOT THREADED FROM config.Load. Load validates the spec so a
	// shared account refuses the start before any I/O; this needs the bindings
	// themselves. ParseBindings is pure and the string is already in hand, so the
	// alternative — a *AccountBindings on Config — would put a live control's
	// state in a value that is copied, compared and logged as configuration.
	bindings, err := execution.ParseBindings(cfg.VenueAccounts)
	if err != nil {
		return nil, err
	}

	if bindings.Empty() {
		// NOT SILENT, AND NOT AN ERROR. #408's set is deliberately ordered, and a
		// deployment that has not yet provisioned a sub-account per portfolio is
		// at a legitimate point in that order — but it must not believe it holds a
		// liquidation control.
		//
		// NO COUNTER IS CREATED ON THIS PATH. A zeroed
		// kanz_risk_margin_skipped_total beside an unregistered measure reads as
		// "wired, and quiet", which is the state this family of metrics exists to
		// distinguish from "not wired at all" (liquidity.go states the same rule).
		// The absent series and the measure-live gauge are the signal.
		compute.RegisterMarginRisk(ctx, registry, nil)
		logger.Warn("MARGIN-01 NOT registered: LiquidationProximity needs to know which exchange "+
			"accounts back each portfolio, and RISK_ENGINE_VENUE_ACCOUNTS is empty. A mandate "+
			"naming this measure REFUSES every order it checks, because a limit on a measure the "+
			"engine does not produce is a refusal (#408 control 4)",
			"fix", "set RISK_ENGINE_VENUE_ACCOUNTS to the same bindings the OMS spends from",
			"absent_measure", compute.MeasureLiquidationProximity,
			"gauge", "kanz_risk_measure_live{family=\"margin\"}=0")
		return nil, nil
	}
	if prices == nil {
		// THE OPERATOR ASKED AND THERE IS NOTHING TO MARK AGAINST. Degrading here
		// would leave a deployment that bound accounts looking exactly like one
		// that never wanted the measure.
		return nil, fmt.Errorf("RISK_ENGINE_VENUE_ACCOUNTS binds %d exchange account(s) and there "+
			"is no price store to mark their positions against. LiquidationProximity divides the "+
			"venue's own liquidation price by a mark, so every open position would be excluded, "+
			"uniformly and forever — which at the measure is indistinguishable from a book holding "+
			"nothing leveraged. Set RISK_ENGINE_MARKETDATA_DATABASE_URL, or clear the bindings and "+
			"accept a dark margin family", bindings.Len())
	}

	skipped := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_risk_margin_skipped_total",
		Help: "LiquidationProximity evaluations that declined to answer, by reason. " +
			"no_margin_accounts = a portfolio is bound to no venue account, so there is no " +
			"liquidation boundary to measure a distance to — it is NOT an unlevered portfolio. " +
			"margin_unknown = an account's margin state was never observed or has aged past " +
			venuemargin.DefaultMaxAge.String() + "; A NON-ZERO VALUE HERE IS ALSO WHAT A BINDING " +
			"SET DISAGREEING WITH THE ONE THE OMS SPENDS FROM LOOKS LIKE, because the account it " +
			"names is then one no venue adapter observes. margin_coverage_not_reported / " +
			"margin_incomplete = the exchange's answer cannot be shown to be whole, and the part " +
			"it left out may be the position nearest the boundary. no_liquidation_price / " +
			"no_mark_price / mark_not_positive / liquidation_price_negative = one position could " +
			"not be turned into a distance, and the account is excluded rather than measured over " +
			"the rest. no_margin_provider is ABSENT from this metric by construction: it fires " +
			"only when the measure was never registered, and this series does not exist on that " +
			"path.",
	}, []string{"reason"})
	uncovered := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_risk_venue_margin_uncovered_total",
		Help: "Venue margin observations folded with the exchange having failed to answer part of " +
			"the account's state. Those quantities are UNKNOWN, not zero, and the account is " +
			"excluded from LiquidationProximity whole — the missing part may be the liquidation " +
			"price of the position nearest the boundary.",
	})
	undated := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_risk_venue_margin_undated_total",
		Help: "Venue margin observations discarded for carrying no observation time. An undated " +
			"figure cannot be judged against a freshness bound, so it is dropped and the account " +
			"ages out to UNKNOWN — which refuses rather than reporting a comfortable number.",
	})
	reg.MustRegister(skipped, uncovered, undated)
	// EVERY REASON GETS A SERIES AT ZERO, matching fiSkipped and factorSkipped. A
	// counter that only appears on its first increment reads as no-data to an
	// alert, so the alert cannot fire on the transition from none to some — which
	// is the transition that matters.
	for _, r := range []string{
		compute.SkipNoMarginAccounts, compute.SkipMarginUnknown, compute.SkipMarginUncovered,
		compute.SkipMarginIncomplete, compute.SkipNoLiquidationPrice, compute.SkipLiquidationNegative,
		compute.SkipNoMark, compute.SkipMarkNotPositive, compute.SkipProximityNotRepresentable,
	} {
		skipped.WithLabelValues(r).Add(0)
	}

	unresolved := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_risk_margin_mark_unresolved_total",
		Help: "Marks the price store could not answer for a position the venue reported a liquidation " +
			"price for, by reason. Each one arrives at the measure as no_mark_price, which excludes the " +
			"whole account — so this is the metric that says WHY: no_observation (nothing ingested for " +
			"the instrument), stale (a mark older than the margin freshness bound at the moment the " +
			"venue reported), unusable_price (a stored-data defect), store_error, no_as_of.",
	}, []string{"reason"})
	reg.MustRegister(unresolved)
	for _, r := range []string{
		spotsource.ReasonNoAsOf, spotsource.ReasonNoObservation, spotsource.ReasonStale,
		spotsource.ReasonUnusablePrice, spotsource.ReasonStoreError,
	} {
		unresolved.WithLabelValues(r).Add(0)
	}

	// THE MARK IS HELD TO THE MARGIN PLANE'S FRESHNESS BOUND, NOT THE PRICING
	// PLANE'S.
	//
	// spotsource.DefaultMaxAge is seven days, which suits an option's underlying:
	// a greek off a week-old spot is wrong but recognisably so, and the alternative
	// is no greek at all. A liquidation distance is the opposite case. Its
	// numerator is at most venuemargin.DefaultMaxAge old BY CONSTRUCTION, and the
	// scenario the whole #408 set guards against is a fast move — so a week-old
	// denominator would report the distance that was true before the move that is
	// about to liquidate the account, which is the "stale book" the issue is built
	// on wearing the other half of the ratio.
	//
	// The estate's ingestion stamps PriceKindClose on every tick rather than once a
	// day (internal/marketdata/ingest.go), so this bound is met by a live feed and
	// missed by a stopped one — which is exactly the discrimination it is for.
	marks, err := spotsource.FromStore(prices,
		spotsource.WithMaxAge(venuemargin.DefaultMaxAge),
		spotsource.WithUnresolvedObserver(func(_, reason string, _ time.Duration) {
			unresolved.WithLabelValues(reason).Inc()
		}),
	)
	if err != nil {
		return nil, err
	}

	fold := venuemargin.New(
		venuemargin.WithOnStale(func(venue, account string, age time.Duration) {
			logger.Warn("risk-engine: the exchange's margin state for this account is too old to "+
				"act on — LiquidationProximity is UNKNOWN for every portfolio bound to it, so a "+
				"mandate naming the measure REFUSES",
				"venue", venue, "account", account, "age", age.String(),
				"max_age", venuemargin.DefaultMaxAge.String(), "subject", venuemargin.Subject)
		}),
		venuemargin.WithOnUncovered(func(venue, account string, excluded uint32) {
			uncovered.Inc()
			logger.Warn("risk-engine: the exchange did not report part of this account's margin "+
				"state — the account is excluded from the liquidation measure whole, because the "+
				"part it left out may be the position nearest the boundary",
				"venue", venue, "account", account, "excluded", excluded, "subject", venuemargin.Subject)
		}),
		venuemargin.WithOnUndated(func(venue, account string) {
			undated.Inc()
			logger.Warn("risk-engine: discarded an UNDATED margin observation — a figure of unknown "+
				"age cannot be judged against a freshness bound, so the account will age out to "+
				"UNKNOWN", "venue", venue, "account", account, "subject", venuemargin.Subject)
		}),
	)
	// HELD vs CURRENT, the same pair the OMS exports and for the same reason: the
	// gap between them is the population whose LiquidationProximity is about to
	// become UNKNOWN, and it is visible minutes before anybody asks why a mandate
	// started refusing.
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_risk_venue_margin_accounts_held",
		Help: "Exchange accounts whose venue margin state this engine has folded at all. ZERO " +
			"MEANS NOTHING IS OBSERVING MARGIN, not that the accounts are safe.",
	}, func() float64 { held, _ := fold.Stats(); return float64(held) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_risk_venue_margin_accounts_current",
		Help: "Exchange accounts whose folded margin state is still within " +
			venuemargin.DefaultMaxAge.String() + ". BELOW kanz_risk_venue_margin_accounts_held " +
			"means those accounts' liquidation proximity is UNKNOWN and every mandate naming the " +
			"measure is refusing for them.",
	}, func() float64 { _, live := fold.Stats(); return float64(live) }))

	compute.RegisterMarginRisk(ctx, registry, &marginProvider{
		tenant:   cfg.Tenant,
		bindings: bindings,
		fold:     fold,
		marks:    marks,
	}, compute.WithMarginObserver(func(_, reason string) {
		// COUNTED BY REASON, NEVER LABELLED BY INSTRUMENT — one series per
		// instrument ever held is unbounded cardinality, and the question an
		// operator has is "is the margin measure falling silent", which a count
		// answers. Which position is a log's job.
		skipped.WithLabelValues(reason).Inc()
	}))
	logger.Info("MARGIN-01: LiquidationProximity registered over the exchange's own margin "+
		"observations — a mandate may now bound how close a portfolio may come to being "+
		"liquidated (#408 control 4)",
		"tenant", cfg.Tenant, "bound_accounts", bindings.Len(), "accounts", bindings.Accounts(),
		"subject", venuemargin.Subject, "max_age", venuemargin.DefaultMaxAge.String())
	return fold, nil
}
