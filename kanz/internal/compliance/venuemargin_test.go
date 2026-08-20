package compliance

import (
	"math/big"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// THE MARGIN HALF OF THE PRE-TRADE GATE (#408, control 3).
//
// #408's ruling: margin turns "we lost money on a trade" into "the exchange sold
// our collateral while we were reading stale books". Every test below is an
// answer to "what does this gate do when it does not know", and there is exactly
// one right answer.

var observed = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

func marginRule(venue string, floor *commonpb.Decimal) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "m-1",
		Type:   compliancepb.RuleType_RULE_TYPE_VENUE_MARGIN,
		Params: &compliancepb.Rule_VenueMargin{
			VenueMargin: &compliancepb.VenueMarginLimit{
				Venue:          venue,
				MinMarginRatio: floor,
			},
		},
	}
}

// candidate builds a projected book holding `post` units of BTC after an order
// of `delta` units at `venue`, with a margin source answering `state`/`ok`.
func candidate(post, delta int64, venue string, state MarginState, ok bool) *Candidate {
	c := &Candidate{
		Book: &Book{
			PortfolioID:  "fund-alpha",
			BaseCurrency: "USD",
			Positions: []Position{{
				InstrumentID: "BTC",
				Quantity:     &commonpb.Decimal{Coefficient: post},
				MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: post * 100}, CurrencyCode: "USD"},
			}},
		},
		AsOf: observed,
		Order: &CandidateOrder{
			InstrumentID:   "BTC",
			SignedQuantity: &commonpb.Decimal{Coefficient: delta},
			Venue:          venue,
		},
		Margin: func(string) (MarginState, bool) { return state, ok },
	}
	return c
}

// A HEALTHY, CURRENT, FULLY-COVERED ACCOUNT PASSES.
//
// The companion to every refusal below: a control that refuses everything is a
// trading outage wearing the shape of a control, and it would be indistinguishable
// from a correct implementation without this test.
func TestVenueMarginRule_ReadCurrentAndCompletePasses(t *testing.T) {
	c := candidate(10, 10, "XOKX", MarginState{
		Account: "okx-sub-1", Ratio: big.NewRat(5, 1), ObservedAt: observed,
		CoverageReported: true,
	}, true)

	if v := VenueMarginRule(c, marginRule("XOKX", nil)); v != nil {
		t.Fatalf("a fully reported, current, complete margin state was REFUSED: %s %v",
			v.GetMessage(), v.GetEvidence())
	}
}

// UNKNOWN REFUSES. The first property in #408's ruling and the reason the whole
// set exists: "unknown margin refuses the order. It does not proceed on a
// default, a stale cache, or a zero."
//
// ok=false is the single answer venuemargin gives for never-observed, observed
// too long ago, and bound to no account — deliberately one answer, so no caller
// can treat one of them as passable.
func TestVenueMarginRule_UnknownRefuses(t *testing.T) {
	c := candidate(10, 10, "XOKX", MarginState{}, false)

	v := VenueMarginRule(c, marginRule("XOKX", nil))
	if v == nil {
		t.Fatal("an order was ADMITTED against an account whose margin state is UNKNOWN — this is " +
			"the confident-zero defect on the one input where being wrong costs the fund its collateral")
	}
	if v.GetEvidence()["margin"] != "unknown" {
		t.Errorf("evidence = %v, want margin=unknown so an operator knows the feed is the problem",
			v.GetEvidence())
	}
}

// NO SOURCE WIRED REFUSES, under evidence of its own.
//
// "Nothing configured" and "checked, and fine" must never look the same, and on
// a margin feed they would: a portfolio with no leveraged position and a
// portfolio whose margin nobody has ever read both show no margin call.
//
// (STALE is the same refusal by a different road, and it is proven where the
// freshness bound actually lives — the rule never sees an age, because
// venuemargin.View applies DefaultMaxAge and hands back ok=false. See
// TestMarginSource_StaleIsUnknown in services/oms/internal/compliance.)
func TestVenueMarginRule_NoSourceWiredRefuses(t *testing.T) {
	c := candidate(10, 10, "XOKX", MarginState{}, true)
	c.Margin = nil

	v := VenueMarginRule(c, marginRule("XOKX", nil))
	if v == nil {
		t.Fatal("a mandate DECLARED margin trading and the order was ADMITTED with nothing observing " +
			"margin at all — 'nothing configured' must never look like 'checked, and fine'")
	}
	if v.GetEvidence()["margin"] != "unavailable" {
		t.Errorf("evidence = %v, want margin=unavailable — distinct from 'unknown', because the "+
			"operator action is to wire a source, not to chase a venue", v.GetEvidence())
	}
}

// ABSENT COVERAGE IS NOT ZERO EXCLUSIONS.
//
// domain.v1.InputCoverage's zero value means "this publisher does not report
// coverage", NOT "everything resolved" — stated on the message and in
// internal/risk/api/v1/engine.go. An observation that does not say what it left
// out cannot be shown to have left out nothing.
func TestVenueMarginRule_UnreportedCoverageRefuses(t *testing.T) {
	c := candidate(10, 10, "XOKX", MarginState{
		Account: "okx-sub-1", Ratio: big.NewRat(5, 1), ObservedAt: observed,
		CoverageReported: false, ExcludedCount: 0,
	}, true)

	v := VenueMarginRule(c, marginRule("XOKX", nil))
	if v == nil {
		t.Fatal("an observation that reports NO coverage was read as fully covered — this is " +
			"InputCoverage's zero value being taken for 'everything resolved', the exact " +
			"conflation presence-as-signal exists to break")
	}
	if v.GetEvidence()["coverage"] != "not_reported" {
		t.Errorf("evidence = %v, want coverage=not_reported and NOT coverage=incomplete: the "+
			"publisher is the thing to go and look at", v.GetEvidence())
	}
}

// REPORTED EXCLUSIONS REFUSE, and carry the count.
//
// InputCoverage's own contract: a caller gating on the value must treat a
// non-zero excluded_count as a REFUSAL TO ANSWER, not an annotation on a good
// number, because the direction of the error is not knowable.
func TestVenueMarginRule_ReportedExclusionsRefuse(t *testing.T) {
	c := candidate(10, 10, "XOKX", MarginState{
		Account: "okx-sub-1", Ratio: big.NewRat(5, 1), ObservedAt: observed,
		CoverageReported: true, ExcludedCount: 3,
	}, true)

	v := VenueMarginRule(c, marginRule("XOKX", nil))
	if v == nil {
		t.Fatal("an order was ADMITTED against a margin observation the venue could not answer in " +
			"full — the ratio beside the gap is not a smaller answer, it is a different one")
	}
	if v.GetEvidence()["coverage"] != "incomplete" || v.GetEvidence()["excluded"] != "3" {
		t.Errorf("evidence = %v, want coverage=incomplete and excluded=3", v.GetEvidence())
	}
}

// AN ABSENT RATIO IS UNKNOWN AND NEVER ZERO. A zero ratio is an active claim
// about the account; this is the absence of one, and the two must not collapse.
func TestVenueMarginRule_AbsentRatioRefuses(t *testing.T) {
	c := candidate(10, 10, "XOKX", MarginState{
		Account: "okx-sub-1", Ratio: nil, ObservedAt: observed, CoverageReported: true,
	}, true)

	v := VenueMarginRule(c, marginRule("XOKX", nil))
	if v == nil {
		t.Fatal("the venue reported NO margin ratio and the order was ADMITTED")
	}
	if v.GetEvidence()["margin_ratio"] != "unknown" {
		t.Errorf("evidence = %v, want margin_ratio=unknown", v.GetEvidence())
	}
}

// THE REFUSAL IS DATED. venuemargin makes the number inseparable from its
// observation time on purpose; this is the seam where the timestamp would
// otherwise be dropped, and a refusal an operator cannot date is one they cannot
// act on.
func TestVenueMarginRule_RefusalCarriesTheObservationTime(t *testing.T) {
	c := candidate(10, 10, "XOKX", MarginState{
		Account: "okx-sub-1", Ratio: big.NewRat(1, 1), ObservedAt: observed, CoverageReported: true,
	}, true)

	v := VenueMarginRule(c, marginRule("XOKX", &commonpb.Decimal{Coefficient: 5}))
	if v == nil {
		t.Fatal("a ratio of 1 under a floor of 5 was admitted")
	}
	if !strings.HasPrefix(v.GetEvidence()["observed_at"], "2026-08-20T12:00:00") {
		t.Errorf("evidence = %v, want observed_at naming when the VENUE's state was true",
			v.GetEvidence())
	}
	if v.GetEvidence()["account"] != "okx-sub-1" {
		t.Errorf("evidence = %v, want the exchange ACCOUNT named — it is the liquidation boundary",
			v.GetEvidence())
	}
}

// A FLOOR IS A FLOOR: at it passes, below it refuses.
func TestVenueMarginRule_FloorBoundary(t *testing.T) {
	floor := &commonpb.Decimal{Coefficient: 5, Exponent: -1} // 0.5

	at := candidate(10, 10, "XOKX", MarginState{
		Account: "a", Ratio: big.NewRat(1, 2), ObservedAt: observed, CoverageReported: true,
	}, true)
	if v := VenueMarginRule(at, marginRule("XOKX", floor)); v != nil {
		t.Errorf("a ratio exactly at the floor was refused: %v", v.GetEvidence())
	}

	below := candidate(10, 10, "XOKX", MarginState{
		Account: "a", Ratio: big.NewRat(49, 100), ObservedAt: observed, CoverageReported: true,
	}, true)
	v := VenueMarginRule(below, marginRule("XOKX", floor))
	if v == nil {
		t.Fatal("a ratio below the declared floor was ADMITTED")
	}
	if !strings.HasPrefix(v.GetEvidence()["floor"], "0.5") {
		t.Errorf("evidence = %v, want the floor named", v.GetEvidence())
	}
}

// A FLOOR WITHOUT A VENUE IS REFUSED, NOT APPLIED.
//
// Margin ratios do not agree across venues on units or on which way danger lies
// (OKX's rises as the account gets safer). Applying an unattributed floor to
// whichever venue the order reached would be this platform asserting a
// convention on an exchange's behalf.
func TestVenueMarginRule_FloorWithoutAVenueIsRefused(t *testing.T) {
	c := candidate(10, 10, "XOKX", MarginState{
		Account: "a", Ratio: big.NewRat(99, 1), ObservedAt: observed, CoverageReported: true,
	}, true)

	v := VenueMarginRule(c, marginRule("", &commonpb.Decimal{Coefficient: 5, Exponent: -1}))
	if v == nil {
		t.Fatal("a margin floor naming no venue was APPLIED — an unattributed ratio has no " +
			"direction, so this passed a healthy-looking number that means nothing")
	}
	if v.GetEvidence()["venue"] != "unspecified" {
		t.Errorf("evidence = %v, want venue=unspecified", v.GetEvidence())
	}
}

// AN EMPTY VENUE ON THE RULE GOVERNS EVERY VENUE. It can only ever gate MORE
// orders, never fewer, which is why it is the permitted default when no floor is
// declared.
func TestVenueMarginRule_EmptyVenueGovernsWhereverTheOrderRoutes(t *testing.T) {
	c := candidate(10, 10, "XBIN", MarginState{}, false)

	if v := VenueMarginRule(c, marginRule("", nil)); v == nil {
		t.Fatal("a rule governing EVERY venue admitted an order to a venue whose margin is unknown")
	}
}

// A RULE SCOPED TO ANOTHER VENUE DOES NOT APPLY. It is neither satisfied nor
// breached: the order is not going where the rule governs, and a portfolio
// trading on margin at two venues declares two rules.
func TestVenueMarginRule_ScopedToAnotherVenueDoesNotApply(t *testing.T) {
	c := candidate(10, 10, "XBIN", MarginState{}, false)

	if v := VenueMarginRule(c, marginRule("XOKX", nil)); v != nil {
		t.Fatalf("an OKX margin rule refused an order routed to Binance: %v", v.GetEvidence())
	}
}

// AN ORDER NAMING NO VENUE, UNDER A RULE GOVERNING EVERY VENUE, IS REFUSED.
// Nothing identifies the exchange account whose collateral the order would
// spend, which is the question the rule exists to ask.
func TestVenueMarginRule_OrderWithNoVenueRefuses(t *testing.T) {
	c := candidate(10, 10, "", MarginState{}, false)

	v := VenueMarginRule(c, marginRule("", nil))
	if v == nil {
		t.Fatal("an order naming no venue was admitted under a margin mandate")
	}
	if v.GetEvidence()["venue"] != "unspecified" {
		t.Errorf("evidence = %v, want venue=unspecified", v.GetEvidence())
	}
}

// A MISMATCHED PARAMS BLOCK DENIES BY DEFAULT, like every other rule here.
func TestVenueMarginRule_ParamsMismatchDenies(t *testing.T) {
	c := candidate(10, 10, "XOKX", MarginState{
		Account: "a", Ratio: big.NewRat(9, 1), ObservedAt: observed, CoverageReported: true,
	}, true)
	rule := &compliancepb.Rule{RuleId: "m-1", Type: compliancepb.RuleType_RULE_TYPE_VENUE_MARGIN}

	if v := VenueMarginRule(c, rule); v == nil {
		t.Fatal("a venue-margin rule with no params was treated as satisfied")
	}
}

// REDUCING ORDERS ARE NOT GATED — the property that keeps this control from
// becoming a trap.
//
// The moment the margin feed goes quiet is the moment the fund most needs to cut
// a position. A gate refusing the close as well as the open holds it in the
// trade while the exchange decides what to sell.
func TestVenueMarginRule_ReducingOrdersPassWhileMarginIsDark(t *testing.T) {
	for _, tc := range []struct {
		name        string
		post, delta int64
	}{
		{"long trimmed", 4, -6},  // was 10 long, sold 6
		{"long closed", 0, -10},  // was 10 long, sold all
		{"short trimmed", -4, 6}, // was 10 short, bought 6
		{"short closed", 0, 10},  // was 10 short, bought all
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := candidate(tc.post, tc.delta, "XOKX", MarginState{}, false)
			if v := VenueMarginRule(c, marginRule("XOKX", nil)); v != nil {
				t.Fatalf("a DE-RISKING order was refused because margin was unknown: %s %v — "+
					"this traps the fund in the position it needs to leave",
					v.GetMessage(), v.GetEvidence())
			}
		})
	}
}

// TAKING OR INCREASING A POSITION IS GATED, and so is a FLIP — which closes and
// then takes a fresh position on the other side, i.e. exactly the thing this
// control is for. Flat-to-flat reduces nothing and is gated too.
func TestVenueMarginRule_TakingOrIncreasingIsGated(t *testing.T) {
	for _, tc := range []struct {
		name        string
		post, delta int64
	}{
		{"opened from flat", 10, 10},
		{"added to a long", 15, 5},
		{"added to a short", -15, -5},
		{"flipped long to short", -5, -15},
		{"flipped short to long", 5, 15},
		{"zero quantity", 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := candidate(tc.post, tc.delta, "XOKX", MarginState{}, false)
			if v := VenueMarginRule(c, marginRule("XOKX", nil)); v == nil {
				t.Fatalf("an order that TAKES or INCREASES a position (post=%d delta=%d) was "+
					"admitted while margin was UNKNOWN", tc.post, tc.delta)
			}
		})
	}
}

// THE POST-TRADE MONITOR HAS NO ORDER, AND STILL FAILS CLOSED. orderReducesPosition
// must answer false when it cannot be certain, or a nil order would exempt every
// re-evaluation from the control.
func TestVenueMarginRule_NoOrderStillEvaluates(t *testing.T) {
	c := candidate(10, 10, "XOKX", MarginState{}, false)
	c.Order = nil

	if v := VenueMarginRule(c, marginRule("XOKX", nil)); v == nil {
		t.Fatal("with no order in hand the rule reported the account satisfied — a nil order " +
			"must not read as 'this reduces the position'")
	}
}

// AN INSTRUMENT THE PROJECTION DOES NOT CARRY IS GATED. There is no pre-trade
// quantity to subtract from, so nothing can be shown to reduce.
func TestVenueMarginRule_UnprojectedInstrumentIsGated(t *testing.T) {
	c := candidate(10, -6, "XOKX", MarginState{}, false)
	c.Order.InstrumentID = "ETH"

	if v := VenueMarginRule(c, marginRule("XOKX", nil)); v == nil {
		t.Fatal("an order on an instrument absent from the projected book was treated as reducing")
	}
}

// THE RULE IS REGISTERED IN THE DEFAULT REGISTRY. Without this, a mandate
// declaring RULE_TYPE_VENUE_MARGIN would hit the engine's unknown-rule-type path
// — which denies by default, so the control would still refuse, but for the
// wrong reason and with evidence naming no venue and no account.
func TestDefaultRegistry_CarriesTheVenueMarginRule(t *testing.T) {
	r := DefaultRegistry()
	if _, ok := r.funcs[compliancepb.RuleType_RULE_TYPE_VENUE_MARGIN]; !ok {
		t.Fatal("RULE_TYPE_VENUE_MARGIN has no evaluator in DefaultRegistry")
	}
}
