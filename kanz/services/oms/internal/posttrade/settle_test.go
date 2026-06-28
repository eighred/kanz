package posttrade

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"
)

func TestNewInstructionAmount(t *testing.T) {
	s := NewInstruction("I1", fillFixture("F1"), "CP1", "CUST", "USD", settleDate())
	// amount = 100*150 + 5 = 15005
	if s.Amount.Cmp(big.NewRat(15005, 1)) != 0 {
		t.Fatalf("amount: want 15005 got %s", s.Amount.RatString())
	}
	if s.Status != StatusAffirmed {
		t.Fatalf("new instruction status: want affirmed got %s", s.Status)
	}
}

func TestSettlementStateMachine(t *testing.T) {
	s := NewInstruction("I1", fillFixture("F1"), "CP1", "", "USD", settleDate())
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
	s := NewInstruction("I1", fillFixture("F1"), "CP1", "", "USD", settleDate())
	// AFFIRMED -> SETTLED skips INSTRUCTED.
	if err := s.Settle(); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("settle from affirmed should be illegal, got %v", err)
	}
}

func TestFailFromNonTerminal(t *testing.T) {
	s := NewInstruction("I1", fillFixture("F1"), "CP1", "", "USD", settleDate())
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
	pending := NewInstruction("I1", fillFixture("F1"), "CP1", "", "USD", settleDate())
	if err := Instruct(ctx, venue, pending, settleDate().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if pending.Status != StatusInstructed {
		t.Fatalf("pre-date status: want instructed got %s", pending.Status)
	}

	// On/after the settlement date: settles.
	done := NewInstruction("I2", fillFixture("F2"), "CP1", "", "USD", settleDate())
	if err := Instruct(ctx, venue, done, settleDate()); err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusSettled {
		t.Fatalf("on-date status: want settled got %s", done.Status)
	}
}
