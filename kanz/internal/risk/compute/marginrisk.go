package compute

import (
	"context"
	"math/big"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"

	// decutil: this package's own tests declare a local `dec(...)` Decimal-literal
	// helper (decimal_test.go), so the platform decimal package is aliased here —
	// the same alias decimal.go uses.
	decutil "github.com/eighred/kanz/internal/dec"
)

// HOW CLOSE IS THIS PORTFOLIO TO BEING LIQUIDATED (#408, control 4).
//
// #408's ruling closes its set with one sentence: "A margin control nobody can
// write a limit against is a report, not a control." Controls 1 and 3 made the
// exchange's margin state readable and made the pre-trade gate refuse on it.
// Neither gives a mandate a number to bound. This does: one registered measure,
// carrying its coverage like every other, that RiskLimitRule can gate on and a
// mandate can name.
//
// # IT IS A PROXIMITY, NOT A DISTANCE, AND THE DIRECTION IS FORCED
//
// The ruling names control 4 "a liquidation-distance measure". The number here
// runs the other way — 0 is safe, 1 is being liquidated — and that inversion is
// not a liberty. compliance.RiskLimitRule holds exactly one comparison:
//
//	observed.Cmp(limit) <= 0  ⇒  pass
//
// RiskMeasureLimit carries a max_value and nothing else. A DISTANCE gated by a
// ceiling would read "refuse when the portfolio is TOO FAR from liquidation",
// which is the control backwards, and a mandate author writing `max_value: 0.2`
// against it would believe they had installed a floor. Under a rule that can
// only bound from above, the only measure a mandate can honestly name is one
// where LARGER IS WORSE. So the semantic the ruling asked for is delivered and
// the sign is chosen to fit the one rule that can enforce it.
//
// The second reason is the estate's own: a distance would put the SAFE reading
// at the top of the range and the DANGEROUS one at zero — and zero is the value
// every accumulator that resolved nothing produces. This estate has shipped that
// defect six times in three days. Here it would mean an account nobody could
// read reported "about to be liquidated"… which is at least the loud direction,
// but it would also mean an unlevered book reported an INFINITE distance, and
// there is no common.v1.Decimal for infinity. Proximity has neither problem: the
// unlevered answer is an exact 0 that is TRUE, and the unreadable answer is not
// a number at all (see below).
//
// # WHAT IT MEASURES
//
// For one open leveraged position the exchange reports its own liquidation price
// L. Against the current reference price P for the same instrument:
//
//	proximity = 1 − |P − L| / P,  clamped below at 0
//
// — the fraction of the way the market has already travelled towards the move
// that liquidates the position. P = L gives 1 (the venue is liquidating now); a
// liquidation price a full mark away or further gives 0. It is DIMENSIONLESS and
// therefore the venue-comparable number collateral.v1.VenueMarginState says
// belongs here: margin_ratio's own doc rules itself out for this ("its scale and
// direction are the VENUE's, not ours … control 4's liquidation-distance measure
// is where a venue-comparable number belongs"), because OKX's mgnRatio rises as
// the account gets safer and other venues agree on neither the units nor which
// way danger lies.
//
// The portfolio's value is the MAXIMUM over every open position on every venue
// account backing it. The exchange liquidates an ACCOUNT as a unit and the
// nearest position is what trips it, so an average would let twenty comfortable
// positions hide the one that is about to be sold.
//
// # NOTHING HERE RECONSTRUCTS THE EXCHANGE'S ARITHMETIC
//
// L is the venue's, whole. This does not compute a maintenance requirement, does
// not model a margin tier and does not net anything the exchange does not net —
// all three are the "stale book" #408 exists to remove, and internal/collateral
// (which can compute a requirement under an agreement Kanz holds) is deliberately
// not consulted.
//
// P is OURS, and that is stated rather than hidden. A distance between two
// prices needs two prices, and the exchange publishes only one of them. P is
// market data — a price, not a margin figure — so using it is not the
// reconstruction the ruling rules out; but it is a number the venue did not
// vouch for, it is resolved by the provider below rather than here, and a
// position the provider cannot price is EXCLUDED rather than measured. What this
// measure will never do is invent P from the book's own valuation in order to
// produce a number.
//
// # THREE ANSWERS, AND ONLY TWO OF THEM ARE NUMBERS
//
//	a number, coverage complete   the exchange answered, and this is how close it is
//	exact 0, coverage complete    the EXCHANGE says these accounts hold no
//	                              leveraged position — nothing to be near
//	NO VALUE, coverage excluding  the platform could not read the margin
//
// The middle and the last are the pair the ruling calls out, and they are the
// pair a spot-only venue makes real: Binance's adapter has no margin source at
// all (#601), so it publishes nothing on accounting.margin.observed and its
// accounts are UNKNOWN here — NOT unlevered. This measure never infers "no
// leverage" from a silent feed. It infers it only from an observation that was
// read, is current, reports its coverage, excludes nothing, and lists no open
// position: the exchange itself saying the account holds none. Reading silence
// as safety is precisely how an unlevered book and a blind one come to look
// identical.
//
// The last answer carries NO Value at all rather than a zero. A zero proximity
// passes every ceiling ever written, and decimal.go states the rule this follows:
// "the honest answer is an ABSENT measure, not a wrapped one and never a zero".
// The coverage is what makes it safe on the wire — riskview declines to fold any
// measure whose coverage reports exclusions, so it reaches RiskLimitRule as
// UNKNOWN, which is a refusal. That is inherited, not rebuilt: this measure adds
// no path of its own around the fold.
//
// INVARIANT, PINNED BY TEST: a Measure returned from here with no Value ALWAYS
// carries at least one exclusion. It has to be. riskview folds a measure whose
// coverage excludes nothing, and dec.FromProto(nil) is zero — so an absent value
// with clean coverage would arrive at the gate as a confident, limit-passing
// zero, by the one route the coverage discipline does not cover.

// MeasureLiquidationProximity is how close the portfolio is to being liquidated
// at the venues it trades on: 0 = no leveraged position, 1 = at the exchange's
// liquidation price. Larger is worse, so a mandate bounds it from above.
const MeasureLiquidationProximity v1.MeasureName = "LiquidationProximity"

// The reasons this measure declines to answer. A SMALL CLOSED SET rather than
// free text, matching SkipNoTerms / SkipNoModel / SkipIlliquid: the caller counts
// by this string and a metric label must not be whatever a future edit writes.
const (
	// SkipNoMarginProvider: the measure was NOT REGISTERED, because no
	// MarginProvider was supplied. FIRES ONCE, AT REGISTRATION, NOT PER
	// EVALUATION — it is a property of the wiring, and instrumentID is empty. It
	// is reported at all because the symptom is otherwise silence:
	// engine.filterMeasures drops unknown names, so a client asking for this
	// measure gets 200 with it absent, indistinguishable from never asking.
	SkipNoMarginProvider = "no_margin_provider"
	// SkipNoMarginAccounts: the platform cannot say which venue accounts back
	// this portfolio, so there is no liquidation boundary to measure a distance
	// to. Whole-book; instrumentID is empty.
	SkipNoMarginAccounts = "no_margin_accounts"
	// SkipMarginUnknown: an account's margin state was never observed or is no
	// longer current. venuemargin.DefaultMaxAge is two minutes and aging out
	// yields UNKNOWN, which every control in the #408 set fails closed on.
	SkipMarginUnknown = "margin_unknown"
	// SkipMarginUncovered: the observation carried NO coverage record, so it
	// cannot be shown to have left nothing out. Absent coverage is not zero
	// exclusions — domain.v1.InputCoverage's contract is that presence is the
	// signal — and collapsing the two hides the worse of them.
	SkipMarginUncovered = "margin_coverage_not_reported"
	// SkipMarginIncomplete: the observation reports coverage and the exchange did
	// not answer part of what was asked. The account is excluded whole: the
	// missing part may be the liquidation price of the position nearest the
	// boundary, and there is no way from here to know it was not.
	SkipMarginIncomplete = "margin_incomplete"
	// SkipNoLiquidationPrice: an open position with no liquidation price from the
	// venue. Never read as "does not liquidate".
	SkipNoLiquidationPrice = "no_liquidation_price"
	// SkipLiquidationNegative: the venue's liquidation price is negative, which
	// this measure cannot express as a fraction of a mark. Refused rather than
	// clamped.
	SkipLiquidationNegative = "liquidation_price_negative"
	// SkipNoMark: no current reference price for the position, so the venue's
	// liquidation price cannot be turned into a distance. The gap is usually the
	// venue symbol → instrument mapping (see MarginProvider).
	SkipNoMark = "no_mark_price"
	// SkipMarkNotPositive: the reference price is zero or negative, so the
	// fraction has no denominator.
	SkipMarkNotPositive = "mark_not_positive"
	// SkipProximityNotRepresentable: the computed proximity will not survive
	// conversion to a common.v1.Decimal (#94). Unreachable for a value in [0,1]
	// and handled anyway, because the alternative is a wrapped coefficient the
	// system then acts on.
	SkipProximityNotRepresentable = "proximity_not_representable"
)

// LiquidationRef is one open leveraged position as the EXCHANGE reported it,
// paired with the reference price it is measured against.
//
// BOTH PRICES ARRIVE FROM THE PROVIDER, ALREADY PAIRED, and that is the seam's
// whole reason for existing. collateral.v1.VenueLiquidationPrice.venue_symbol is
// the exchange's own spelling and deliberately NOT a Kanz instrument_id — the
// adapter's symbol map is a one-way instrument → symbol table, so a position the
// map does not cover keeps its liquidation price instead of being dropped.
// Inverting that map is the venue adapter's business and is not knowable from
// inside the risk module, so this package does not attempt it: it receives what
// somebody who holds the map resolved, and excludes what they could not.
type LiquidationRef struct {
	// InstrumentID names the position for the exclusion record. It may be empty
	// — a position whose venue symbol nothing maps still has a liquidation price
	// worth refusing over, and dropping it for want of a name would silently lose
	// exactly the position nobody is watching. VenueSymbol carries the venue's
	// spelling for an operator.
	InstrumentID domain.InstrumentID
	VenueSymbol  string

	// Liquidation is the venue's own liquidation price for the position, in the
	// instrument's quote currency. nil ⇒ UNKNOWN, never zero: a zero would read
	// as "liquidates only at a price of nothing".
	Liquidation *big.Rat

	// Mark is the current reference price for the same instrument, in the same
	// currency. nil ⇒ UNKNOWN. It is OURS rather than the exchange's; see the
	// file header for why that is sound here and where the line is.
	Mark *big.Rat
}

// AccountMargin is one venue account's exchange-reported liquidation state, in
// the form this measure consumes.
//
// # Three booleans and not one, on purpose
//
// Read, CoverageReported and ExcludedCount are the presence discipline control 1
// built and control 3 gates on (#601, #604), carried forward rather than
// collapsed. Storing only a count made "the publisher reports no coverage" and
// "it reports, and excluded nothing" the same zero, which is the wrong
// conflation in the wrong direction — the publisher that says nothing is the one
// nobody has checked. Each state also produces a DIFFERENT exclusion reason,
// because an operator has to know which one to go and fix; not one of them is a
// pass.
//
// # No observation timestamp, deliberately
//
// The freshness bound lives upstream, in the view that folds the observations: a
// state older than venuemargin.DefaultMaxAge never reaches a caller at all, and
// arrives here as Read=false. compliance.MarginState carries the stamp because
// it writes it into a refusal's EVIDENCE, which a person reads. This measure has
// no evidence surface — its output is a number and a reason string — so a stamp
// here would be carried and never read, and a field nothing reads is a check
// nobody performs while looking like one.
type AccountMargin struct {
	// Venue and Account identify the liquidation boundary: the exchange margins
	// and liquidates per ACCOUNT, and #415's ErrAccountShared is what makes one
	// account resolvable from one portfolio.
	Venue   string
	Account string

	// Read is false when the exchange's margin state for this account is UNKNOWN
	// — never observed, no margin source on the venue, or no longer current. All
	// three are one answer here for the reason venuemargin.View gives: a caller
	// that could tell them apart would eventually pass one of them.
	Read bool

	// CoverageReported says whether the observation carried a coverage record at
	// all; ExcludedCount how much of the exchange's answer was missing. False and
	// zero are NOT the same as true and zero.
	CoverageReported bool
	ExcludedCount    uint32

	// Positions are the open leveraged positions on this account. EMPTY ON A
	// READ, CURRENT, COMPLETE OBSERVATION IS A POSITIVE STATEMENT: the exchange
	// says this account holds no leveraged position, and the honest proximity is
	// an exact zero. Empty on any other observation says nothing at all.
	Positions []LiquidationRef
}

// MarginProvider resolves the venue accounts backing a portfolio and the
// exchange's reported liquidation state of each.
//
// ok=false means the platform cannot say WHICH accounts back this portfolio —
// no bindings, an unknown portfolio, a source that is not there. It is not "this
// portfolio has no accounts", and it is never an empty, comfortable answer: the
// measure excludes the whole book on it.
type MarginProvider interface {
	AccountMargins(ctx context.Context, portfolioID v1.PortfolioID) ([]AccountMargin, bool)
}

// MarginOption customises the margin measure registration.
type MarginOption func(*marginOptions)

type marginOptions struct {
	onSkip func(instrumentID, reason string)
}

// WithMarginObserver sets the hook invoked when this measure declines to answer
// for something, or when it is not registered at all. reason is one of the Skip*
// constants above; instrumentID is empty for the whole-book and
// registration-time reasons.
//
// The signature matches FIProviders.OnSkip, WithLiquidityObserver and their
// siblings so ONE observer serves every seam.
//
// IT IS A METRIC AND IT DOES NOT REPLACE THE COVERAGE. fi.go states the argument
// this file inherits: the counter is watched across all portfolios by an
// operator who happens to be looking, while the InputCoverage attached to the
// value travels WITH the number, to the one caller acting on that one portfolio,
// at the moment they act. #527 is the case where the counter existed and the
// number on the wire was still a confident zero.
func WithMarginObserver(fn func(instrumentID, reason string)) MarginOption {
	return func(o *marginOptions) { o.onSkip = fn }
}

func (o *marginOptions) skip(instrumentID, reason string) {
	if o != nil && o.onSkip != nil {
		o.onSkip(instrumentID, reason)
	}
}

// RegisterMarginRisk registers MeasureLiquidationProximity on r, closing over
// ctx and the provider.
//
// A NIL PROVIDER REGISTERS NOTHING, and fires the observer once with
// SkipNoMarginProvider. The alternative — registering a measure that returns no
// value with a whole-book exclusion on every portfolio, forever — reaches the
// same refusal at the gate by a longer road, and costs the one signal that says
// why: an unregistered measure is counted DARK by compute.Dark against the
// catalogue, so kanz_risk_measure_live reports the margin family at zero and an
// operator can see that nothing is wired. A registered measure that always
// refuses reads as live.
//
// Call at engine startup after DefaultRegistry.
func RegisterMarginRisk(ctx context.Context, r *Registry, provider MarginProvider, opts ...MarginOption) {
	o := &marginOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	if provider == nil {
		o.skip("", SkipNoMarginProvider)
		return
	}
	r.Register(MeasureLiquidationProximity, liquidationProximityMeasure(ctx, provider, o))
}

// liquidationProximityMeasure emits the worst liquidation proximity across every
// venue account backing the portfolio. See the file header for the three
// answers and why only two of them are numbers.
func liquidationProximityMeasure(ctx context.Context, provider MarginProvider, o *marginOptions) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		var cov Coverage

		accounts, ok := provider.AccountMargins(ctx, p.ID())
		if !ok || len(accounts) == 0 {
			// NO LIQUIDATION BOUNDARY IS KNOWN FOR THIS BOOK. Not "it has none":
			// the platform could not say, and saying "nothing to liquidate" of a
			// portfolio nobody could resolve is the flattering reading of silence.
			o.skip("", SkipNoMarginAccounts)
			cov.ExcludeWhole(SkipNoMarginAccounts)
			return v1.Measure{Name: MeasureLiquidationProximity, Coverage: cov.Result()}
		}

		// CONTRIBUTED COUNTS ACCOUNTS, NOT POSITIONS. Coverage.Contributed is left
		// to the measure because "only the measure knows what contributed means for
		// it", and for this one the unit of input is the ACCOUNT — that is what the
		// exchange margins, what it liquidates, and what a single observation
		// describes. Counting positions instead would make a genuinely unlevered
		// account contribute nothing, and toProtoInputCoverage renders a coverage
		// with no contributions and no exclusions as ABSENT — telling every
		// consumer that this measure does not report coverage, on exactly the
		// answer whose credibility rests on it.
		var worst *big.Rat
		levered, priced := 0, 0
		for i := range accounts {
			acct := accounts[i]
			switch {
			case !acct.Read:
				o.skip("", SkipMarginUnknown)
				cov.ExcludeWhole(SkipMarginUnknown)
				continue
			case !acct.CoverageReported:
				o.skip("", SkipMarginUncovered)
				cov.ExcludeWhole(SkipMarginUncovered)
				continue
			case acct.ExcludedCount > 0:
				o.skip("", SkipMarginIncomplete)
				cov.ExcludeWhole(SkipMarginIncomplete)
				continue
			}
			cov.Contributed++

			for j := range acct.Positions {
				pos := acct.Positions[j]
				levered++
				prox, reason := positionProximity(pos)
				if prox == nil {
					o.skip(string(pos.InstrumentID), reason)
					cov.Exclude(pos.InstrumentID, reason)
					continue
				}
				priced++
				// THE MAXIMUM, not an average: the exchange liquidates the account
				// as a unit and the nearest position trips it, so twenty comfortable
				// positions must not average away the one that is about to be sold.
				if worst == nil || prox.Cmp(worst) > 0 {
					worst = prox
				}
			}
		}

		switch {
		case cov.Contributed == 0:
			// Every account was unreadable. The exclusions above say which way.
			return v1.Measure{Name: MeasureLiquidationProximity, Coverage: cov.Result()}
		case levered > 0 && priced == 0:
			// THE BOOK IS LEVERED AND NOT ONE POSITION COULD BE PLACED against its
			// liquidation price. Falling through to the zero below would report
			// "no leveraged position" for a book the exchange has just told us is
			// full of them — the confident zero, arriving through the one door the
			// account-level checks do not cover. The per-position exclusions are
			// already recorded; the value is withheld.
			return v1.Measure{Name: MeasureLiquidationProximity, Coverage: cov.Result()}
		}
		if worst == nil {
			// PROVEN UNLEVERED. Every account was read, current and complete, and
			// the exchange listed no open leveraged position on any of them. Zero is
			// the true answer, not the absence of one — and it is reachable ONLY
			// from a complete observation, so it can never stand in for ignorance.
			worst = new(big.Rat)
		}
		val, representable := decutil.ToProtoScaled(worst)
		if !representable {
			o.skip("", SkipProximityNotRepresentable)
			cov.ExcludeWhole(SkipProximityNotRepresentable)
			return v1.Measure{Name: MeasureLiquidationProximity, Coverage: cov.Result()}
		}
		return v1.Measure{Name: MeasureLiquidationProximity, Value: val, Coverage: cov.Result()}
	}
}

// oneRat is the constant proximity is measured down from. A package-level
// *big.Rat would be a shared mutable value — big.Rat's operations write into
// their receiver — so this returns a fresh one per call rather than handing out
// a pointer any arithmetic could rewrite.
func oneRat() *big.Rat { return big.NewRat(1, 1) }

// positionProximity returns how far through the move to liquidation one position
// already is, in [0,1], or nil and the reason it could not be placed.
//
// EXACT RATIONAL ARITHMETIC, NEVER FLOAT. Prices and the fraction between them
// are *big.Rat all the way to the single conversion in the caller; the estate's
// rule is that money and quantities are common.v1.Decimal / *big.Rat, and a
// liquidation boundary is the last place to spend precision.
//
// NOTHING THE CALLER OWNS IS MUTATED. big.Rat's methods write into the receiver,
// and pos.Liquidation / pos.Mark are the provider's pointers — a Sub or an Abs
// applied to one of them in place would silently rewrite the venue's own figure
// for every later reader. Every receiver below is freshly allocated.
func positionProximity(pos LiquidationRef) (*big.Rat, string) {
	if pos.Liquidation == nil {
		return nil, SkipNoLiquidationPrice
	}
	if pos.Liquidation.Sign() < 0 {
		return nil, SkipLiquidationNegative
	}
	if pos.Mark == nil {
		return nil, SkipNoMark
	}
	if pos.Mark.Sign() <= 0 {
		return nil, SkipMarkNotPositive
	}
	// move = |P − L| / P — the fraction of the current price the market must
	// travel before the exchange liquidates this position.
	move := new(big.Rat).Sub(pos.Mark, pos.Liquidation)
	move.Abs(move)
	move.Quo(move, pos.Mark)

	prox := new(big.Rat).Sub(oneRat(), move)
	if prox.Sign() < 0 {
		// A liquidation price more than a full mark away — a short whose boundary
		// is above twice the current price. The move required is larger than the
		// price itself, and there is no more room than "none of the way there", so
		// the measure floors at 0 rather than reporting a negative proximity a
		// limit would compare against.
		prox = new(big.Rat)
	}
	return prox, ""
}
