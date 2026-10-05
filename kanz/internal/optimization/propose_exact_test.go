package optimization

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

func exactFixture() (MarketInputs, FinancialInputs) {
	return MarketInputs{Instruments: []string{"A"}, ExpectedReturns: []float64{0.1}}, FinancialInputs{Current: map[string]dec.Exact{}, NAV: "9007199254740993", Prices: map[string]dec.Exact{"A": "0.000000000001"}, Threshold: "0", Currency: "USD"}
}

func TestProposalPreservesFinancialFactsBeyondFloatAndEightPlaces(t *testing.T) {
	in, f := exactFixture()
	p, err := ProposeExact(context.Background(), "PF", in, Objective{Type: MaxReturn}, nil, f, nil, nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	trade := p.Trades[0]
	if trade.Notional != "9007199254740993" || trade.Quantity != "9007199254740993000000000000" || p.MandateStatus != MandateUnchecked || !p.ReadOnly || p.Currency != "USD" {
		t.Fatalf("lost exact inputs: %+v", p)
	}
}

func TestExactMandateBookNeverRoundsToCents(t *testing.T) {
	for _, nav := range []dec.Exact{"9007199254740993", "0.000000000001"} {
		reg := compliance.NewRegistry()
		called := false
		reg.Register(compliancepb.RuleType_RULE_TYPE_CONCENTRATION, func(c *compliance.Candidate, _ *compliancepb.Rule) *compliancepb.Violation {
			called = true
			want, _ := nav.Rat()
			if dec.FromProto(c.Book.NAV.Amount).Cmp(want) != 0 || dec.FromProto(c.Book.Positions[0].MarketValue.Amount).Cmp(want) != 0 {
				t.Fatalf("candidate changed: %+v", c.Book)
			}
			return nil
		})
		m := &compliancepb.Mandate{PortfolioId: "PF", Rules: []*compliancepb.Rule{{Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION}}}
		status, _, err := CheckMandateExact(context.Background(), map[string]dec.Exact{"A": "1"}, nav, "USD", nil, compliance.NewEngine(reg), m, time.Now())
		if err != nil || !called || status != MandateFeasible {
			t.Fatalf("%s: %v %v %v", nav, status, called, err)
		}
		_, _, err = CheckMandateExact(context.Background(), map[string]dec.Exact{"A": "1/3"}, nav, "USD", nil, nil, m, time.Now())
		if err == nil && nav == "0.000000000001" {
			t.Fatal("non-decimal book was rounded")
		}
	}
}

func TestMandateChecksBookAfterThresholdOmissions(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: [][]float64{{1, 0}, {0, 1}}}
	f := FinancialInputs{Current: map[string]dec.Exact{"A": "1"}, NAV: "100", Prices: map[string]dec.Exact{"A": "1", "B": "1"}, Threshold: "0.6", Currency: "USD"}
	m := &compliancepb.Mandate{PortfolioId: "PF", Rules: []*compliancepb.Rule{{Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION, Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT, MaxWeight: &commonpb.Decimal{Coefficient: 6, Exponent: -1}}}}}}
	p, err := ProposeExact(context.Background(), "PF", in, Objective{Type: MinVariance}, nil, f, nil, nil, m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Trades) != 0 || p.Targets["A"] != "0.5" || p.EvaluatedWeights["A"] != "1" || p.MandateStatus != MandateInfeasible {
		t.Fatalf("checked ideal target instead of proposed book: %+v", p)
	}
}

func TestProposalRejectsUnknownUnboundedAndNonfiniteInputs(t *testing.T) {
	for _, mutate := range []func(*MarketInputs, *FinancialInputs){
		func(_ *MarketInputs, f *FinancialInputs) { f.NAV = "" },
		func(_ *MarketInputs, f *FinancialInputs) { f.Current = nil },
		func(_ *MarketInputs, f *FinancialInputs) { f.Prices = nil },
		func(_ *MarketInputs, f *FinancialInputs) { f.Threshold = "" },
		func(_ *MarketInputs, f *FinancialInputs) { f.Currency = "" },
		func(in *MarketInputs, _ *FinancialInputs) { in.ExpectedReturns = []float64{math.Inf(1)} },
		func(in *MarketInputs, _ *FinancialInputs) { in.Instruments = make([]string, MaxProposalInstruments+1) },
		func(in *MarketInputs, _ *FinancialInputs) { in.Covariance = [][]float64{{1, 2}, {2, 3}} },
		func(_ *MarketInputs, f *FinancialInputs) { f.Current = map[string]dec.Exact{"unknown": "1"} },
	} {
		in, f := exactFixture()
		mutate(&in, &f)
		if _, err := ProposeExact(context.Background(), "PF", in, Objective{}, nil, f, nil, nil, nil, time.Now()); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	in, f := exactFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProposeExact(ctx, "PF", in, Objective{}, nil, f, nil, nil, nil, time.Now()); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestExactFinancialConstraintsCannotRoundIntoCompliance(t *testing.T) {
	in, f := exactFixture()
	for _, c := range []*ExactConstraintSet{
		{LongOnly: true, MaxWeight: "0.999999999999999999"},
		{LongOnly: true, MaxTurnover: "0.499999999999999999"},
		{LongOnly: true, SectorCaps: map[string]dec.Exact{"technology": "1"}},
		{LongOnly: true, Bounds: map[string]ExactWeightBound{"missing": {Min: "0", Max: "1"}}},
	} {
		if _, err := ProposeExact(context.Background(), "PF", in, Objective{}, c, f, nil, nil, nil, time.Now()); err == nil {
			t.Fatalf("unchecked constraint: %+v", c)
		}
	}
}

func TestThresholdCannotInventFinancing(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B", "C"}, ExpectedReturns: []float64{0, 0.1, 0}}
	f := FinancialInputs{Current: map[string]dec.Exact{"A": "0.004", "B": "0.396", "C": "0.6"}, NAV: "100", Prices: map[string]dec.Exact{"A": "1", "B": "1", "C": "1"}, Threshold: "0.005", Currency: "USD"}
	if _, err := ProposeExact(context.Background(), "PF", in, Objective{}, nil, f, nil, nil, nil, time.Now()); err == nil {
		t.Fatal("omitted sale silently financed excess purchases")
	}
}
