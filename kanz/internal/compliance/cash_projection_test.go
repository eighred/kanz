package compliance

import (
	"context"
	"math"
	"math/big"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func TestCashProjectionConservesExactSignedTrade(t *testing.T) {
	cases := []struct {
		name             string
		cash, qty, price *commonpb.Decimal
		want             *big.Rat
	}{
		{"min_coefficient_sell", dec(-1, 0), dec(math.MinInt64, 0), dec(1, 0), new(big.Rat).SetInt64(math.MaxInt64)},
		{"scaled_min_sell", dec(-1, -8), dec(math.MinInt64, -8), dec(1, 0), new(big.Rat).SetFrac(big.NewInt(math.MaxInt64), big.NewInt(100000000))},
		{"tiny_buy", dec(2, -12), dec(1, -12), dec(1, 0), big.NewRat(1, 1000000000000)},
		{"ordinary_buy", dec(1000, 0), dec(3, 0), dec(100, 0), big.NewRat(700, 1)},
		{"ordinary_sell", dec(1000, 0), dec(-4, 0), dec(100, 0), big.NewRat(1400, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := &commonpb.Money{Amount: tc.cash, CurrencyCode: "USD"}
			b := bookWithCash(original)
			projected, ok := project(b, OrderDelta{InstrumentID: "A", SignedQuantity: tc.qty, Price: tc.price, Currency: "USD"})
			if !ok {
				t.Fatal("exactly representable final cash was refused")
			}
			if got := ratFromDecimal(projected.Cash.GetAmount()); got.Cmp(tc.want) != 0 {
				t.Fatalf("cash_after=%s, independent conservation oracle=%s", got.RatString(), tc.want.RatString())
			}
			if b.Cash != original || b.Cash.Amount != tc.cash {
				t.Fatal("input cash was mutated")
			}
		})
	}
}

func TestCashProjectionRefusesUnsupportedExactResultOrUnits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cash          *commonpb.Money
		tradeCurrency string
		qty           *commonpb.Decimal
	}{
		{"unrepresentable_positive_min", money(0, 0, "USD"), "USD", dec(math.MinInt64, 0)},
		{"foreign_trade", money(100, 0, "USD"), "EUR", dec(1, 0)},
		{"unstated_trade", money(100, 0, "USD"), "", dec(1, 0)},
		{"unstated_cash", money(100, 0, ""), "USD", dec(1, 0)},
		{"zero_unstated_cash", money(0, 0, ""), "USD", dec(1, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := project(bookWithCash(tc.cash), OrderDelta{InstrumentID: "A", SignedQuantity: tc.qty, Price: dec(1, 0), Currency: tc.tradeCurrency}); ok {
				t.Fatal("cash projection fabricated a supported result")
			}
		})
	}
}

func TestBuyingPowerKeepsMissingAmountsAndUnitsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		cash *commonpb.Money
	}{
		{"missing_money", nil},
		{"missing_amount", &commonpb.Money{CurrencyCode: "USD"}},
		{"unstated_currency", money(0, 0, "")},
		{"foreign_currency", money(100, 0, "EUR")},
		{"outside_domain", money(1, 65, "USD")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v := BuyingPowerRule(&Candidate{Book: bookWithCash(tc.cash)}, buyingPowerRule(nil)); v == nil {
				t.Fatal("unreadable cash satisfied the base-currency floor")
			}
		})
	}
	if v := BuyingPowerRule(&Candidate{Book: bookWithCash(money(0, 0, "USD"))}, buyingPowerRule(nil)); v != nil {
		t.Fatalf("explicit measured zero against the documented zero floor refused: %v", v)
	}
}

func TestGateRecordsRefusalWithoutInventingCash(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cash     *commonpb.Money
		currency string
	}{
		{"missing_amount", &commonpb.Money{CurrencyCode: "USD"}, "USD"},
		{"foreign_cash", money(100, 0, "EUR"), "EUR"},
		{"foreign_trade", money(100, 0, "USD"), "EUR"},
		{"outside_domain", money(1, 65, "USD"), "USD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := bookWithCash(tc.cash)
			b.PortfolioID = "p1"
			reg := NewMandateRegistry()
			mustPut(t, reg, mandate(buyingPowerRule(nil)))
			rec := &recordingRecorder{}
			gate := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": b}, reg, nil, rec, nil)
			decision, err := gate.Evaluate(context.Background(), OrderDelta{
				TenantID: "t1", PortfolioID: "p1", InstrumentID: "A", SignedQuantity: dec(-1, 0), Price: dec(1, 0),
				Currency: tc.currency, OrderID: "cash-truth", AsOf: t0,
			})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Allowed || len(rec.records) != 1 || rec.records[0].Allowed {
				t.Fatalf("unusable cash must refuse and record once: %+v records=%+v", decision, rec.records)
			}
		})
	}
}

func TestCashProjectionKeepsAbsentAmountUnknown(t *testing.T) {
	b := bookWithCash(&commonpb.Money{CurrencyCode: "USD"})
	result, ok := project(b, OrderDelta{InstrumentID: "A", SignedQuantity: dec(-1, 0), Price: dec(1, 0), Currency: "USD"})
	if !ok || result.Cash == nil || result.Cash.GetAmount() != nil {
		t.Fatalf("missing amount must remain unknown for the rule that needs it: ok=%v result=%+v", ok, result)
	}
}

func TestCashProjectionUsesBoundedExactEncoding(t *testing.T) {
	for _, exponent := range []int32{-60, 60} {
		cash := dec(1, exponent)
		result, ok := project(bookWithCash(&commonpb.Money{Amount: cash, CurrencyCode: "USD"}), OrderDelta{
			InstrumentID: "A", SignedQuantity: dec(0, 0), Price: dec(1, 0), Currency: "USD",
		})
		if !ok || ratFromDecimal(result.Cash.Amount).Cmp(ratFromDecimal(cash)) != 0 {
			t.Fatalf("supported exponent %d changed or refused", exponent)
		}
	}
	for _, exponent := range []int32{-61, 61, -64, 64} {
		if _, ok := project(bookWithCash(money(1, exponent, "USD")), OrderDelta{InstrumentID: "A", SignedQuantity: dec(0, 0), Price: dec(1, 0), Currency: "USD"}); ok {
			t.Fatalf("existing exact encoder does not support normalized exponent %d; admission must refuse rather than round", exponent)
		}
	}
	if v := BuyingPowerRule(&Candidate{Book: bookWithCash(money(1, 0, "USD"))}, buyingPowerRule(dec(1, 65))); v == nil {
		t.Fatal("out-of-domain cash floor was computed as a policy value")
	}
}
