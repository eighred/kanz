package posttrade

import (
	"context"
	"errors"
	"testing"
	"time"
)

func instructed(id string, settleDate time.Time) *Settlement {
	s := NewInstruction(id, fillFixture("F-"+id), "CP1", "", "USD", settleDate)
	_ = s.Instruct()
	return s
}

// asOf returns settleDate + n days.
func plusDays(base time.Time, n int) time.Time { return base.Add(time.Duration(n) * 24 * time.Hour) }

func TestDetectFailsAging(t *testing.T) {
	base := settleDate()
	s := instructed("I1", base)
	settlements := []*Settlement{s}

	// Within grace (1 day) ⇒ no fail.
	if fails := DetectFails(settlements, DefaultAgingPolicy, plusDays(base, 1)); len(fails) != 0 {
		t.Fatalf("within grace should not fail, got %+v", fails)
	}
	// 2 days past ⇒ WARNING.
	if fails := DetectFails(settlements, DefaultAgingPolicy, plusDays(base, 2)); len(fails) != 1 || fails[0].Severity != SeverityWarning {
		t.Fatalf("2 days ⇒ warning, got %+v", fails)
	}
	// 3 days ⇒ CRITICAL.
	if fails := DetectFails(settlements, DefaultAgingPolicy, plusDays(base, 3)); len(fails) != 1 || fails[0].Severity != SeverityCritical {
		t.Fatalf("3 days ⇒ critical, got %+v", fails)
	}
	// 6 days ⇒ ESCALATED, with the right age.
	fails := DetectFails(settlements, DefaultAgingPolicy, plusDays(base, 6))
	if len(fails) != 1 || fails[0].Severity != SeverityEscalated || fails[0].AgeDays != 6 {
		t.Fatalf("6 days ⇒ escalated age 6, got %+v", fails)
	}
}

func TestDetectFailsSettledNeverFails(t *testing.T) {
	base := settleDate()
	s := instructed("I1", base)
	_ = s.Settle()
	if fails := DetectFails([]*Settlement{s}, DefaultAgingPolicy, plusDays(base, 10)); len(fails) != 0 {
		t.Fatalf("settled should never fail, got %+v", fails)
	}
}

func TestDetectFailsExplicitFailAlwaysReported(t *testing.T) {
	base := settleDate()
	s := instructed("I1", base)
	_ = s.Fail("counterparty rejected")
	// Detected the same day (within grace) — an explicit fail is still reported.
	fails := DetectFails([]*Settlement{s}, DefaultAgingPolicy, base)
	if len(fails) != 1 || fails[0].Severity == SeverityNone || fails[0].Reason != "counterparty rejected" {
		t.Fatalf("explicit fail should report with reason, got %+v", fails)
	}
}

func TestDetectFailsOrderedBySeverity(t *testing.T) {
	base := settleDate()
	warn := instructed("I-warn", plusDays(base, 4)) // 2 days aged at +6 ⇒ warning
	escalate := instructed("I-escalate", base)      // 6 days aged at +6 ⇒ escalated
	fails := DetectFails([]*Settlement{warn, escalate}, DefaultAgingPolicy, plusDays(base, 6))
	if len(fails) != 2 {
		t.Fatalf("want 2 fails, got %d (%+v)", len(fails), fails)
	}
	// The worst fail surfaces first.
	if fails[0].Severity != SeverityEscalated || fails[1].Severity != SeverityWarning {
		t.Fatalf("fails not ordered by descending severity: %+v", fails)
	}
}

func TestEmitFails(t *testing.T) {
	fails := []Fail{{InstructionID: "I1"}, {InstructionID: "I2"}}
	var got []string
	sink := FailSinkFunc(func(_ context.Context, f Fail) error {
		got = append(got, f.InstructionID)
		return nil
	})
	n, err := EmitFails(context.Background(), sink, fails)
	if err != nil || n != 2 || len(got) != 2 {
		t.Fatalf("emit all: n=%d err=%v got=%v", n, err, got)
	}

	// A publish error stops and reports how many succeeded.
	failing := FailSinkFunc(func(_ context.Context, f Fail) error {
		if f.InstructionID == "I2" {
			return errors.New("bus down")
		}
		return nil
	})
	n, err = EmitFails(context.Background(), failing, fails)
	if err == nil || n != 1 {
		t.Fatalf("publish error should stop at 1: n=%d err=%v", n, err)
	}
}
