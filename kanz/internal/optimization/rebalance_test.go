package optimization

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"

	"github.com/eighred/kanz/internal/compliance"
)

func TestRebalance_MinimalTradeList(t *testing.T) {
	current := map[string]float64{"A": 0.5, "B": 0.5, "C": 0.0}
	target := map[string]float64{"A": 0.5001, "B": 0.3, "C": 0.2} // A barely moves
	prices := map[string]float64{"A": 10, "B": 20, "C": 5}
	p := Rebalance("PF", current, target, 100000, prices, 0.005, time.Now())

	// A's move (1bp) is below the 50bp threshold ⇒ no trade. Only B (sell) and C
	// (buy) are traded — minimality.
	if len(p.Trades) != 2 {
		t.Fatalf("want 2 trades (B,C), got %d: %+v", len(p.Trades), p.Trades)
	}
	byID := map[string]ProposedTrade{}
	for _, tr := range p.Trades {
		byID[tr.InstrumentID] = tr
	}
	if _, traded := byID["A"]; traded {
		t.Fatal("A moved below threshold and must not be traded")
	}
	if byID["B"].Side != Sell {
		t.Fatalf("B should be a SELL, got %v", byID["B"].Side)
	}
	if byID["C"].Side != Buy {
		t.Fatalf("C should be a BUY, got %v", byID["C"].Side)
	}
	// C: Δw=0.2 ⇒ notional 20000 / price 5 = 4000 units.
	approx(t, "C quantity", byID["C"].Quantity, 4000, 1e-6)
	// One-way turnover = (|−0.2| + |0.2|)/2 = 0.2.
	approx(t, "turnover", p.Turnover, 0.2, 1e-9)
}

func TestPropose_MandateInfeasibleFlagged(t *testing.T) {
	// Optimize all-in to the highest return, but a mandate caps any instrument at
	// 50% ⇒ the result (100% B) breaches the post-check, flagged not dropped.
	in := MarketInputs{Instruments: []string{"A", "B"}, ExpectedReturns: []float64{0.05, 0.10}}
	mandate := &compliancepb.Mandate{
		PortfolioId: "PF",
		Rules: []*compliancepb.Rule{{
			RuleId: "conc-1",
			Type:   compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				MaxWeight: &commonpb.Decimal{Coefficient: 50, Exponent: -2}, // 50%
			}},
			OnViolation: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
		}},
	}
	p, err := Propose(context.Background(), "PF", in, Objective{Type: MaxReturn}, nil,
		map[string]float64{"A": 0.5, "B": 0.5}, 100000, map[string]float64{"A": 10, "B": 10}, 0.005,
		nil, compliance.NewEngine(nil), mandate, "USD", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.MandateFeasible {
		t.Fatal("100% concentration must breach the 50% mandate cap")
	}
	if len(p.Violations) == 0 {
		t.Fatal("expected violation messages on an infeasible proposal")
	}
}

func TestPropose_MandateFeasiblePasses(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: diag(0.04, 0.04)}
	// Min-variance ⇒ 50/50, within a 60% cap.
	mandate := &compliancepb.Mandate{
		PortfolioId: "PF",
		Rules: []*compliancepb.Rule{{
			RuleId: "conc-1",
			Type:   compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				MaxWeight: &commonpb.Decimal{Coefficient: 60, Exponent: -2},
			}},
			OnViolation: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
		}},
	}
	p, err := Propose(context.Background(), "PF", in, Objective{Type: MinVariance}, nil,
		map[string]float64{"A": 1.0}, 100000, map[string]float64{"A": 10, "B": 10}, 0.005,
		nil, compliance.NewEngine(nil), mandate, "USD", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !p.MandateFeasible {
		t.Fatalf("50/50 within a 60%% cap must be feasible: %v", p.Violations)
	}
}

func TestMandateConstraints_DerivesBoxFromRules(t *testing.T) {
	mandate := &compliancepb.Mandate{
		Rules: []*compliancepb.Rule{
			{
				Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
				Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
					Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
					Bucket:    "AAPL",
					MaxWeight: &commonpb.Decimal{Coefficient: 25, Exponent: -2},
				}},
			},
			{
				Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
				Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
					Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
					Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
					Values:    []string{"TSLA"},
				}},
			},
		},
	}
	cs := MandateConstraints(mandate, true)
	approx(t, "AAPL cap", cs.Bounds["AAPL"].Max, 0.25, 1e-9)
	if b := cs.Bounds["TSLA"]; b.Max != 0 {
		t.Fatalf("denied instrument must have max weight 0, got %.4f", b.Max)
	}
}
