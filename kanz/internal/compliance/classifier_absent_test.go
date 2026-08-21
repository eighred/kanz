package compliance

import (
	"context"
	"strings"
	"testing"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A MANDATE RULE NAMING A DIMENSION THE PLATFORM CANNOT RESOLVE MUST NOT PASS
// (#640).
//
// No compliance.Classifier is constructed at any composition root — the OMS
// pre-trade gate and the compliance monitor both pass nil, and no production
// implementation exists to pass instead. Every one of these tests therefore runs
// the PRODUCTION configuration, not a hypothetical one.
//
// What used to happen, and what makes this a P0 rather than a gap: classify()
// returned an empty Attributes for a nil classifier, bucketKey returned "" for
// SECTOR / ISSUER / ASSET_CLASS, and ConcentrationRule's bucket lookup found
// nothing and took its `not held ⇒ zero weight` branch. A book 100% in
// technology was ADMITTED under a 10% technology cap, and the decision was
// recorded as a pass — an audit artifact asserting a check that could not have
// run.

// techCapMandate is "no more than 10% in TECH", the mandate in the issue.
func techCapMandate() *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "sector-cap", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			Bucket:    "TECH",
			MaxWeight: dec(10, -2),
		}},
	}
}

// allTechBook is 100% technology by any honest reading — two tech names and
// nothing else.
func allTechBook() *Book {
	return &Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: money(200000, 0, "USD"),
		Positions: []Position{pos("AAPL", 100000, 0, "USD"), pos("MSFT", 100000, 0, "USD")},
	}
}

// TestConcentration_SectorBucketWithNoClassifierDoesNotPass is the assertion
// the issue names. It checks the STATUS rather than just the presence of a
// violation, because the gate admits on anything short of BREACH.
func TestConcentration_SectorBucketWithNoClassifierDoesNotPass(t *testing.T) {
	res := evalStatus(t, allTechBook(), nil, techCapMandate())
	if res.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("a 100%%-technology book PASSED a 10%% technology cap with no classifier — "+
			"the cap cannot be evaluated on this estate and must not read as satisfied: %v", res)
	}
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("status=%v want BREACH — the rule declares no on_violation, so deny-by-default "+
			"applies", res.GetStatus())
	}
	if len(res.GetViolations()) != 1 {
		t.Fatalf("violations=%v want exactly one", res.GetViolations())
	}
	v := res.GetViolations()[0]
	// "Cannot be verified" and "exceeds limit" must not be the same string: an
	// operator reading the audit trail has to be able to tell a breached limit
	// from an unevaluable one.
	if !strings.Contains(v.GetMessage(), "cannot be verified") {
		t.Errorf("message=%q must say the check could not be made, not that a limit was exceeded",
			v.GetMessage())
	}
	if got := v.GetEvidence()["classifier"]; got != "unavailable" {
		t.Errorf("evidence[classifier]=%q want %q — the reason must name the missing input",
			got, "unavailable")
	}
	if got := v.GetEvidence()["dimension"]; got != "DIMENSION_SECTOR" {
		t.Errorf("evidence[dimension]=%q want DIMENSION_SECTOR", got)
	}
}

// TestConcentration_UnbucketedSectorCapWithNoClassifierNamesTheCause covers the
// other half of the issue's first consequence. A cap with no bucket ("no sector
// above 25%") did NOT pass — it breached for every portfolio, with `"bucket": ""`
// as the evidence, which is a rule that is wrong in the safe direction and
// therefore still wrong: the operator was told a sector limit was exceeded and
// given an empty sector as proof.
func TestConcentration_UnbucketedSectorCapWithNoClassifierNamesTheCause(t *testing.T) {
	rule := &compliancepb.Rule{
		RuleId: "any-sector", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			MaxWeight: dec(25, -2),
		}},
	}
	res := evalStatus(t, allTechBook(), nil, rule)
	v := res.GetViolations()[0]
	if v.GetEvidence()["bucket"] == "" && v.GetEvidence()["classifier"] == "" {
		t.Fatalf("violation still reports an empty BUCKET as the offender rather than the "+
			"missing classifier: %v", v)
	}
	if !strings.Contains(v.GetMessage(), "cannot be verified") {
		t.Errorf("message=%q want the unevaluable form", v.GetMessage())
	}
}

// TestRestriction_SectorDenyWithNoClassifierDoesNotPass is consequence two.
func TestRestriction_SectorDenyWithNoClassifierDoesNotPass(t *testing.T) {
	rule := &compliancepb.Rule{
		RuleId: "no-tobacco", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
		Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			Values:    []string{"TOBACCO"},
			Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
		}},
	}
	res := evalStatus(t, allTechBook(), nil, rule)
	if res.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("a sector DENY list PASSED with no classifier — nothing can match the empty "+
			"bucket every holding falls into, so this exclusion could never fire: %v", res)
	}
	if got := res.GetViolations()[0].GetEvidence()["classifier"]; got != "unavailable" {
		t.Errorf("evidence[classifier]=%q want %q", got, "unavailable")
	}
}

// TestIssuerExclusion_WithNoClassifierDoesNotPass is the second half of
// consequence two: the ESG / sanctions path, which internal/sustainability.Screen
// also routes through.
func TestIssuerExclusion_WithNoClassifierDoesNotPass(t *testing.T) {
	rule := &compliancepb.Rule{
		RuleId: "excl", Type: compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION,
		Params: &compliancepb.Rule_IssuerExclusion{IssuerExclusion: &compliancepb.IssuerExclusion{
			IssuerIds: []string{"SANCTIONED_CO"},
		}},
	}
	res := evalStatus(t, allTechBook(), nil, rule)
	if res.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("an issuer exclusion PASSED with no classifier — every holding's issuer is "+
			"empty, so no listed issuer can ever match: %v", res)
	}
}

// TestUnresolvedDimension_PartialClassifierIsAlsoRefused is the layer out. A fix
// that only checked for a nil classifier would hand the silent pass straight
// back the day somebody wires a classifier covering most of the universe: the
// instruments missing from it fall into the same empty bucket.
// THE CAP IS 60%, NOT THE ISSUE'S 10%, and that matters. Under this classifier
// AAPL alone is 50% TECH, which a 10% cap breaches on its own — so the refusal
// would be satisfied by a real breach and the test would pass with the
// unresolved check deleted. A mutation proved exactly that. At 60% the resolved
// half is INSIDE the cap, so the only thing that can produce a non-PASS is the
// unknown holding: if MSFT is also TECH the book is 100% TECH and blows through.
func TestUnresolvedDimension_PartialClassifierIsAlsoRefused(t *testing.T) {
	cl := StaticClassifier{"AAPL": {Sector: "TECH"}} // MSFT missing
	rule := &compliancepb.Rule{
		RuleId: "sector-cap", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			Bucket:    "TECH",
			MaxWeight: dec(60, -2),
		}},
	}
	res := evalStatus(t, allTechBook(), cl, rule)
	if res.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("a partially-classified book PASSED a 60%% sector cap — the resolved half is "+
			"50%% TECH and MSFT's sector is unknown, so whether the book is over the cap is "+
			"unknown: %v", res)
	}
	v := res.GetViolations()[0]
	if got := v.GetEvidence()["classifier"]; got != "present" {
		t.Errorf("evidence[classifier]=%q want %q — a wired-but-incomplete classifier is a "+
			"reference-data gap, and sends an operator somewhere different from a missing one",
			got, "present")
	}
	if got := v.GetEvidence()["unresolved_instruments"]; got != "MSFT" {
		t.Errorf("evidence[unresolved_instruments]=%q want MSFT — the sample is what lets "+
			"somebody load the missing record", got)
	}
	if got := v.GetEvidence()["unresolved"]; got != "1" {
		t.Errorf("evidence[unresolved]=%q want 1", got)
	}
}

// TestUnresolvedDimension_FullyClassifiedBookIsStillEvaluated is the
// FALSE-REFUSAL arm. Without it every assertion above is satisfied by a rule
// family that refuses unconditionally, which would reject every sector mandate
// on the platform rather than the ones it cannot check.
func TestUnresolvedDimension_FullyClassifiedBookIsStillEvaluated(t *testing.T) {
	cl := StaticClassifier{"AAPL": {Sector: "TECH"}, "MSFT": {Sector: "ENERGY"}}
	b := allTechBook() // AAPL 50% TECH, MSFT 50% ENERGY under this classifier
	rule := &compliancepb.Rule{
		RuleId: "sector-cap", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			Bucket:    "TECH",
			MaxWeight: dec(60, -2), // 50% is inside a 60% cap
		}},
	}
	if got := evalStatus(t, b, cl, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("a fully classified book inside its cap must PASS, got %v", got)
	}
}

// TestUnresolvedDimension_UnclassifiedDimensionsAreUnaffected is the second
// false-refusal arm. INSTRUMENT and CURRENCY are read off the Position itself,
// so a missing classifier is irrelevant to them — and those are the dimensions
// every mandate on this estate actually uses. Refusing them would turn a
// compliance defect into a trading outage.
func TestUnresolvedDimension_UnclassifiedDimensionsAreUnaffected(t *testing.T) {
	instrumentCap := &compliancepb.Rule{
		RuleId: "inst", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
			MaxWeight: dec(60, -2), // each name is 50%
		}},
	}
	currencyOK := &compliancepb.Rule{
		RuleId: "ccy", Type: compliancepb.RuleType_RULE_TYPE_CURRENCY,
		Params: &compliancepb.Rule_CurrencyRestriction{CurrencyRestriction: &compliancepb.CurrencyRestriction{
			AllowedCurrencies: []string{"USD"},
		}},
	}
	if got := evalStatus(t, allTechBook(), nil, instrumentCap, currencyOK).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("INSTRUMENT and CURRENCY rules read the position directly and must be "+
			"unaffected by a nil classifier, got %v", got)
	}
}

// TestUnresolvedDimension_EmptyBookIsNotRefused: no holdings means no holding's
// sector is in question. A portfolio that holds nothing must not be refused for
// want of reference data about nothing.
func TestUnresolvedDimension_EmptyBookIsNotRefused(t *testing.T) {
	empty := &Book{PortfolioID: "p1", BaseCurrency: "USD", NAV: money(0, 0, "USD")}
	restriction := &compliancepb.Rule{
		RuleId: "no-tobacco", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
		Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			Values:    []string{"TOBACCO"},
			Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
		}},
	}
	if got := evalStatus(t, empty, nil, restriction).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("an empty book must PASS a sector restriction, got %v", got)
	}
}

// --- through the gate, which is where an order is actually refused -----------

// TestPreTradeGate_SectorMandateWithNoClassifierRefusesTheOrder proves the rule
// change reaches the DECISION. A violation the gate does not act on is not a
// control, and PreTradeGate admits on anything short of BREACH — so the rule and
// the gate have to be checked separately.
//
// The gate here is built exactly as services/oms constructs it: classifier nil.
func TestPreTradeGate_SectorMandateWithNoClassifierRefusesTheOrder(t *testing.T) {
	m := mandate(techCapMandate())
	m.EffectiveAt = timestamppb.New(t0)
	reg := NewMandateRegistry()
	mustPut(t, reg, m)
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, nil, nil)

	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(10, 0), Price: dec(1000, 0), Currency: "USD",
		OrderID: "o1", AsOf: t0,
	})
	// A nil error is NOT the result here: the gate returns (Decision, nil) for a
	// refusal exactly as it does for an admission. The state is the Decision.
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got.Allowed {
		t.Fatalf("the gate ADMITTED an order under a 10%% sector cap it cannot evaluate — "+
			"result=%v", got.Result)
	}
	if got.Result.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("status=%v want BREACH", got.Result.GetStatus())
	}
}

// TestPreTradeGate_InstrumentMandateStillAdmits is the gate-level false-refusal
// arm: the gate with a nil classifier must keep admitting compliant orders under
// the dimensions it CAN evaluate, which is every mandate deployed today.
func TestPreTradeGate_InstrumentMandateStillAdmits(t *testing.T) {
	g := newGate(t, concentrationMandate(60))
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(10, 0), Price: dec(1000, 0), Currency: "USD",
		OrderID: "o2", AsOf: t0,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !got.Allowed {
		t.Fatalf("an INSTRUMENT-dimension mandate needs no classifier and must still admit; "+
			"result=%v", got.Result)
	}
}
