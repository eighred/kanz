package posttrade

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

func mustInstruct(t *testing.T, id string, fill *orderpb.Fill, custodian, currency string) *Settlement {
	t.Helper()
	s, err := NewInstruction(id, fill, "CP1", custodian, currency, settleDate())
	if err != nil {
		t.Fatalf("new instruction %s: %v", id, err)
	}
	return s
}

func TestNewInstructionAmount(t *testing.T) {
	s := mustInstruct(t, "I1", fillFixture("F1"), "CUST", "USD")
	// A BUY pays the notional PLUS the fee: 100*150 + 5 = 15005.
	if s.Amount.Cmp(big.NewRat(15005, 1)) != 0 {
		t.Fatalf("amount: want 15005 got %s", s.Amount.RatString())
	}
	if s.Status != StatusAffirmed {
		t.Fatalf("new instruction status: want affirmed got %s", s.Status)
	}
}

func TestSpotSettlementInstructsNativeLegs(t *testing.T) {
	fill := fillFixture("spot")
	fill.InstrumentId, fill.SpotVenueSymbol = "BTC-USDT", "BTC-USDT"
	fill.Quantity = &commonpb.Decimal{Coefficient: 1}
	fill.Price = &commonpb.Decimal{Coefficient: 50000}
	fill.Fee = &commonpb.Money{CurrencyCode: "BTC", Amount: &commonpb.Decimal{Coefficient: 8, Exponent: -4}}
	for _, tc := range []struct {
		side     orderpb.Side
		fee      int64
		quantity int64
	}{
		{orderpb.Side_SIDE_BUY, 8, 9992}, {orderpb.Side_SIDE_SELL, 8, 10008},
		{orderpb.Side_SIDE_BUY, -8, 10008}, {orderpb.Side_SIDE_SELL, -8, 9992},
	} {
		fill.Side, fill.Fee.Amount.Coefficient = tc.side, tc.fee
		s := mustInstruct(t, "instruction", fill, "account", "USDT")
		if s.Quantity.Cmp(big.NewRat(tc.quantity, 10000)) != 0 || s.Amount.Cmp(big.NewRat(50000, 1)) != 0 {
			t.Fatal(s)
		}
	}
	fill.SpotVenueSymbol = "BTC-USD"
	if _, err := NewInstruction("instruction", fill, "cp", "account", "USDT", settleDate()); err == nil {
		t.Fatal("mismatched pair accepted")
	}
	fill.SpotVenueSymbol = "BTC-USDT"
	if _, err := NewInstruction("instruction", fill, "cp", "account", "USD", settleDate()); !errors.Is(err, dec.ErrCurrencyMismatch) {
		t.Fatal(err)
	}
	fill.Fee.Amount = nil
	if _, err := NewInstruction("instruction", fill, "cp", "account", "USDT", settleDate()); err == nil {
		t.Fatal("unknown fee accepted")
	}
}

// #221: a SELLER nets the fee OUT of the proceeds. Adding it on both sides
// instructed 15007.50 for a SELL the ledger books as +14992.50 — a 15.00 break
// between the instruction and the book of record, which no reconciliation can
// close because both sides believe their own number.
func TestNewInstructionSellNetsFeeOutOfProceeds(t *testing.T) {
	fill := fillFixture("F1")
	fill.Side = orderpb.Side_SIDE_SELL
	fill.Fee = &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 750, Exponent: -2}, CurrencyCode: "USD"}

	s := mustInstruct(t, "I1", fill, "CUST", "USD")
	want := big.NewRat(1499250, 100) // 100*150 - 7.50
	if s.Amount.Cmp(want) != 0 {
		t.Fatalf("sell amount: want 14992.5 got %s", s.Amount.RatString())
	}
}

// #221: order.v1.Fill.fee carries its own currency and a crypto venue charges in
// the base asset or a discount token. Folding a BNB fee into a USD instruction
// instructs a cash amount the counterparty never agreed to.
func TestNewInstructionRefusesFeeInAnotherCurrency(t *testing.T) {
	fill := fillFixture("F1")
	fill.Fee = &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 75, Exponent: -6}, CurrencyCode: "BNB"}

	s, err := NewInstruction("I1", fill, "CP1", "CUST", "USD", settleDate())
	if err == nil {
		t.Fatalf("a BNB fee on a USD instruction must be refused, got amount=%s", s.Amount.RatString())
	}
	if !errors.Is(err, dec.ErrCurrencyMismatch) {
		t.Fatalf("want ErrCurrencyMismatch, got %v", err)
	}
	for _, want := range []string{"F1", `"BNB"`, `"USD"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must name %s so an operator can act on it: %v", want, err)
		}
	}
}

func TestSettlementStateMachine(t *testing.T) {
	s := mustInstruct(t, "I1", fillFixture("F1"), "", "USD")
	if err := s.Instruct(); err != nil || s.Status != StatusInstructed {
		t.Fatalf("affirmed->instructed: %v status=%s", err, s.Status)
	}
	if err := s.Settle(); err != nil || s.Status != StatusSettled {
		t.Fatalf("instructed->settled: %v status=%s", err, s.Status)
	}
	// Terminal: cannot settle or fail again.
	if err := s.Settle(); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("settle from settled should be illegal, got %v", err)
	}
	if err := s.Fail("x"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("fail from settled should be illegal, got %v", err)
	}
}

func TestIllegalTransitionSkippingStates(t *testing.T) {
	s := mustInstruct(t, "I1", fillFixture("F1"), "", "USD")
	// AFFIRMED -> SETTLED skips INSTRUCTED.
	if err := s.Settle(); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("settle from affirmed should be illegal, got %v", err)
	}
}

func TestFailFromNonTerminal(t *testing.T) {
	s := mustInstruct(t, "I1", fillFixture("F1"), "", "USD")
	_ = s.Instruct()
	if err := s.Fail("custodian rejected"); err != nil {
		t.Fatalf("fail from instructed: %v", err)
	}
	if s.Status != StatusFailed || s.FailReason != "custodian rejected" {
		t.Fatalf("fail state: status=%s reason=%q", s.Status, s.FailReason)
	}
}

func TestInstructSimVenueSettlesOnDate(t *testing.T) {
	venue := NewSimSettlementVenue("SIM")
	ctx := context.Background()

	// Before the settlement date: stays INSTRUCTED (pending).
	pending := mustInstruct(t, "I1", fillFixture("F1"), "", "USD")
	if err := Instruct(ctx, venue, pending, settleDate().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if pending.Status != StatusInstructed {
		t.Fatalf("pre-date status: want instructed got %s", pending.Status)
	}

	// On/after the settlement date: settles.
	done := mustInstruct(t, "I2", fillFixture("F2"), "", "USD")
	if err := Instruct(ctx, venue, done, settleDate()); err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusSettled {
		t.Fatalf("on-date status: want settled got %s", done.Status)
	}
}
