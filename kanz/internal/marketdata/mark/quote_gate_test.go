package mark_test

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/cashview"
	comp "github.com/eighred/kanz/internal/compliance"
	decutil "github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/mark"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type quoteBooks struct {
	marks *mark.Source
	cash  *cashview.View
}

func (s quoteBooks) Book(context.Context, string) (*comp.Book, error) {
	b := &comp.Book{PortfolioID: "PF", BaseCurrency: "USD", NAVBasis: comp.NAVBasisGrossPositions,
		NAV:       &commonpb.Money{Amount: dec(90_000, 0), CurrencyCode: "USD"},
		Positions: []comp.Position{{InstrumentID: "QUOTE", Quantity: dec(1_000, 0), MarketValue: &commonpb.Money{Amount: dec(90_000, 0), CurrencyCode: "USD"}}}}
	comp.JoinEquity(b, s.cash, s.marks)
	return b, nil
}

type quoteDecisions struct{ records []comp.DecisionRecord }

func (r *quoteDecisions) Record(_ context.Context, record comp.DecisionRecord) error {
	r.records = append(r.records, record)
	return nil
}

// The actual shared gate and equity join consume the public Mark contract. This
// is not an OMS adapter/OrderPrice or venue-dispatch fixture.
func TestLiveQuoteCannotFabricateACompliantLeveragedBook(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bid, ask *commonpb.Decimal
	}{
		{"negative-bid", dec(-100, 0), dec(300, 0)},
		{"zero-bid", dec(0, 0), dec(200, 0)},
		{"crossed", dec(110, 0), dec(90, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marks := mark.New(func() time.Time { return base }, time.Minute)
			foldQuote(t, marks, "QUOTE", dec(79, 0), dec(81, 0), base)
			cash := cashview.New(cashview.WithClock(func() time.Time { return base }))
			payload, err := proto.Marshal(&accountingpb.PortfolioCashBalance{PortfolioId: "PF", BaseCurrency: "USD",
				Total: dec(-50_000, 0), AsOf: timestamppb.New(base), KnowledgeTime: timestamppb.New(base),
				CurrencyCoverage: &domainpb.InputCoverage{Contributed: 1},
				Completeness:     &accountingpb.BalanceCompleteness{ProducedEntryTypes: []string{"cash"}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := cash.Handle(t.Context(), nil, payload); err != nil {
				t.Fatal(err)
			}
			registry := comp.NewMandateRegistry()
			if err := registry.Put(&compliancepb.Mandate{MandateId: "quote-leverage", TenantId: "T", PortfolioId: "PF", Version: 7,
				Rules: []*compliancepb.Rule{{RuleId: "leverage", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
					Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{MaxGrossLeverage: dec(25, -1)}}}}}); err != nil {
				t.Fatal(err)
			}
			recorder := &quoteDecisions{}
			gate := comp.NewPreTradeGate(nil, quoteBooks{marks, cash}, registry, nil, recorder, nil)
			check := func(allowed bool) {
				t.Helper()
				price, ok := decutil.ToProtoExact(marks.Mark("QUOTE"))
				if !ok {
					t.Fatal("fixture unexpectedly lost its known price")
				}
				decision, err := gate.Evaluate(t.Context(), comp.OrderDelta{TenantID: "T", PortfolioID: "PF", InstrumentID: "QUOTE",
					SignedQuantity: dec(100, 0), Price: price, Currency: "USD", OrderID: "quote-contract", Issuer: "quote-contract", AsOf: base})
				if err != nil || decision.Allowed != allowed {
					t.Fatalf("gate allowed=%v want=%v result=%v err=%v", decision.Allowed, allowed, decision.Result, err)
				}
				if !allowed {
					// 1,100 x 80 gross / (1,000 x 80 - 50,000) equity = 44/15.
					violations := decision.Result.GetViolations()
					if len(violations) != 1 || violations[0].GetEvidence()["observed"] != "2.933333" || violations[0].GetEvidence()["limit"] != "2.500000" {
						t.Fatalf("wrong independently derived leverage refusal: %v", decision.Result)
					}
				}
				last := recorder.records[len(recorder.records)-1]
				if last.Allowed != allowed || last.TenantID != "T" || last.Phase != comp.PhasePreTrade || last.Result.GetMandateVersion() != 7 {
					t.Fatal("shared gate lost decision/tenant/mandate attribution")
				}
			}
			check(false)
			foldQuote(t, marks, "QUOTE", tc.bid, tc.ask, base)
			check(false) // old midpoint100 falsely admitted at 2.2x under the 2.5x cap
			foldQuote(t, marks, "QUOTE", dec(99, 0), dec(101, 0), base)
			check(true) // genuine new price100 restores equity50k, post-BUY gross110k
		})
	}
}
