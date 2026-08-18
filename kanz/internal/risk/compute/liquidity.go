package compute

import (
	"context"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/liquidity"
)

// LIQ-01d wires the liquidity-risk models into the RISK-07 measure registry —
// the liquidity sibling of the FI-01d rate-risk and DERIV-01d Greek measures. It
// registers the portfolio liquidation horizon and, WHEN THE PROVIDER CAN SERVE A
// SPREAD, liquidity-adjusted VaR; each resolves a position's ADV/spread through
// the injected liquidity.Provider at p.AsOf(), the same point-in-time enrichment
// seam.
//
//	LiquidationHorizon = notional-weighted average days-to-liquidate (liquid names)
//	LVaR99             = VaR99 + Σ |MV_i| × liquidation-cost-fraction_i
//
// LVaR closes over the already-registered VaR measure (the RISK-07 placeholder or
// the MODEL-01d historical model), so it tracks whichever VaR the engine serves
// and is ≥ it by construction — the liquidation cost is never negative.
//
// # THE TWO MEASURES ARE NOT EQUALLY HONEST, AND THE PAIRING IS NOT FIXED
//
// LiquidationHorizon reads ADV ONLY. internal/risk/liquiditysource MEASURES ADV
// off the durable bar series, so the horizon is a real statistic over a real
// tape with no assumption in it.
//
// LVaR99 needs a SPREAD, and nothing in this repository persists a bid or an ask
// (the evidence is enumerated in liquiditysource's package doc: the ingest path
// folds market.v1.Quote to a mid and discards both sides; no migration creates a
// bid/ask/spread column; the in-memory order book has no writer). So a spread
// arrives here only because a desk typed one into a config.
//
// WITH NO SPREAD, LVaR99 IS DEFINITIONALLY NEVER AN ADJUSTMENT.
// liquidity.Model.CostFraction returns 0 for a non-positive spread, so
// LiquidationCost is exactly 0 and LVaR99 is exactly VaR99 — a
// liquidity-ADJUSTED VaR that equals the number the engine already serves, on
// every book, forever. That is not a degraded measure, it is a measure whose
// name is a claim it cannot make; and because it is plausible, in range, and
// tracks VaR perfectly, nothing downstream can tell.
//
// So the pair is decided per deployment: see RegisterLiquidityRisk and
// SpreadServing.
//
// # THE OBSERVER IS NOT OPTIONAL DECORATION HERE
//
// BOTH measures report ZERO for "nothing was known", and for the horizon a zero
// is the MOST LIQUID ANSWER THE MEASURE CAN GIVE. liquidity.Profile skips a
// position whose provider answers ok=false, and WeightedDays is 0 when no
// position resolved — so a book whose every name was refused (wrong venue, wrong
// resolution, an empty store, a desk that never entered a spread) reports
// LiquidationHorizon = 0.0000 DAYS: "this book liquidates instantly". That is
// the #257 shape (ten copies of the base-currency filter each dropped positions
// and not one recorded it, so a USD book holding only EUR reported
// GrossExposure = 0) in the unsafe direction. WithLiquidityObserver is what
// makes those zeros distinguishable from an answer.

// Liquidity measure names (RISK-07 naming: short, CamelCase).
const (
	MeasureLiquidationHorizon v1.MeasureName = "LiquidationHorizon"
	MeasureLVaR99             v1.MeasureName = "LVaR99"
)

// liqHorizonExp is the precision the liquidation-horizon (a number of days) is
// emitted at, matching the dimensionless-ratio band (1e-4). liqVaRExp is the
// money scale LVaR is emitted at (cents), matching the VaR models.
const (
	liqHorizonExp int32 = -4
	liqVaRExp     int32 = -2
)

// SpreadServing is the OPTIONAL capability a liquidity.Provider implements to
// declare whether any spread it serves can be non-zero — which is exactly the
// question of whether LVaR99 is a measure or a copy of VaR99.
//
// # Why the provider answers this and not the composition root
//
// liquiditysource already forces the deployment to SPELL its spread posture at
// construction: WithAssumedSpreads (desk-entered spreads) or WithNoSpread
// (ADV-only), with no default, because the two postures degrade different
// measures. That decision is therefore ALREADY MADE, in one place, before this
// package sees anything.
//
// A separate flag here — a RegisterLiquidityRisk option, a providers-struct
// field, a second Register* function — would be the SAME decision spelled a
// SECOND time, in a second place, by a caller who could disagree with the first.
// A deployment that passes WithNoSpread and forgets the flag serves the
// degenerate LVaR99 anyway, and the misconfiguration is invisible precisely
// because the measure looks fine. Asking the provider makes the two impossible
// to contradict: there is one posture, held by the object that knows it.
//
// # What a provider that does NOT implement it gets, and why
//
// Both measures — today's behaviour. A provider that has made no claim gives
// this package nothing to infer from, and refusing to register LVaR99 on that
// silence would drop a measure from every hand-built test provider and every
// existing wrapper. The claim is one method long, and THE PRODUCTION PROVIDER
// MAKES IT (liquiditysource.Provider.ServesSpread).
//
// KNOWN GAP, STATED RATHER THAN DISCOVERED: liquidity.Stress.Wrap returns a
// wrapper that does not forward this method, so a STRESSED provider reads as
// "no claim" and gets LVaR99 registered even when the wrapped provider serves no
// spread. Stress multiplies the spread it is given, so 3 × 0 is still 0 and the
// degenerate LVaR comes back. Forwarding it belongs in internal/risk/liquidity
// with the stress path's own evidence.
type SpreadServing interface {
	// ServesSpread reports whether this provider can ever return a LiquiditySpec
	// with a non-zero Spread. False means every LVaR99 it could produce equals
	// VaR99 exactly.
	ServesSpread() bool
}

// ServesSpread reports whether provider can serve a non-zero spread — true when
// it does not implement SpreadServing (no claim ⇒ today's behaviour; see the
// interface doc). Exported so a composition root can ask the same question this
// package asks, rather than re-deriving it from its own config.
func ServesSpread(provider liquidity.Provider) bool {
	s, ok := provider.(SpreadServing)
	if !ok {
		return true
	}
	return s.ServesSpread()
}

// The reasons a liquidity measure reports zero, or reports nothing at all. A
// SMALL CLOSED SET rather than free text, matching SkipNoTerms / SkipNoCurve /
// SkipNoModel: the caller counts by this string and a metric label must not be
// whatever a future edit writes.
const (
	// SkipNoSpreadSource: LVaR99 was NOT REGISTERED, because the provider
	// declared it serves no spread. FIRES ONCE, AT REGISTRATION, NOT PER
	// EVALUATION — it is a property of the wiring, not of a portfolio, and
	// instrumentID is empty. It is reported at all because the symptom is a
	// measure that is simply ABSENT: internal/risk/engine.filterMeasures drops
	// unknown names, so a client asking for LVaR99 gets 200 with LVaR99 missing,
	// indistinguishable from a request that never asked for it.
	SkipNoSpreadSource = "no_spread_source"
	// SkipIlliquid: a position resolved and its ADV is non-positive, so
	// DaysToLiquidate is +Inf and it is EXCLUDED FROM THE WEIGHTED HORIZON (it is
	// counted in liquidity.Profile.IlliquidNotional, which this scalar measure
	// cannot carry — a +Inf horizon has no Decimal representation). The reported
	// horizon is therefore the horizon of the names that CAN be liquidated, and a
	// book with one unliquidatable name reports a finite, comfortable number.
	SkipIlliquid = "illiquid"
	// SkipNoLiquidHorizon: the evaluation found NO liquid position at all, so
	// LiquidationHorizon is 0.0000 days — the most liquid answer the measure can
	// give, for a book about which nothing is known. instrumentID is empty: this
	// is a property of the (portfolio, asOf) evaluation, not of any instrument,
	// matching SkipNoModel's convention so one observer can serve every seam.
	//
	// NOT REPORTED FOR AN EMPTY PORTFOLIO. A book with no positions liquidates in
	// zero days and that is the honest answer, not a gap.
	SkipNoLiquidHorizon = "no_liquid_horizon"
	// SkipNoLiquidationCost: LVaR99 came out EXACTLY EQUAL to VaR99 on a
	// non-empty book — the liquidation cost was zero. Either every spread served
	// was zero, or no position resolved at all. Registration-time
	// SkipNoSpreadSource catches the posture that guarantees this; THIS one
	// catches the deployment that declared it supplies spreads and whose table
	// covers nothing on the book. instrumentID is empty (a whole-book event).
	SkipNoLiquidationCost = "no_liquidation_cost"
)

// LiquidityOption customises the liquidity measure registration.
//
// A VARIADIC OPTION RATHER THAN THE FIProviders / GreeksProviders / FactorProviders
// STRUCT, and the reason is narrow: those three seams bundle PROVIDERS, and this
// one has exactly one provider that is already a named parameter. Folding
// (provider, model, baseVaR) into a LiquidityProviders struct would be a
// signature change to a seam whose only callers are tests — cheap — but it would
// buy nothing over what is here, because the thing being added is an OBSERVER
// and not a fourth data source. The option keeps the observer NAMED at the call
// site, which a fifth positional argument would not.
type LiquidityOption func(*liquidityOptions)

type liquidityOptions struct {
	onSkip func(instrumentID, reason string)
}

// WithLiquidityObserver sets the hook invoked when a liquidity measure reports
// zero for something that is not a flat book, or when LVaR99 is not registered
// at all. reason is one of the Skip* constants above; instrumentID is empty for
// the whole-book and registration-time reasons.
//
// The signature matches FIProviders.OnSkip, GreeksProviders.OnSkip and
// FactorProviders.OnSkip so ONE observer can serve all four seams.
//
// FIRES ONCE PER MEASURE, NOT ONCE PER POSITION — RegisterLiquidityRisk installs
// up to two measures over the same per-position path, and ComputeMeasures takes
// a name filter, so the fan-out is not even a fixed number to divide by. Read
// the counter as "skip events", never as "positions dropped".
func WithLiquidityObserver(fn func(instrumentID, reason string)) LiquidityOption {
	return func(o *liquidityOptions) { o.onSkip = fn }
}

func (o *liquidityOptions) skip(instrumentID, reason string) {
	if o != nil && o.onSkip != nil {
		o.onSkip(instrumentID, reason)
	}
}

// RegisterLiquidityRisk registers the liquidity measures on r, each closing over
// ctx + the provider + model.
//
// LiquidationHorizon IS ALWAYS REGISTERED: it reads ADV only, and ADV is the half
// of liquidity this repository can measure.
//
// LVaR99 IS REGISTERED ONLY IF THE PROVIDER CAN SERVE A SPREAD (see
// SpreadServing). A provider that declares it cannot yields no LVaR99 at all
// rather than one that equals VaR99 on every book forever — the honest measure
// reaches the wire without the degenerate one riding along. The omission fires
// the observer once with SkipNoSpreadSource, because an ABSENT measure is
// otherwise silent: engine.filterMeasures drops unknown names.
//
// baseVaR is the VaR measure LVaR adds the liquidation cost to.
//
// NIL MEANS THE REGISTRY'S VaR99, RESOLVED AT EVALUATION TIME — and it did not
// used to. It captured the package-level compute.VaR99, the RISK-07 PLACEHOLDER
// (1% of gross), while its own doc claimed "the registry's current VaR99".
// Because varmodel.Register overrides MeasureVaR99 IN THE REGISTRY, a nil here
// built LVaR on the placeholder while the engine served the historical model:
// two different VaR numbers in one response, with LVaR99 ≥ VaR99 no longer
// guaranteed and nothing anywhere saying why. Resolving through r on every
// evaluation also makes the call ORDER-INDEPENDENT, so registering the VaR model
// after this call is no longer a silent misconfiguration.
//
// Call at engine startup after DefaultRegistry; tests register against
// deterministic providers.
func RegisterLiquidityRisk(ctx context.Context, r *Registry, provider liquidity.Provider, model liquidity.Model, baseVaR MeasureFunc, opts ...LiquidityOption) {
	o := &liquidityOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	if baseVaR == nil {
		baseVaR = registryVaR99(r)
	}
	r.Register(MeasureLiquidationHorizon, liquidationHorizonMeasure(ctx, provider, model, o))
	if !ServesSpread(provider) {
		o.skip("", SkipNoSpreadSource)
		return
	}
	r.Register(MeasureLVaR99, lvarMeasure(ctx, provider, model, baseVaR, o))
}

// registryVaR99 is the LATE-BOUND registry VaR: it reads MeasureVaR99 out of r
// at EVALUATION time, so LVaR tracks whichever VaR the engine ends up serving
// regardless of the order the two registrations happened in. Falls back to the
// package placeholder only if nothing at all is registered under the name, which
// DefaultRegistry makes impossible.
func registryVaR99(r *Registry) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		if fn, ok := r.funcs[MeasureVaR99]; ok && fn != nil {
			return fn(p)
		}
		return VaR99(p)
	}
}

// liquidationHorizonMeasure emits the notional-weighted average days-to-liquidate
// over the liquid, base-currency book. Illiquid (ADV-zero) names are flagged in
// the richer liquidity.Profile, not folded into this scalar (a +Inf horizon has
// no Decimal representation) — they surface through the observer as SkipIlliquid,
// and a book with nothing liquid at all as SkipNoLiquidHorizon.
func liquidationHorizonMeasure(ctx context.Context, provider liquidity.Provider, model liquidity.Model, o *liquidityOptions) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		prof := model.LiquidationProfile(ctx, p, provider)
		var cov Coverage
		liquid := 0
		for i := range prof.Positions {
			if prof.Positions[i].Liquid {
				liquid++
				cov.Contributed++
				continue
			}
			o.skip(prof.Positions[i].InstrumentID, SkipIlliquid)
			cov.Exclude(domain.InstrumentID(prof.Positions[i].InstrumentID), SkipIlliquid)
		}
		// THE BASE-CURRENCY FILTER IS NOT RE-RUN HERE. liquidity.Profile already
		// applies it, and a second copy in this file is precisely the #257 defect
		// (one concept, many implementations, most of them wrong). So a position
		// the provider REFUSED is not attributable to an instrument at this layer
		// — it is simply absent from prof.Positions — and the whole-book counter
		// below is what says the horizon rests on nothing. Per-instrument refusal
		// reasons live in the provider's own observer, which is the only place
		// that can tell a wrong venue from an empty store.
		if liquid == 0 && len(p.Positions()) > 0 {
			o.skip("", SkipNoLiquidHorizon)
			// WHOLE-BOOK ONLY WHEN THERE WAS NOTHING TO ATTRIBUTE. A profile that
			// came back empty means the measure never got as far as the book, which
			// is ExcludeWhole's documented meaning. When the profile DID name
			// positions and every one was illiquid, the per-position exclusions
			// above already say so — adding a whole-book entry on top would count
			// one outage twice and overstate how much was missed.
			if len(prof.Positions) == 0 {
				cov.ExcludeWhole(SkipNoLiquidHorizon)
			}
		}
		// THE COVERAGE TRAVELS WITH THE NUMBER, AND THE COUNTER DOES NOT REPLACE IT
		// (#527, #509). A horizon of zero days says the book unwinds instantly —
		// the flattering direction, and a number somebody sizes a position against.
		// Served over a book where nothing resolved it is indistinguishable from a
		// flat book. o.skip already fired, but fi.go's own argument applies here:
		// the counter is watched across all portfolios by an operator who happens
		// to be looking, while this reaches the one caller acting on this one
		// portfolio at the moment they act. This measure is REGISTERED IN
		// PRODUCTION, which is what separated it from the other three families
		// #527 fixed.
		return v1.Measure{
			Name:     MeasureLiquidationHorizon,
			Value:    floatToDecimal(prof.WeightedDays, liqHorizonExp),
			Coverage: cov.Result(),
		}
	}
}

// lvarMeasure emits VaR99 widened by the book's liquidation cost. The value
// inherits VaR's money scale (cents); the cost is added in the same units.
func lvarMeasure(ctx context.Context, provider liquidity.Provider, model liquidity.Model, baseVaR MeasureFunc, o *liquidityOptions) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		base := baseVaR(p)
		varFloat := decimalToFloat(base.Value)
		lvar := model.LiquidityAdjustedVaR(ctx, varFloat, p, provider)
		// INHERITED FIRST. LVaR is VaR widened by a cost; if the VaR underneath was
		// computed over nothing, so is this, and a derived measure must not launder
		// a flagged input into a clean answer.
		var cov Coverage
		cov.Inherit(base.Coverage)
		if lvar == varFloat && len(p.Positions()) > 0 {
			// EXACT EQUALITY IS THE RIGHT TEST, not a tolerance.
			// LiquidityAdjustedVaR is varValue + cost, so an identical float means
			// the cost was exactly zero — the degenerate case — rather than small.
			o.skip("", SkipNoLiquidationCost)
			cov.ExcludeWhole(SkipNoLiquidationCost)
		}
		return v1.Measure{Name: MeasureLVaR99, Value: floatToDecimal(lvar, liqVaRExp), Coverage: cov.Result()}
	}
}
