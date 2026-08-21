package optimization

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"

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
	if p.MandateStatus != MandateInfeasible {
		t.Fatalf("100%% concentration must breach the 50%% mandate cap, got %s", p.MandateStatus)
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
	if p.MandateStatus != MandateFeasible {
		t.Fatalf("50/50 within a 60%% cap must be feasible, got %s: %v", p.MandateStatus, p.Violations)
	}
}

// A PROPOSAL NOBODY CHECKED MUST NOT REPORT ITSELF FEASIBLE (#646).
//
// Rebalance is handed no mandate, no engine and no classifier. It used to return
// MandateFeasible: true regardless, which is the value bridge.ToOrders reads to
// decide whether a rebalance becomes live SubmitOrder commands — so every
// proposal the optimization service ever built arrived at that gate certified by
// a check that had not run.
func TestRebalanceCannotCertifyAProposalItDidNotCheck(t *testing.T) {
	p := Rebalance("PF", map[string]float64{"A": 1.0}, map[string]float64{"A": 0.5, "B": 0.5},
		100000, map[string]float64{"A": 10, "B": 10}, 0.005, time.Now())
	if len(p.Trades) == 0 {
		t.Fatal("non-vacuity: this proposal must carry trades, or the assertion below is about an empty proposal")
	}
	if p.MandateStatus != MandateUnchecked {
		t.Fatalf("Rebalance consulted no mandate and must report UNCHECKED, got %s", p.MandateStatus)
	}
	if p.MandateStatus == MandateFeasible {
		t.Fatal("a constructor that was given no mandate certified the proposal as feasible")
	}
}

// A NIL MANDATE IS NOT A PASS (#646). CheckMandate used to return feasible=true
// for one, so "this deployment holds no mandate for the portfolio" and "every
// rule was satisfied" were the same answer at the gate that emits orders.
func TestProposeWithNoMandateIsUncheckedNotFeasible(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: diag(0.04, 0.04)}
	p, err := Propose(context.Background(), "PF", in, Objective{Type: MinVariance}, nil,
		map[string]float64{"A": 1.0}, 100000, map[string]float64{"A": 10, "B": 10}, 0.005,
		nil, compliance.NewEngine(nil), nil, "USD", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.MandateStatus != MandateUnchecked {
		t.Fatalf("a nil mandate means nothing was checked; got %s", p.MandateStatus)
	}
}

// The verdict crosses the wire as a NAME. As an integer the unchecked state
// would be 0, which every JSON client reads as absent, false and fine — the same
// collapse the bool had.
func TestMandateStatusJSONRoundTripsByName(t *testing.T) {
	for _, want := range []MandateStatus{MandateUnchecked, MandateFeasible, MandateInfeasible} {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %s: %v", want, err)
		}
		if string(b) != `"`+want.String()+`"` {
			t.Fatalf("%s marshalled as %s, want the name in quotes", want, b)
		}
		var got MandateStatus
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got != want {
			t.Fatalf("round trip: got %s want %s", got, want)
		}
	}
}

// A BOOLEAN VERDICT IS REFUSED, NOT REINTERPRETED. `true` was the pre-#646
// spelling of feasible; decoding it into MandateFeasible would carry the exact
// forgery the rename closes straight back in.
func TestMandateStatusRefusesABooleanVerdict(t *testing.T) {
	var got MandateStatus
	err := json.Unmarshal([]byte("true"), &got)
	if err == nil {
		t.Fatalf("the boolean true was accepted as a verdict and decoded to %s", got)
	}
	if !strings.Contains(err.Error(), "UNCHECKED") {
		t.Errorf("the refusal does not name the legal verdicts: %v", err)
	}
	if got != MandateUnchecked {
		t.Errorf("a refused verdict must leave the value unchecked, got %s", got)
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
