package compliance

import (
	"context"
	"testing"

	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func concentrationMandate(maxPct int64) *compliancepb.Mandate {
	m := mandate(&compliancepb.Rule{
		RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
			MaxWeight: dec(maxPct, -2),
		}},
	})
	m.EffectiveAt = timestamppb.New(t0)
	return m
}

// currentBook: AAPL 100 @ 1000 = 100k, MSFT 100k. AAPL is 50% of gross.
func currentBook() *Book {
	return &Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: money(200000, 0, "USD"),
		Positions: []Position{
			{InstrumentID: "AAPL", Quantity: dec(100, 0), MarketValue: money(100000, 0, "USD")},
			{InstrumentID: "MSFT", Quantity: dec(100, 0), MarketValue: money(100000, 0, "USD")},
		},
	}
}

func newGate(t *testing.T, m *compliancepb.Mandate) *PreTradeGate {
	t.Helper()
	reg := NewMandateRegistry()
	if m != nil {
		reg.Put(m)
	}
	return NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, nil, nil)
}

func TestPreTradeGate_RejectsBreachingOrder(t *testing.T) {
	g := newGate(t, concentrationMandate(60)) // 60% cap
	// Buy 100 more AAPL @ 1000 ⇒ AAPL 200k of 300k = 66.7% > 60%.
	dec1 := OrderDelta{
		PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(100, 0), Price: dec(1000, 0), Currency: "USD",
		OrderID: "o1", AsOf: t0,
	}
	got, err := g.Evaluate(context.Background(), dec1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatalf("order pushing AAPL to 66.7%% over a 60%% cap must be rejected")
	}
	if got.Result.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("want BREACH, got %v", got.Result.GetStatus())
	}
}

func TestPreTradeGate_AdmitsCompliantOrder(t *testing.T) {
	g := newGate(t, concentrationMandate(60))
	// Buy 10 AAPL ⇒ AAPL 110k of 210k ≈ 52% < 60%.
	got, err := g.Evaluate(context.Background(), OrderDelta{
		PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(10, 0), Price: dec(1000, 0), Currency: "USD", OrderID: "o2", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatalf("compliant order must be admitted; result=%v", got.Result.GetStatus())
	}
}

func TestPreTradeGate_NoMandateAdmits(t *testing.T) {
	g := newGate(t, nil) // no mandate for p1
	got, err := g.Evaluate(context.Background(), OrderDelta{
		PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(1000, 0), Price: dec(1000, 0), Currency: "USD", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatalf("order on an unmandated portfolio must be admitted")
	}
}

// recordingRecorder captures decision records for assertions.
type recordingRecorder struct{ records []DecisionRecord }

func (r *recordingRecorder) Record(_ context.Context, rec DecisionRecord) error {
	r.records = append(r.records, rec)
	return nil
}

func TestPreTradeGate_RecordsDecision(t *testing.T) {
	reg := NewMandateRegistry()
	reg.Put(concentrationMandate(60))
	rec := &recordingRecorder{}
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, rec, nil)
	if _, err := g.Evaluate(context.Background(), OrderDelta{
		PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(100, 0), Price: dec(1000, 0), Currency: "USD", OrderID: "o1", AsOf: t0,
	}); err != nil {
		t.Fatal(err)
	}
	if len(rec.records) != 1 || rec.records[0].Phase != PhasePreTrade || rec.records[0].Allowed {
		t.Fatalf("expected one pre-trade reject record, got %+v", rec.records)
	}
}
