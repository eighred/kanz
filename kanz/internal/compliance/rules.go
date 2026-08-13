package compliance

import (
	"context"
	"math/big"
	"sort"

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
func ConcentrationRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	cl := rule.GetConcentration()
	if cl == nil {
		return paramsMismatch("concentration")
	}
	total := totalGross(c)
	if total.Sign() == 0 {
		return nil // empty book: nothing is concentrated
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
func RestrictionRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	rl := rule.GetRestriction()
	if rl == nil {
		return paramsMismatch("restriction")
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
// is on the exclusion list. An instrument the classifier cannot identify
// (empty issuer) does not match a listed issuer — you cannot exclude what you
// cannot name; unclassified holdings are surfaced separately, not silently
// blocked here.
func IssuerExclusionRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	ie := rule.GetIssuerExclusion()
	if ie == nil {
		return paramsMismatch("issuer_exclusion")
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
	return &compliancepb.Violation{
		Message: "order would spend below the permitted cash floor",
		Evidence: map[string]string{
			"cash_after": ratString(after),
			"floor":      ratString(floor),
			"currency":   c.Book.Cash.GetCurrencyCode(),
		},
	}
}

// CurrencyRule breaches when the book holds a position denominated in a currency
// outside the allowed set.
func CurrencyRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	cr := rule.GetCurrencyRestriction()
	if cr == nil {
		return paramsMismatch("currency_restriction")
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
func heldPositions(c *Candidate) []Position {
	out := make([]Position, 0, len(c.Book.Positions))
	for _, p := range c.Book.Positions {
		if p.MarketValue == nil || absRatFromMoney(p.MarketValue).Sign() == 0 {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstrumentID < out[j].InstrumentID })
	return out
}

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
