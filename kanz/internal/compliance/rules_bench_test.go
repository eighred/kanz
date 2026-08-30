package compliance

import (
	"context"
	"strconv"
	"testing"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// A realistic admission: an N-position single-currency book under a five-rule
// mandate, evaluated the way the pre-trade gate evaluates it (one Candidate,
// one Evaluate, rules in mandate order).
//
// The dimensions are deliberately CLASSIFIED ones (SECTOR, ISSUER): those are
// the dimensions unresolvedDimension actually walks the book for, so this is
// the shape that pays the full cost rather than the cheapest one.
func benchBook(n int) (*Book, Classifier) {
	sectors := []string{"TECH", "FIN", "ENERGY", "HEALTH", "UTIL"}
	positions := make([]Position, 0, n)
	cl := StaticClassifier{}
	var gross int64
	for i := 0; i < n; i++ {
		id := "INST" + strconv.Itoa(i)
		positions = append(positions, pos(id, int64(100000+i), 0, "USD"))
		cl[id] = Attributes{
			Issuer:     "ISS" + strconv.Itoa(i%37),
			Sector:     sectors[i%len(sectors)],
			AssetClass: "EQUITY",
		}
		gross += int64(100000 + i)
	}
	return &Book{
		PortfolioID:  "p1",
		BaseCurrency: "USD",
		NAV:          money(gross*2, 0, "USD"), // equity ⇒ leverage 0.5, passes
		NAVBasis:     NAVBasisEquity,
		Positions:    positions,
	}, cl
}

// benchMandate is the five-rule mandate the issue describes, every rule of
// which passes — the admission path, not the refusal path.
func benchMandate() *compliancepb.Mandate {
	return mandate(
		&compliancepb.Rule{
			RuleId: "conc", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
				MaxWeight: dec(99, -2),
			}},
		},
		&compliancepb.Rule{
			RuleId: "restr", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
			Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
				Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
				Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
				Values:    []string{"TOBACCO"},
			}},
		},
		&compliancepb.Rule{
			RuleId: "issuer", Type: compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION,
			Params: &compliancepb.Rule_IssuerExclusion{IssuerExclusion: &compliancepb.IssuerExclusion{
				IssuerIds: []string{"ISS-BANNED"},
			}},
		},
		&compliancepb.Rule{
			RuleId: "lev", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
			Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
				MaxGrossLeverage: dec(15, -1),
			}},
		},
		&compliancepb.Rule{
			RuleId: "ccy", Type: compliancepb.RuleType_RULE_TYPE_CURRENCY,
			Params: &compliancepb.Rule_CurrencyRestriction{CurrencyRestriction: &compliancepb.CurrencyRestriction{
				AllowedCurrencies: []string{"USD"},
			}},
		},
	)
}

func benchEvaluate(b *testing.B, n int) {
	book, cl := benchBook(n)
	m := benchMandate()
	e := NewEngine(nil)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := e.Evaluate(ctx, &Candidate{Book: book, Classifier: cl, AsOf: t0}, m)
		if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
			b.Fatalf("mandate should pass: %v", res.GetViolations())
		}
	}
}

func BenchmarkEvaluate_FiveRuleMandate_50(b *testing.B)   { benchEvaluate(b, 50) }
func BenchmarkEvaluate_FiveRuleMandate_200(b *testing.B)  { benchEvaluate(b, 200) }
func BenchmarkEvaluate_FiveRuleMandate_1000(b *testing.B) { benchEvaluate(b, 1000) }
