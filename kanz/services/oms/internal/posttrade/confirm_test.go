package posttrade

import (
	"math/big"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

func dnum(coeff int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: coeff, Exponent: 0} }

func settleDate() time.Time { return time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) }

func fillFixture(id string) *orderpb.Fill {
	return &orderpb.Fill{
		FillId: id, OrderId: "O1", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_BUY,
		Quantity: dnum(100), Price: dnum(150),
		Fee: &commonpb.Money{Amount: dnum(5), CurrencyCode: "USD"},
	}
}

func confFixture(fillID string) Confirmation {
	return Confirmation{
		ConfirmationID: "C-" + fillID, FillID: fillID, InstrumentID: "AAPL",
		Side: orderpb.Side_SIDE_BUY, Quantity: big.NewRat(100, 1), Price: big.NewRat(150, 1),
		Counterparty: "CP1", SettlementDate: settleDate(),
	}
}

func TestMatchFillClean(t *testing.T) {
	if res := MatchFill(fillFixture("F1"), confFixture("F1"), MatchTolerance{}); !res.Matched {
		t.Fatalf("expected match, got break %+v", res.Break)
	}
}

func TestMatchFillBreaks(t *testing.T) {
	fill := fillFixture("F1")
	conf := confFixture("F1")
	conf.Quantity = big.NewRat(90, 1)  // qty break
	conf.Price = big.NewRat(151, 1)    // price break
	conf.Side = orderpb.Side_SIDE_SELL // side break
	res := MatchFill(fill, conf, MatchTolerance{})
	if res.Matched || res.Break == nil {
		t.Fatal("expected a break")
	}
	got := map[string]bool{}
	for _, f := range res.Break.Fields {
		got[f] = true
	}
	for _, want := range []string{"side", "quantity", "price"} {
		if !got[want] {
			t.Fatalf("expected %s break, got %v", want, res.Break.Fields)
		}
	}
}

func TestMatchFillTolerance(t *testing.T) {
	fill := fillFixture("F1")
	conf := confFixture("F1")
	conf.Price = big.NewRat(15001, 100) // 150.01, off by 0.01
	// Within a 0.05 price tolerance ⇒ match.
	if res := MatchFill(fill, conf, MatchTolerance{Price: big.NewRat(5, 100)}); !res.Matched {
		t.Fatalf("within tolerance should match, got %+v", res.Break)
	}
	// Exact match required ⇒ break.
	if res := MatchFill(fill, conf, MatchTolerance{}); res.Matched {
		t.Fatal("exact match should detect the 0.01 break")
	}
}

func TestReconcileBatch(t *testing.T) {
	fills := []*orderpb.Fill{fillFixture("F1"), fillFixture("F2"), fillFixture("F3")}
	badConf := confFixture("F2")
	badConf.Quantity = big.NewRat(80, 1) // F2 breaks on quantity
	confs := []Confirmation{
		confFixture("F1"), // clean
		badConf,           // break
		{ConfirmationID: "C-orphan", FillID: "F9"}, // no matching fill
	}
	breaks, unconfirmed, orphans := Reconcile(fills, confs, MatchTolerance{})
	if len(breaks) != 1 || breaks[0].FillID != "F2" {
		t.Fatalf("expected one F2 break, got %+v", breaks)
	}
	if len(unconfirmed) != 1 || unconfirmed[0] != "F3" {
		t.Fatalf("expected F3 unconfirmed, got %v", unconfirmed)
	}
	if len(orphans) != 1 || orphans[0] != "C-orphan" {
		t.Fatalf("expected C-orphan orphan, got %v", orphans)
	}
}
