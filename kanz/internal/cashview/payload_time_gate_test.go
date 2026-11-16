package cashview_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/cashview"
	comp "github.com/eighred/kanz/internal/compliance"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type cashTimeBooks struct{ view *cashview.View }

func (b cashTimeBooks) Book(_ context.Context, portfolio string) (*comp.Book, error) {
	book := &comp.Book{PortfolioID: portfolio, BaseCurrency: "USD", NAVBasis: comp.NAVBasisEquity,
		NAV: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1_000_000}}}
	comp.JoinEquity(book, b.view, nil)
	return book, nil
}

type cashTimeDecisions struct{ records []comp.DecisionRecord }

func (d *cashTimeDecisions) Record(_ context.Context, record comp.DecisionRecord) error {
	d.records = append(d.records, record)
	return nil
}

func cashTimeGate(t *testing.T, view *cashview.View, portfolio string) (*comp.PreTradeGate, *cashTimeDecisions) {
	t.Helper()
	registry := comp.NewMandateRegistry()
	if err := registry.Put(&compliancepb.Mandate{MandateId: "cash-time", TenantId: "T", PortfolioId: portfolio, Version: 7,
		Rules: []*compliancepb.Rule{{RuleId: "cash-floor", Type: compliancepb.RuleType_RULE_TYPE_BUYING_POWER,
			Params: &compliancepb.Rule_BuyingPower{BuyingPower: &compliancepb.BuyingPowerLimit{}}}}}); err != nil {
		t.Fatal(err)
	}
	recorder := &cashTimeDecisions{}
	return comp.NewPreTradeGate(nil, cashTimeBooks{view}, registry, nil, recorder, nil), recorder
}

func assertCashTimeDecision(t *testing.T, gate *comp.PreTradeGate, recorder *cashTimeDecisions, portfolio, state string) {
	t.Helper()
	decision, err := gate.Evaluate(t.Context(), comp.OrderDelta{TenantID: "T", PortfolioID: portfolio, InstrumentID: "CASH-TIME",
		SignedQuantity: &commonpb.Decimal{Coefficient: 3}, Price: &commonpb.Decimal{Coefficient: 100}, Currency: "USD",
		OrderID: "cash-time-order", Issuer: "cash-time-test", AsOf: cashTimeBase})
	wantAllowed := state == "funded"
	if err != nil || decision.Allowed != wantAllowed {
		t.Fatalf("shared gate allowed=%v want=%v result=%v err=%v", decision.Allowed, wantAllowed, decision.Result, err)
	}
	if !wantAllowed {
		violations := decision.Result.GetViolations()
		if len(violations) != 1 {
			t.Fatalf("expected exactly the buying-power refusal: %v", decision.Result)
		}
		evidence := violations[0].GetEvidence()
		if state == "unknown" {
			if evidence["cash"] != "unavailable" {
				t.Fatalf("UNKNOWN became a measured balance: %v", evidence)
			}
		} else {
			// A measured zero less BUY 3 x USD100 is USD-300, not UNKNOWN.
			after, ok := new(big.Rat).SetString(evidence["cash_after"])
			if !ok || after.Cmp(big.NewRat(-300, 1)) != 0 || evidence[comp.EvidenceBalanceCompleteness] != "complete" {
				t.Fatalf("wrong exact empty-account refusal: %v", evidence)
			}
		}
	}
	if len(recorder.records) == 0 {
		t.Fatal("shared gate did not record its decision")
	}
	last := recorder.records[len(recorder.records)-1]
	if last.Allowed != wantAllowed || last.TenantID != "T" || last.Result.GetPortfolioId() != portfolio || last.Phase != comp.PhasePreTrade ||
		last.Result.GetMandateVersion() != 7 || last.OrderID != "cash-time-order" || last.Issuer != "cash-time-test" {
		t.Fatal("shared gate lost tenant/portfolio/mandate decision attribution")
	}
}

// This reaches the actual shared gate through JoinEquity/CashSource. It does
// not stand in for OMS dispatch or a compliance Monitor registration.
func TestCashPayloadTimeCannotFabricateBuyingPower(t *testing.T) {
	for _, tc := range malformedCashTimes(cashTimeBase) {
		for _, state := range []string{"cold", "warm"} {
			t.Run(tc.name+"-"+state, func(t *testing.T) {
				view := cashview.New(cashview.WithClock(func() time.Time { return cashTimeBase }))
				wantState := "unknown"
				if state == "warm" {
					foldCashTime(t, view, cashTimeReport("PF", &commonpb.Decimal{}, timestamppb.New(cashTimeBase)))
					wantState = "empty"
				}
				gate, recorder := cashTimeGate(t, view, "PF")
				assertCashTimeDecision(t, gate, recorder, "PF", wantState)
				foldCashTime(t, view, cashTimeReport("PF", &commonpb.Decimal{Coefficient: 750}, tc.at))
				assertCashTimeDecision(t, gate, recorder, "PF", wantState)
				foldCashTime(t, view, cashTimeReport("PF", &commonpb.Decimal{Coefficient: 750}, timestamppb.New(cashTimeBase)))
				assertCashTimeDecision(t, gate, recorder, "PF", "funded") // USD750 - 3 x USD100 = USD450
			})
		}
	}
}
