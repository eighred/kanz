package compliance

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var t0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func dec(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func money(coeff int64, exp int32, ccy string) *commonpb.Money {
	return &commonpb.Money{Amount: dec(coeff, exp), CurrencyCode: ccy}
}

// pos builds a position with unit quantity and the given market value.
func pos(inst string, mvCoeff int64, mvExp int32, ccy string) Position {
	return Position{InstrumentID: inst, Quantity: dec(1, 0), MarketValue: money(mvCoeff, mvExp, ccy)}
}

func mandate(rules ...*compliancepb.Rule) *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		Rules: rules, EffectiveAt: timestamppb.New(t0),
	}
}

func evalStatus(t *testing.T, b *Book, cl Classifier, rules ...*compliancepb.Rule) *compliancepb.ComplianceResult {
	t.Helper()
	e := NewEngine(nil)
	return e.Evaluate(context.Background(), &Candidate{Book: b, Classifier: cl, AsOf: t0}, mandate(rules...))
}

// Two equal $100k USD holdings ⇒ each is 50% of gross.
func twoEqualBook() *Book {
	return &Book{
		PortfolioID:  "p1",
		BaseCurrency: "USD",
		NAV:          money(200000, 0, "USD"),
		NAVBasis:     NAVBasisEquity,
		Positions:    []Position{pos("AAPL", 100000, 0, "USD"), pos("MSFT", 100000, 0, "USD")},
	}
}

func TestConcentration_AllowAndBreach(t *testing.T) {
	rule := &compliancepb.Rule{
		RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
			MaxWeight: dec(60, -2), // 60%
		}},
	}
	if got := evalStatus(t, twoEqualBook(), nil, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("50%% under 60%% cap: want PASS, got %v", got)
	}

	rule.GetConcentration().MaxWeight = dec(40, -2) // 40% — 50% now breaches
	res := evalStatus(t, twoEqualBook(), nil, rule)
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("50%% over 40%% cap: want BREACH, got %v", res.GetStatus())
	}
	if len(res.GetViolations()) != 1 || res.GetViolations()[0].GetEvidence()["observed"] != "0.500000" {
		t.Fatalf("unexpected violation/evidence: %+v", res.GetViolations())
	}
}

func TestConcentration_BucketScoped_Sector(t *testing.T) {
	cl := StaticClassifier{
		"AAPL": {Sector: "TECH"}, "MSFT": {Sector: "TECH"}, "XOM": {Sector: "ENERGY"},
	}
	b := &Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: money(300000, 0, "USD"),
		NAVBasis:  NAVBasisEquity,
		Positions: []Position{pos("AAPL", 100000, 0, "USD"), pos("MSFT", 100000, 0, "USD"), pos("XOM", 100000, 0, "USD")},
	}
	rule := &compliancepb.Rule{
		RuleId: "c2", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			Bucket:    "TECH",
			MaxWeight: dec(50, -2), // TECH is 2/3 ≈ 66.7% > 50%
		}},
	}
	if got := evalStatus(t, b, cl, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("TECH 66.7%% over 50%% cap: want BREACH, got %v", got)
	}
}

func TestRestriction_DenyAndAllowOnly(t *testing.T) {
	cl := StaticClassifier{"AAPL": {Sector: "TECH"}, "XOM": {Sector: "ENERGY"}}
	b := &Book{PortfolioID: "p1", BaseCurrency: "USD", Positions: []Position{pos("AAPL", 100000, 0, "USD")}}

	deny := &compliancepb.Rule{
		RuleId: "r1", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
		Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			Values:    []string{"TECH"}, Mode: compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
		}},
	}
	if got := evalStatus(t, b, cl, deny).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("deny TECH while holding AAPL: want BREACH, got %v", got)
	}

	allowOnly := &compliancepb.Rule{
		RuleId: "r2", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
		Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			Values:    []string{"ENERGY"}, Mode: compliancepb.RestrictionMode_RESTRICTION_MODE_ALLOW_ONLY,
		}},
	}
	if got := evalStatus(t, b, cl, allowOnly).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("allow-only ENERGY while holding TECH: want BREACH, got %v", got)
	}
}

func TestIssuerExclusion(t *testing.T) {
	cl := StaticClassifier{"AAPL": {Issuer: "APPLE_INC"}, "XOM": {Issuer: "EXXON"}}
	b := &Book{PortfolioID: "p1", BaseCurrency: "USD", Positions: []Position{pos("XOM", 50000, 0, "USD")}}
	rule := &compliancepb.Rule{
		RuleId: "e1", Type: compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION,
		Params: &compliancepb.Rule_IssuerExclusion{IssuerExclusion: &compliancepb.IssuerExclusion{
			IssuerIds: []string{"EXXON"},
		}},
	}
	if got := evalStatus(t, b, cl, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("holding excluded issuer EXXON: want BREACH, got %v", got)
	}
	// Holding a non-excluded issuer passes.
	b.Positions = []Position{pos("AAPL", 50000, 0, "USD")}
	if got := evalStatus(t, b, cl, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("holding non-excluded issuer: want PASS, got %v", got)
	}
}

func TestLeverage_AllowBreachAndUnverifiable(t *testing.T) {
	rule := &compliancepb.Rule{
		RuleId: "l1", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
		Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
			MaxGrossLeverage: dec(15, -1), // 1.5x
		}},
	}
	// gross 200k, NAV 200k ⇒ 1.0x ≤ 1.5x: PASS.
	if got := evalStatus(t, twoEqualBook(), nil, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("1.0x under 1.5x: want PASS, got %v", got)
	}
	// NAV 100k ⇒ 2.0x > 1.5x: BREACH.
	b := twoEqualBook()
	b.NAV = money(100000, 0, "USD")
	if got := evalStatus(t, b, nil, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("2.0x over 1.5x: want BREACH, got %v", got)
	}
	// Missing NAV fails closed.
	b.NAV = nil
	if got := evalStatus(t, b, nil, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("unverifiable NAV: want BREACH (deny-by-default), got %v", got)
	}
}

func TestCurrencyRestriction(t *testing.T) {
	b := &Book{PortfolioID: "p1", BaseCurrency: "USD", Positions: []Position{
		pos("AAPL", 100000, 0, "USD"), pos("SAP", 50000, 0, "EUR"),
	}}
	rule := &compliancepb.Rule{
		RuleId: "cc1", Type: compliancepb.RuleType_RULE_TYPE_CURRENCY,
		Params: &compliancepb.Rule_CurrencyRestriction{CurrencyRestriction: &compliancepb.CurrencyRestriction{
			AllowedCurrencies: []string{"USD"},
		}},
	}
	if got := evalStatus(t, b, nil, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("EUR holding with USD-only mandate: want BREACH, got %v", got)
	}
}

func TestWarnSeverityDoesNotBreach(t *testing.T) {
	rule := &compliancepb.Rule{
		RuleId: "w1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		OnViolation: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT, MaxWeight: dec(40, -2),
		}},
	}
	if got := evalStatus(t, twoEqualBook(), nil, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN {
		t.Fatalf("WARN rule over limit: want WARN, got %v", got)
	}
}

func TestDenyByDefault_UnknownRule(t *testing.T) {
	// A rule with an unspecified type is unknown to the registry ⇒ BREACH.
	rule := &compliancepb.Rule{RuleId: "u1", Type: compliancepb.RuleType_RULE_TYPE_UNSPECIFIED}
	res := evalStatus(t, twoEqualBook(), nil, rule)
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("unknown rule: want BREACH, got %v", res.GetStatus())
	}
	if res.GetViolations()[0].GetSeverity() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("unknown rule violation must be BREACH-severity")
	}
}

func TestDenyByDefault_ParamsMismatch(t *testing.T) {
	// Type says CONCENTRATION but no params set ⇒ params mismatch ⇒ deny.
	rule := &compliancepb.Rule{RuleId: "m1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION}
	if got := evalStatus(t, twoEqualBook(), nil, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("params mismatch: want BREACH, got %v", got)
	}
}

func TestEmptyMandatePasses(t *testing.T) {
	if got := evalStatus(t, twoEqualBook(), nil).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("empty ruleset: want PASS, got %v", got)
	}
}
