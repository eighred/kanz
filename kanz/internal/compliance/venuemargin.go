package compliance

import (
	"math/big"
	"strconv"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// MarginState is what the EXCHANGE reported about one venue account's margin, in
// the form a pre-trade rule gates on (#408, control 3).
//
// # Everything here is the venue's, and nothing here is computed
//
// internal/collateral can model what a margin requirement SHOULD be under an
// agreement Kanz holds, and that computation is sound and is still the wrong
// input for this control: the exchange margins on its own maintenance tiers, its
// own mark and its own index, and it liquidates on its own schedule without
// asking. A reconstruction of its arithmetic is precisely the stale book #408
// exists to remove.
//
// # ObservedAt travels WITH Ratio, and that is not decoration
//
// internal/venuemargin makes the pairing structural: its Quantity has unexported
// fields and no accessor that yields the number without the timestamp, because
// every arrangement that lets them separate eventually separates them. This
// struct is where that value crosses into the rules engine, so it carries the
// stamp forward rather than dropping it — the age is what the refusal's evidence
// is written from, and a refusal an operator cannot date is a refusal they
// cannot act on.
//
// The freshness BOUND is applied upstream, by the view that folds the
// observations: a state older than it never reaches here at all, arriving as
// ok=false from MarginSource. ObservedAt is therefore evidence, not a second
// check — but it is evidence the rule would have no way to produce if this type
// had left it behind.
type MarginState struct {
	// Account is the exchange account this portfolio trades from at the venue —
	// the unit the exchange margins and LIQUIDATES per, resolved through the
	// deploy-time bindings that make it single-valued (#415).
	Account string

	// Ratio is the venue's own margin ratio for the account, IN THE VENUE'S UNITS
	// AND DIRECTION. nil ⇒ the venue did not report one, which is UNKNOWN and
	// refuses; it is never a measured zero.
	Ratio *big.Rat

	// ObservedAt is when the state was TRUE AT THE VENUE, in UTC — the exchange's
	// own stamp where it publishes one. Not when Kanz published, folded or read
	// it.
	ObservedAt time.Time

	// CoverageReported says whether the observation carried a coverage record at
	// all, and ExcludedCount how much of the venue's answer was missing.
	//
	// TWO FIELDS BECAUSE THE ZERO VALUE IS AMBIGUOUS. domain.v1.InputCoverage's
	// contract is that presence is the signal: absent means "this publisher does
	// not report coverage", present with zero exclusions means "it reports, and
	// the venue answered everything". Collapsed into one count they are the same
	// number, and the collapse hides the worse of the two.
	CoverageReported bool
	ExcludedCount    uint32
}

// Complete reports whether the venue answered everything asked of it: coverage
// was reported AND nothing was excluded. The zero MarginState is not complete.
func (m MarginState) Complete() bool { return m.CoverageReported && m.ExcludedCount == 0 }

// MarginSource resolves the exchange's own margin state for the venue account a
// portfolio trades from, or ok=false when it is UNKNOWN (#408, control 3).
//
// A SEAM AND NOT A QUERY, the same stance CashSource and RiskSource take. This
// is read on the order-admission path; a call into a venue adapter here would
// put an externally-owned latency in front of every order and turn a degraded
// adapter into a trading outage. The implementation reads a local fold of
// accounting.margin.observed, which a stalled feed makes STALE — a refusal —
// rather than slow.
//
// tenantID IS IN THE SIGNATURE, and not left to the implementation's own
// configuration, for the reason MandateSource carries one (#243): portfolio_id
// is a caller-chosen string off SubmitOrder, the account bindings are keyed by
// (tenant, portfolio, venue), and a lookup that guessed the tenant would resolve
// one customer's portfolio name to another customer's exchange account — which
// is the collateral-segregation failure this whole control set is built on.
type MarginSource interface {
	Margin(tenantID, portfolioID, venue string) (MarginState, bool)
}

// VenueMarginRule enforces a VenueMarginLimit: an order that would TAKE OR
// INCREASE a position on a margin-enabled account is refused unless the
// exchange's own margin state for that account is READ, CURRENT and COMPLETE —
// and, where the mandate names a floor, above it (#408, control 3).
//
// # What it is bought against
//
// #408's ruling names the failure in one line: margin turns "we lost money on a
// trade" into "the exchange sold our collateral while we were reading stale
// books". The second is not a larger version of the first. It is loss caused by
// our own ignorance of a position's state, executed by a counterparty on their
// schedule, and it cannot be traded out of.
//
// # UNKNOWN REFUSES, and the ways to be unknown are one answer
//
// No margin source wired; no account bound to this portfolio at this venue; the
// venue never observed; the last observation too old; the venue reported no
// ratio; the observation says the venue could not answer everything. All of them
// mean this rule cannot vouch for the account's margin, and all of them refuse.
// They carry DIFFERENT evidence, because an operator has to know which one to go
// and fix — but not one of them is a pass.
//
// THE ESTATE HAS SHIPPED THE OPPOSITE SIX TIMES IN THREE DAYS. A measure that
// resolves its inputs through a provider skips what does not resolve and returns
// its accumulator anyway, and an accumulator nothing reached is zero — announced
// on time, in range, indistinguishable from an answer. Twice in FRTB, in the
// liquidity measures, in the alpha grading loop, in the bar readers. Here the
// zero would read as "this account needs no collateral", and what it costs is
// the fund's collateral.
//
// # ABSENT COVERAGE IS NOT ZERO EXCLUSIONS
//
// domain.v1.InputCoverage's zero value means "this publisher does not report
// coverage", NOT "everything resolved" — stated on the message itself and in
// internal/risk/api/v1. An observation that does not say what it left out cannot
// be shown to have left out nothing, so this rule refuses it under its own
// evidence rather than reading it as clean. That is stricter than riskview's
// fold, and the difference is deliberate: a risk measure may legitimately not
// report coverage (GrossExposure reads the book directly and no provider can
// decline it), whereas every margin figure here comes from an exchange that can
// decline any of them.
//
// # ORDERS THAT REDUCE THE POSITION ARE NOT GATED
//
// A margin control that blocked de-risking would be actively dangerous: the
// moment the feed goes quiet is the moment the fund most needs to be able to cut
// a position, and a gate that refuses the close as well as the open traps it in
// the trade. So an order strictly reducing the instrument's position, without
// crossing through flat, passes this rule untouched — while opening, adding, and
// FLIPPING (which closes and re-opens on the other side, taking a new position)
// are all gated. Flat-to-flat and zero-quantity orders are gated too: they
// reduce nothing, so there is nothing to protect.
//
// # THE CHECK IS ON THE ACCOUNT, NOT ON THE ORDER'S CONTRIBUTION TO IT
//
// Stated rather than hidden, exactly as RiskLimitRule states its own limit: this
// asks whether the account is in a state where taking more position is
// permissible, not what this specific order would do to the margin ratio.
// Projecting the venue's ratio through a candidate trade would mean
// reconstructing the exchange's margin arithmetic — the reconstruction #408
// rules out, and the reason control 1 reads the venue's numbers instead of
// deriving them.
func VenueMarginRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	vm := rule.GetVenueMargin()
	if vm == nil {
		return paramsMismatch("venue_margin")
	}
	declared := vm.GetVenue()
	floor := vm.GetMinMarginRatio()

	// A FLOOR WITHOUT A VENUE IS NOT A FLOOR. Margin ratios do not agree across
	// venues on units or on which way danger lies, so "≥ 0.05" means nothing until
	// somebody says whose 0.05. Applying it to whichever venue the order reaches
	// would be this platform asserting a convention on an exchange's behalf; the
	// mandate is misconfigured, and a misconfigured mandate fails closed.
	if floor != nil && declared == "" {
		return &compliancepb.Violation{
			Message: "venue margin floor names no venue — a margin ratio has no meaning until the venue is named",
			Evidence: map[string]string{
				"venue": "unspecified",
				"floor": ratString(ratFromDecimal(floor)),
			},
		}
	}

	// AN ORDER THAT ONLY REDUCES THE POSITION IS NOT GATED. Checked before
	// anything is looked up, so a de-risking order is admitted even when the
	// margin feed is entirely dark — which is exactly when it will be.
	if orderReducesPosition(c) {
		return nil
	}

	venue := declared
	if c.Order != nil && c.Order.Venue != "" {
		// A RULE SCOPED TO ANOTHER VENUE DOES NOT APPLY. It is not satisfied and it
		// is not breached: this order is not going where the rule governs. A
		// portfolio trading on margin at two venues declares two rules.
		if declared != "" && declared != c.Order.Venue {
			return nil
		}
		venue = c.Order.Venue
	}
	if venue == "" {
		// The mandate governs every venue and the order names none, so there is no
		// account whose collateral this order spends — and therefore no margin state
		// to check it against. Refused rather than passed: "we cannot tell which
		// exchange account backs this order" is the question the rule exists to ask.
		return &compliancepb.Violation{
			Message:  "margin cannot be verified: the order names no venue, so no exchange account can be identified",
			Evidence: map[string]string{"venue": "unspecified"},
		}
	}

	if c.Margin == nil {
		// NOTHING OBSERVES MARGIN ON THIS DEPLOYMENT. The mandate declares margin
		// trading and the platform cannot see a single margin figure — the state the
		// posture gauge exists to make visible before this refusal starts, and the
		// one state where "nothing configured" would otherwise look like "checked,
		// and fine".
		return &compliancepb.Violation{
			Message: "margin cannot be verified: no venue margin source is wired",
			Evidence: map[string]string{
				"venue":  venue,
				"margin": "unavailable",
			},
		}
	}
	state, ok := c.Margin(venue)
	if !ok {
		return &compliancepb.Violation{
			Message: "margin cannot be verified: the exchange's margin state for this account is unknown or not current",
			Evidence: map[string]string{
				"venue":  venue,
				"margin": "unknown",
			},
		}
	}
	if !state.CoverageReported {
		return &compliancepb.Violation{
			Message: "margin cannot be verified: the observation does not report what the exchange left out",
			Evidence: map[string]string{
				"venue":       venue,
				"account":     state.Account,
				"coverage":    "not_reported",
				"observed_at": stamp(state.ObservedAt),
			},
		}
	}
	if state.ExcludedCount > 0 {
		return &compliancepb.Violation{
			Message: "margin cannot be verified: the exchange did not report part of this account's margin state",
			Evidence: map[string]string{
				"venue":       venue,
				"account":     state.Account,
				"coverage":    "incomplete",
				"excluded":    itoa(state.ExcludedCount),
				"observed_at": stamp(state.ObservedAt),
			},
		}
	}
	if state.Ratio == nil {
		// Reachable even with complete coverage, because a publisher other than
		// venuemargin.Reporter may report coverage over a set that never included a
		// margin ratio. Absent is UNKNOWN and never zero: a zero ratio is a claim
		// about the account, and this is the absence of one.
		return &compliancepb.Violation{
			Message: "margin cannot be verified: the exchange reported no margin ratio for this account",
			Evidence: map[string]string{
				"venue":        venue,
				"account":      state.Account,
				"margin_ratio": "unknown",
				"observed_at":  stamp(state.ObservedAt),
			},
		}
	}
	if floor == nil {
		// No floor declared: the control is that the margin state is READABLE,
		// CURRENT and COMPLETE, which is what #408 control 3 asks for. Everything
		// above has established that.
		return nil
	}
	min := ratFromDecimal(floor)
	if state.Ratio.Cmp(min) >= 0 {
		return nil
	}
	return &compliancepb.Violation{
		Message: "order refused: the venue account is below its margin floor",
		Evidence: map[string]string{
			"venue":        venue,
			"account":      state.Account,
			"margin_ratio": ratString(state.Ratio),
			"floor":        ratString(min),
			"observed_at":  stamp(state.ObservedAt),
		},
	}
}

// orderReducesPosition reports whether the candidate's order strictly reduces
// the instrument's position WITHOUT crossing through flat.
//
// It works off the PROJECTED book, which is what the rules receive: the
// pre-trade quantity is the projected one minus the delta that produced it. That
// is exact — project() computes post = pre + delta with the same decimal
// arithmetic — and it avoids handing every rule a second book to disagree with
// the first.
//
// FALSE WHENEVER THE ANSWER IS NOT CERTAIN, because false is the gated
// direction: no order at all (the post-trade monitor), an instrument the
// projection does not carry, a zero delta, opening from flat, adding to a
// position, and a FLIP — which reduces to zero and then takes a fresh position
// on the other side, the very thing this gate is for.
func orderReducesPosition(c *Candidate) bool {
	if c == nil || c.Order == nil || c.Book == nil {
		return false
	}
	delta := ratFromDecimal(c.Order.SignedQuantity)
	if delta.Sign() == 0 {
		return false
	}
	var post *big.Rat
	for i := range c.Book.Positions {
		if c.Book.Positions[i].InstrumentID == c.Order.InstrumentID {
			post = ratFromDecimal(c.Book.Positions[i].Quantity)
			break
		}
	}
	if post == nil {
		return false
	}
	pre := new(big.Rat).Sub(post, delta)
	if pre.Sign() == 0 || pre.Sign() == delta.Sign() {
		return false // opening from flat, or adding to an existing position
	}
	// Opposite signs: the order works against the position. It only REDUCES it if
	// the result has not crossed to the other side.
	return post.Sign() == 0 || post.Sign() == pre.Sign()
}

// stamp renders an observation time for evidence. The zero time is rendered as
// "unknown" rather than as year 1, because an evidence map is read by a person
// deciding what to go and look at.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// itoa renders an exclusion count for evidence.
func itoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }
