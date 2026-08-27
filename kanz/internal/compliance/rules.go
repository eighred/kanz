package compliance

import (
	"context"
	"math/big"
	"sort"
	"strconv"
	"strings"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// The COMP-01 baseline rule set. Each function is a pure RuleFunc: it returns
// nil when the rule holds, or a *Violation (Message + Evidence; the engine
// stamps rule_id/type/severity) when it does not. A rule whose params do not
// match its type fails closed via paramsMismatch — deny-by-default.

// ConcentrationRule enforces a ConcentrationLimit: a bucket's share of gross
// exposure must not exceed max_weight. With an empty bucket the cap applies to
// every bucket on the dimension; the first (lexicographically) breaching bucket
// is reported.
//
// A LIMIT ON A DIMENSION THE PLATFORM CANNOT RESOLVE IS A REFUSAL, not a pass —
// see unresolvedDimension. Before that check existed, "no more than 10% in TECH"
// looked up an empty bucket map, found nothing, took the not-held branch and
// returned nil: a book 100% in technology was ADMITTED under a 10% technology
// cap, and the audit trail recorded a check that ran and approved (#640).
func ConcentrationRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	cl := rule.GetConcentration()
	if cl == nil {
		return paramsMismatch("concentration")
	}
	// BEFORE totalGross, and that order is the fix rather than a detail: a book
	// whose every holding is unmarked totals ZERO, which is indistinguishable
	// from an empty book and would take the "nothing is concentrated" branch
	// below (#760).
	if v := unmarkedHoldings(c, "concentration"); v != nil {
		return v
	}
	total := totalGross(c)
	if total.Sign() == 0 {
		return nil // empty book: nothing is concentrated
	}
	if v := unresolvedDimension(c, "concentration", cl.GetDimension()); v != nil {
		return v
	}
	max := ratFromDecimal(cl.GetMaxWeight())
	buckets := grossByDimension(c, cl.GetDimension())

	check := func(key string, g *big.Rat) *compliancepb.Violation {
		weight := new(big.Rat).Quo(g, total)
		if weight.Cmp(max) <= 0 {
			return nil
		}
		return &compliancepb.Violation{
			Message: "concentration exceeds limit",
			Evidence: map[string]string{
				"dimension": cl.GetDimension().String(),
				"bucket":    key,
				"observed":  ratString(weight),
				"limit":     ratString(max),
			},
		}
	}

	if cl.GetBucket() != "" {
		g := buckets[cl.GetBucket()]
		if g == nil {
			return nil // not held ⇒ zero weight
		}
		return check(cl.GetBucket(), g)
	}
	for _, key := range sortedKeys(buckets) {
		if v := check(key, buckets[key]); v != nil {
			return v
		}
	}
	return nil
}

// RestrictionRule enforces a RestrictionList: in DENY mode a held bucket in the
// list breaches; in ALLOW_ONLY mode a held bucket NOT in the list breaches; an
// unspecified mode denies any holding by default.
//
// A DENY LIST ON AN UNRESOLVABLE DIMENSION IS A REFUSAL, not a pass — see
// unresolvedDimension. "Hold nothing in TOBACCO" against an unresolved sector
// compared "TOBACCO" with the empty bucket every holding fell into, matched
// nothing, and admitted the book (#640). The ALLOW_ONLY half failed the other
// way — the empty bucket was never on the allow list, so every portfolio
// breached — and a rule that is wrong in both directions is not a rule.
func RestrictionRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	rl := rule.GetRestriction()
	if rl == nil {
		return paramsMismatch("restriction")
	}
	// "Hold nothing in TOBACCO" is not satisfied by a holding the engine could
	// not see (#760) — the same argument unresolvedDimension makes one field over.
	if v := unmarkedHoldings(c, "restriction"); v != nil {
		return v
	}
	if v := unresolvedDimension(c, "restriction", rl.GetDimension()); v != nil {
		return v
	}
	set := toSet(rl.GetValues())
	mode := rl.GetMode()
	for _, pos := range heldPositions(c) {
		key := bucketKey(c, rl.GetDimension(), pos)
		var bad bool
		switch mode {
		case compliancepb.RestrictionMode_RESTRICTION_MODE_DENY:
			bad = set[key]
		case compliancepb.RestrictionMode_RESTRICTION_MODE_ALLOW_ONLY:
			bad = !set[key]
		default:
			bad = true // unspecified mode ⇒ deny-by-default
		}
		if bad {
			return &compliancepb.Violation{
				Message: "instrument violates restriction list",
				Evidence: map[string]string{
					"dimension":  rl.GetDimension().String(),
					"bucket":     key,
					"mode":       mode.String(),
					"instrument": pos.InstrumentID,
				},
			}
		}
	}
	return nil
}

// IssuerExclusionRule breaches when the book holds any instrument whose issuer
// is on the exclusion list.
//
// A HOLDING WHOSE ISSUER THE PLATFORM CANNOT NAME IS A REFUSAL — see
// unresolvedDimension. This doc used to say the opposite: that an unidentifiable
// instrument "does not match a listed issuer — you cannot exclude what you
// cannot name", and that "unclassified holdings are surfaced separately, not
// silently blocked here". The first half is true and irrelevant; the second half
// was never true. NOTHING SURFACED THEM. No classifier is constructed at any
// composition root, so every holding on this estate had an empty issuer, no
// exclusion could ever match, and the rule reported PASS for a book that might
// have been entirely in the excluded issuer (#640).
//
// "You cannot exclude what you cannot name" is an argument for refusing to
// answer, not for answering no. The honest reading of a tobacco exclusion
// against a book of unidentifiable issuers is that the mandate cannot be
// checked — which is what this now says, under the rule's own severity, in the
// same shape LeverageRule uses for an absent NAV.
func IssuerExclusionRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	ie := rule.GetIssuerExclusion()
	if ie == nil {
		return paramsMismatch("issuer_exclusion")
	}
	if v := unmarkedHoldings(c, "issuer exclusion"); v != nil {
		return v
	}
	// The dimension is implicit in the rule type: an issuer exclusion is an
	// ISSUER-dimension control, so it is unresolvable under exactly the same
	// conditions a DIMENSION_ISSUER restriction is.
	if v := unresolvedDimension(c, "issuer exclusion", compliancepb.Dimension_DIMENSION_ISSUER); v != nil {
		return v
	}
	excluded := toSet(ie.GetIssuerIds())
	for _, pos := range heldPositions(c) {
		issuer := classify(c, pos).Issuer
		if issuer != "" && excluded[issuer] {
			return &compliancepb.Violation{
				Message: "holding in excluded issuer",
				Evidence: map[string]string{
					"issuer":     issuer,
					"instrument": pos.InstrumentID,
				},
			}
		}
	}
	return nil
}

// LeverageRule enforces a LeverageCap: gross exposure / NAV must not exceed
// max_gross_leverage. A non-positive or absent NAV fails closed — leverage
// cannot be verified, so the book is denied by default.
func LeverageRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	lc := rule.GetLeverageCap()
	if lc == nil {
		return paramsMismatch("leverage_cap")
	}
	nav := absRatFromMoney(c.Book.NAV)
	if c.Book.NAV == nil || nav.Sign() == 0 {
		return &compliancepb.Violation{
			Message:  "leverage cannot be verified: NAV unavailable",
			Evidence: map[string]string{"nav": "unavailable"},
		}
	}
	// THE FAIL-OPEN DIRECTION (#760). Gross is a SUM: a dropped holding shrinks
	// the numerator, so a book reads as less levered for carrying something
	// nobody could price.
	if v := unmarkedHoldings(c, "leverage"); v != nil {
		return v
	}
	gross := totalGross(c)
	max := ratFromDecimal(lc.GetMaxGrossLeverage())
	lev := new(big.Rat).Quo(gross, nav)
	if lev.Cmp(max) <= 0 {
		return nil
	}
	return &compliancepb.Violation{
		Message: "gross leverage exceeds cap",
		Evidence: map[string]string{
			"observed": ratString(lev),
			"limit":    ratString(max),
		},
	}
}

// BuyingPowerRule enforces a BuyingPowerLimit: the portfolio's cash AFTER the
// trade must not fall below min_cash_after (#415).
//
// IT IS THE ONLY RULE HERE THAT ASKS WHETHER THE FUND CAN AFFORD THE ORDER.
// Every other one bounds the SHAPE of the book — how concentrated, how levered,
// which instruments and currencies. Margin sufficiency was otherwise discovered
// from the exchange, after dispatch, which is the wrong side of the trade to
// find out on.
//
// ABSENT CASH FAILS CLOSED, exactly as an absent NAV does for LeverageRule, and
// for a sharper reason. Treating unknown cash as unlimited would admit every
// order while a mandate declares a spending limit — a control that reports
// success. A portfolio whose cash the platform cannot establish is one whose
// affordability it cannot establish, and the honest answer to "can we afford
// this" is then no.
//
// THAT IS SAFE TO SHIP AHEAD OF THE CASH SOURCE because the rule only runs when
// a mandate DECLARES this limit. Nothing declares it today, so no order changes
// behaviour; a deployment that declares it before wiring cash gets a loud
// refusal naming the reason, not a silent pass.
//
// The comparison is in the portfolio's BASE CURRENCY, like NAV and like the
// leverage denominator. A per-currency floor needs the FX layer and is a
// different rule; approximating it here would make the simple case wrong
// invisibly.
//
// A REFUSAL SAYS WHAT THE BALANCE WAS MISSING (#614). Book.CashCompleteness
// carries the producer's own statement of which journal entry types its
// deployment feeds, and every refusal here records it — see attributeCash. The
// verdict is unchanged by it; what changes is that "the fund is out of money"
// and "the platform never received the dividend" stop being the same sentence in
// the audit trail.
func BuyingPowerRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	bp := rule.GetBuyingPower()
	if bp == nil {
		return paramsMismatch("buying_power")
	}
	if c.Book.Cash == nil {
		return &compliancepb.Violation{
			Message:  "buying power cannot be verified: cash balance unavailable",
			Evidence: map[string]string{"cash": "unavailable"},
		}
	}
	after := ratFromDecimal(c.Book.Cash.GetAmount())
	floor := ratFromDecimal(bp.GetMinCashAfter())
	if after.Cmp(floor) >= 0 {
		return nil
	}
	v := &compliancepb.Violation{
		Message: "order would spend below the permitted cash floor",
		Evidence: map[string]string{
			"cash_after": ratString(after),
			"floor":      ratString(floor),
			"currency":   c.Book.Cash.GetCurrencyCode(),
		},
	}
	attributeCash(v, c.Book.CashCompleteness)
	return v
}

// Evidence keys carrying what the refused balance was known to contain (#614).
// Named constants because a test asserting on a literal and a rule writing a
// different literal is a guard that checks nothing.
const (
	// EvidenceBalanceCompleteness is "complete", "incomplete" or "unstated".
	EvidenceBalanceCompleteness = "balance_completeness"
	// EvidenceBalanceOmits lists the entry types the balance is missing,
	// comma-separated, and is present only when completeness is "incomplete".
	EvidenceBalanceOmits = "balance_omits"
)

// attributeCash records, ON A REFUSAL, what the balance that caused it was known
// to be missing (#614).
//
// # Why a refusal needs this at all
//
// BuyingPowerRule is the one control that asks whether the fund can AFFORD the
// order, and the number it asks with is announced by accounting. Two of the six
// journal entry types that number is folded from have no producer anywhere on
// this platform (#588): nothing publishes a corporate action, so no dividend,
// coupon or merger cash has ever reached the book, and nothing posts an accrual.
// A portfolio that was paid a dividend is therefore refused on a balance that
// does not contain it — and until now that refusal was spelled exactly like a
// mandate's spending limit being hit. "Nothing configured" and "checked, and
// fine" looked the same, which is the failure mode this platform designs
// against.
//
// # It changes the WORDS and never the VERDICT
//
// The order is still refused. That is not timidity, it is the only answer that
// is not a guess: the direction of the error is unknown. accounting's
// foldCorpAct pays quantity x per-unit with the SIGN of the holding, so an
// unfolded dividend understates the cash of a book that is long the instrument
// and OVERSTATES the cash of one that is short it. Admitting the order — or
// grossing the balance up by some assumed entitlement — would convert a control
// that refuses too much into one that admits what the fund cannot pay for, and
// that trade is strictly worse than the bug. What a reader gets instead is the
// ability to tell the two refusals apart and to go and look at the feed.
//
// # Three answers, because the reader's next action differs
//
//	complete    the producer stated that everything it folds is fed. This is a
//	            spending limit, and it was reached.
//	incomplete  the producer named entry types nothing feeds. The refusal may be
//	            an artifact of the missing feed; balance_omits says which one.
//	unstated    nobody said. An accounting old enough to predate the statement,
//	            or a composition root that forgot to pass it. Not evidence of
//	            completeness — the absence of evidence either way.
func attributeCash(v *compliancepb.Violation, cc *CashCompleteness) {
	switch {
	case !cc.Stated():
		v.Evidence[EvidenceBalanceCompleteness] = "unstated"
		v.Message = "order is below the permitted cash floor, but the balance's producer did not " +
			"state what that balance contains — this refusal has not been shown to be a spending limit"
	case cc.Incomplete():
		v.Evidence[EvidenceBalanceCompleteness] = "incomplete"
		v.Evidence[EvidenceBalanceOmits] = strings.Join(cc.OmittedEntryTypes, ",")
		v.Message = "order is below the permitted cash floor of a balance the book of record says is " +
			"INCOMPLETE — nothing feeds the entry types in balance_omits, so this refusal may be a " +
			"missing feed rather than a spending limit"
	default:
		v.Evidence[EvidenceBalanceCompleteness] = "complete"
	}
}

// CurrencyRule breaches when the book holds a position denominated in a currency
// outside the allowed set.
func CurrencyRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	cr := rule.GetCurrencyRestriction()
	if cr == nil {
		return paramsMismatch("currency_restriction")
	}
	// The currency is read off the market value, so an unmarked holding has no
	// currency to check and silently satisfied any restriction (#760).
	if v := unmarkedHoldings(c, "currency restriction"); v != nil {
		return v
	}
	allowed := toSet(cr.GetAllowedCurrencies())
	for _, pos := range heldPositions(c) {
		ccy := pos.MarketValue.GetCurrencyCode()
		if ccy != "" && !allowed[ccy] {
			return &compliancepb.Violation{
				Message: "position in disallowed currency",
				Evidence: map[string]string{
					"currency":   ccy,
					"instrument": pos.InstrumentID,
				},
			}
		}
	}
	return nil
}

// --- shared rule helpers ----------------------------------------------------

// paramsMismatch is the deny-by-default violation for a rule whose params do
// not match its declared type (a misconfigured mandate fails closed).
func paramsMismatch(want string) *compliancepb.Violation {
	return &compliancepb.Violation{
		Message:  "rule params do not match rule type — denied by default",
		Evidence: map[string]string{"expected_params": want},
	}
}

// totalGross is the book's gross exposure — Σ|market value| over held positions,
// the same gross definition compute.ComputeExposure uses (RISK-06), as an exact
// Rat.
func totalGross(c *Candidate) *big.Rat {
	sum := new(big.Rat)
	for _, pos := range heldPositions(c) {
		sum.Add(sum, absRatFromMoney(pos.MarketValue))
	}
	return sum
}

// grossByDimension buckets gross exposure on a dimension.
func grossByDimension(c *Candidate, dim compliancepb.Dimension) map[string]*big.Rat {
	out := map[string]*big.Rat{}
	for _, pos := range heldPositions(c) {
		key := bucketKey(c, dim, pos)
		amt := absRatFromMoney(pos.MarketValue)
		if g, ok := out[key]; ok {
			g.Add(g, amt)
		} else {
			out[key] = amt
		}
	}
	return out
}

// bucketKey resolves the bucket a position falls in on a dimension.
func bucketKey(c *Candidate, dim compliancepb.Dimension, pos Position) string {
	switch dim {
	case compliancepb.Dimension_DIMENSION_INSTRUMENT:
		return pos.InstrumentID
	case compliancepb.Dimension_DIMENSION_CURRENCY:
		return pos.MarketValue.GetCurrencyCode()
	case compliancepb.Dimension_DIMENSION_ISSUER:
		return classify(c, pos).Issuer
	case compliancepb.Dimension_DIMENSION_SECTOR:
		return classify(c, pos).Sector
	case compliancepb.Dimension_DIMENSION_ASSET_CLASS:
		return classify(c, pos).AssetClass
	default:
		return ""
	}
}

// classifiedDimension reports whether a dimension can ONLY be answered through
// the Classifier. INSTRUMENT and CURRENCY come off the Position itself, so they
// are unaffected by a missing classifier and must not be refused.
func classifiedDimension(dim compliancepb.Dimension) bool {
	switch dim {
	case compliancepb.Dimension_DIMENSION_ISSUER,
		compliancepb.Dimension_DIMENSION_SECTOR,
		compliancepb.Dimension_DIMENSION_ASSET_CLASS:
		return true
	default:
		return false
	}
}

// unresolvedDimension is the deny-by-default violation for a rule whose
// dimension the platform cannot resolve for every holding. It returns nil when
// the dimension needs no classifier, when the book holds nothing, or when every
// held position resolves — so a rule that CAN be evaluated is evaluated exactly
// as before.
//
// # It is the same shape as an absent NAV or an unknown risk measure
//
// LeverageRule refuses without a NAV, BuyingPowerRule without cash, and
// RiskLimitRule without the measure it names. This is the fourth instance of one
// rule: A CONTROL WHOSE INPUT THE PLATFORM CANNOT ESTABLISH REFUSES. The three
// classified dimensions had no such check, so they were the ones that answered
// PASS instead (#640).
//
// # Why an unresolvable rule is not simply skipped
//
// Skipping would put "no rule breached" and "the rule could not be evaluated"
// behind the same observable outcome — an ALLOWED order and a PASS in the audit
// trail. The whole reason this platform records a compliance decision is so
// somebody can later ask what was checked; a decision that says a sector cap
// passed, when nothing on this estate can resolve a sector, is a worse artifact
// than no decision at all.
//
// # Two causes, told apart, because the operator's next action differs
//
// NO CLASSIFIER AT ALL means the deployment has no instrument reference source
// wired — nobody can fix that per-instrument, and the fix is a composition-root
// change. A classifier that is present but does not know THESE instruments is a
// reference-data gap, and the evidence names the holdings so somebody can go and
// load them. Collapsing them would send an operator to the wrong place.
//
// # Severity is the rule's own
//
// The engine stamps violationSeverity(rule), so an advisory (WARN) rule that
// cannot be evaluated is an advisory failure rather than a rejection — the same
// treatment every other cannot-be-verified violation gets. Deviating here would
// make this rule family the one place where a mandate's declared severity does
// not hold.
func unresolvedDimension(c *Candidate, what string, dim compliancepb.Dimension) *compliancepb.Violation {
	if !classifiedDimension(dim) {
		return nil
	}
	held := heldPositions(c)
	if len(held) == 0 {
		return nil // nothing held: no holding's dimension is in question
	}
	if c.Classifier == nil {
		return &compliancepb.Violation{
			Message: what + " cannot be verified: no instrument classifier is wired, so this dimension cannot be resolved for any holding",
			Evidence: map[string]string{
				"dimension":  dim.String(),
				"classifier": "unavailable",
				"holdings":   strconv.Itoa(len(held)),
			},
		}
	}
	var unresolved []string
	for _, pos := range held {
		if bucketKey(c, dim, pos) == "" {
			unresolved = append(unresolved, pos.InstrumentID)
		}
	}
	if len(unresolved) == 0 {
		return nil
	}
	// A BOUNDED SAMPLE, for v1.InputCoverage's reason: on a book whose reference
	// data was never loaded every holding is unresolved, and putting the whole
	// book in a violation would put it in the audit stream too. The count carries
	// the magnitude; the sample is what lets somebody go and look.
	sample := unresolved
	if len(sample) > maxUnresolvedSample {
		sample = sample[:maxUnresolvedSample]
	}
	return &compliancepb.Violation{
		Message: what + " cannot be verified: the classifier does not resolve this dimension for every holding",
		Evidence: map[string]string{
			"dimension":              dim.String(),
			"classifier":             "present",
			"holdings":               strconv.Itoa(len(held)),
			"unresolved":             strconv.Itoa(len(unresolved)),
			"unresolved_instruments": strings.Join(sample, ","),
		},
	}
}

// maxUnresolvedSample bounds the instrument list in an unresolvedDimension
// violation. Mirrors v1.MaxInputExclusions in intent — one bound for the whole
// rule family so a violation's size cannot depend on which dimension is dark.
const maxUnresolvedSample = 32

// classify resolves a position's reference attributes via the candidate's
// classifier (empty Attributes when none is set or the instrument is unknown).
func classify(c *Candidate, pos Position) Attributes {
	if c.Classifier == nil {
		return Attributes{}
	}
	// context.TODO, not nil: no request context reaches the rule engine yet (the
	// evaluators take only a *Candidate). A nil ctx is not a placeholder, it is a
	// panic waiting for the first Classifier that does I/O.
	a, _ := c.Classifier.Classify(context.TODO(), pos.InstrumentID, c.AsOf)
	return a
}

// heldPositions returns positions with a non-zero market value, in stable
// instrument order — lineage-only flat positions (zero value) are not holdings
// and cannot breach.
//
// A POSITION IT DROPS IS ONE THAT WAS PRICED AND FOUND TO BE WORTH NOTHING, and
// never one nobody could price (#760). Those two used to leave through the same
// `continue`: an absent MarketValue read as zero, so an unmarked holding was
// removed from the book before any rule saw it. See unmarkedHoldings for what
// that cost and why the caller refuses instead.
func heldPositions(c *Candidate) []Position {
	out := make([]Position, 0, len(c.Book.Positions))
	for _, p := range c.Book.Positions {
		if unmarkedPosition(p) || absRatFromMoney(p.MarketValue).Sign() == 0 {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstrumentID < out[j].InstrumentID })
	return out
}

// unmarkedPosition reports that the platform could not price this holding.
//
// BOTH SPELLINGS OF THE ABSENCE COUNT. A nil MarketValue and a Money carrying a
// currency but no Amount reach absRatFromMoney identically and both come back
// zero, so catching only the first would leave the hole open to any producer
// that sets the currency and not the number.
//
// A MARK OF ZERO IS NOT THIS. That is a measurement — the platform looked, and
// the holding is worth nothing — and it stays a holding the rules evaluate
// normally. Conflating the two in that direction would make every worthless
// position unevaluable and the refusal meaningless.
func unmarkedPosition(p Position) bool {
	return p.MarketValue == nil || p.MarketValue.GetAmount() == nil
}

// unmarkedHoldings is the deny-by-default refusal for a book carrying a position
// nobody could price. It is the value-side twin of unresolvedDimension, and it
// returns nil — evaluate normally — when every holding is marked.
//
// # What the silence cost
//
// Every rule that reasons about the shape of the book funnels through
// heldPositions, which dropped an unmarked position outright. Three separate
// wrongs came out of that one filter, and only the first is the obvious one:
//
//   - The holding escaped limits written about it. A cap on the instrument
//     passed because the instrument was not in the book.
//   - Every OTHER bucket was measured against a denominator that had lost it, so
//     a violation could be raised carrying an `observed` weight that was never
//     measured. A breach on invented evidence is not the safe direction; it is a
//     different wrong answer.
//   - Gross leverage was UNDERSTATED, which is fail-open: Σ|market value| shrinks
//     when a holding vanishes, so a book reads as less levered for carrying
//     something nobody could price.
//
// # Why it refuses rather than estimating
//
// The estate's rule is that absent is UNKNOWN and never zero, because a zero is
// a claim about the book — venuemargin.go states it for a margin ratio, and
// LeverageRule already applies it to NAV ("leverage cannot be verified: NAV
// unavailable"). There is nothing to substitute for a mark: an unpriced holding
// could be any size, in either direction, so no bound on the answer survives.
func unmarkedHoldings(c *Candidate, what string) *compliancepb.Violation {
	var unmarked []string
	for _, p := range c.Book.Positions {
		if unmarkedPosition(p) {
			unmarked = append(unmarked, p.InstrumentID)
		}
	}
	if len(unmarked) == 0 {
		return nil
	}
	sort.Strings(unmarked)
	// A BOUNDED SAMPLE, for the reason unresolvedDimension bounds its own: on a
	// book whose marks never loaded every holding is unmarked, and putting the
	// whole book in a violation puts it in the audit stream too. The count
	// carries the magnitude; the sample is what lets somebody go and look.
	sample := unmarked
	if len(sample) > maxUnresolvedSample {
		sample = sample[:maxUnresolvedSample]
	}
	return &compliancepb.Violation{
		Message: what + " cannot be verified: the book carries a position with no market value, " +
			"so neither its own exposure nor the book's total is known",
		Evidence: map[string]string{
			EvidenceUnmarkedHoldings: strconv.Itoa(len(unmarked)),
			EvidenceUnmarkedSample:   strings.Join(sample, ","),
		},
	}
}

// Evidence keys naming the holdings the platform could not price (#760). Named
// constants because a test asserting on a literal and a rule writing a different
// literal is a guard that checks nothing.
const (
	// EvidenceUnmarkedHoldings is the TOTAL count of unmarked positions, not the
	// length of the sample.
	EvidenceUnmarkedHoldings = "unmarked_holdings"
	// EvidenceUnmarkedSample is a bounded, comma-separated instrument list.
	EvidenceUnmarkedSample = "unmarked_sample"
)

func toSet(vals []string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}

func sortedKeys(m map[string]*big.Rat) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RiskLimitRule enforces a RiskMeasureLimit: a measure the RISK ENGINE computes
// must not exceed the mandate's ceiling (#438).
//
// IT IS THE FIRST RULE HERE WHOSE INPUT THE OMS CANNOT DERIVE. Every other rule
// bounds the SHAPE of the book from the holdings the OMS already has — how
// concentrated, how levered, which instruments and currencies. VaR, expected
// shortfall and factor decomposition come from a model, a covariance estimate
// and a history, and until this rule existed order admission could not ask for
// any of them. An order could sit inside every mandate rule on this platform and
// still take the portfolio through its VaR limit; the platform would admit it,
// execute it, and only then compute how bad it was.
//
// THE MEASURE IS PRE-COMPUTED, NOT REQUESTED. This reads the OMS's local fold of
// risk.portfolio.measures_computed — arithmetic on a map, microseconds, no
// network. #438 rules out a synchronous call for a reason: it would put an
// unbounded, externally-owned latency in front of every order, and a degraded
// risk engine would become a trading outage rather than a refusal.
//
// AN UNKNOWN MEASURE FAILS CLOSED, and the four ways to be unknown are one
// answer: never announced, absent from the last announcement, too old to be
// current, or announced with a coverage record saying the engine computed it
// over a book it could not resolve (#509). Admitting an order because the
// platform could not establish its risk is a control that reports success — and
// it is worse here than for cash, because a portfolio whose risk cannot be
// computed is exactly the one nobody should be adding to.
//
// THE FOURTH IS THE ONE THAT LOOKS LIKE AN ANSWER. The other three are silence,
// which this rule was built to refuse. A measure computed over nothing is a
// number, announced on time, in range — a DV01 of zero passes every rate-risk
// ceiling ever written. riskview declines to fold it for exactly that reason, so
// it arrives here as unknown rather than as a pass.
//
// A LIMIT ON A MEASURE THE ENGINE DOES NOT PRODUCE IS ALSO A REFUSAL, by the
// same path. measure_name is the engine's own name rather than an enum mirrored
// into the schema, so a mandate CAN name something nothing computes — and the
// honest answer to "your limit refers to a number that does not exist" is not to
// let the order through.
//
// THE CHECK IS ON THE BOOK AS IT STANDS, not on the book after the order. That
// is a real limitation and it is stated rather than hidden: projecting a VaR
// through a candidate trade needs the covariance the risk engine holds and the
// OMS does not, which is the marginal-risk calculation #438's own follow-up work
// describes. What this catches today is an order arriving at a portfolio ALREADY
// over its limit — the case where the platform currently has nothing to say at
// all.
func RiskLimitRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	rl := rule.GetRiskMeasure()
	if rl == nil {
		return paramsMismatch("risk_measure")
	}
	name := rl.GetMeasureName()
	if name == "" {
		// A limit that names no measure cannot be checked and must not read as
		// satisfied — the mandate declared a control and did not say on what.
		return &compliancepb.Violation{
			Message:  "risk limit names no measure",
			Evidence: map[string]string{"measure": "unspecified"},
		}
	}
	if c.Book.Risk == nil {
		return &compliancepb.Violation{
			Message: "risk limit cannot be verified: no risk measures available",
			Evidence: map[string]string{
				"measure": name,
				"risk":    "unavailable",
			},
		}
	}
	observed, ok := c.Book.Risk(name)
	if !ok {
		return &compliancepb.Violation{
			Message: "risk limit cannot be verified: measure unknown or not current",
			Evidence: map[string]string{
				"measure": name,
				"risk":    "unknown",
			},
		}
	}
	limit := ratFromDecimal(rl.GetMaxValue())
	if observed.Cmp(limit) <= 0 {
		return nil
	}
	return &compliancepb.Violation{
		Message: "order refused: the portfolio is over its risk limit",
		Evidence: map[string]string{
			"measure":  name,
			"observed": ratString(observed),
			"limit":    ratString(limit),
		},
	}
}
