package ledger

import (
	"math/big"
	"testing"
)

// The IBOR carries a hand-copy of the OMS position fold, and the divide-by-zero
// that crash-looped the OMS (#217) is present in this copy's arithmetic too.
//
// The difference — established by running it, not by reading it — is that every
// caller on THIS side already screens a zero quantity out: Apply at
// `e.Quantity.Sign() != 0`, foldContribution at `qty.Sign() == 0`. So accounting
// was never exposed the way the OMS was. These tests pin the screening at the
// entry point (which is what makes it safe) and the arithmetic underneath it
// (which is what stops the next edit from making it unsafe again).
func TestApplyScreensOutAZeroQuantityEntry(t *testing.T) {
	b := NewBook("PF")

	b.Apply(trade("z1", "AAPL", "0", "150", 2, 2))

	if _, ok := b.Positions["AAPL"]; ok {
		t.Error("a zero-quantity entry created a holding — Apply's Sign() != 0 screen is " +
			"what keeps foldPosition's divisor non-zero; without it this panics")
	}
}

// foldPosition directly: the screen above is the reason this is unreachable in
// production, which is exactly why the arithmetic is worth pinning on its own. A
// future caller does not inherit Apply's screen, and this line's failure mode is
// taking the process down rather than returning a wrong number.
func TestFoldPositionSurvivesALotThatNetsToZero(t *testing.T) {
	b := NewBook("PF")

	b.traded().foldPosition("AAPL", new(big.Rat), big.NewRat(150, 1))

	pos, ok := b.Positions["AAPL"]
	if !ok {
		t.Fatal("foldPosition should have created the holding")
	}
	if pos.Qty.Sign() != 0 {
		t.Errorf("qty = %s, want 0", pos.Qty.RatString())
	}
	if pos.AvgCost.Sign() != 0 {
		t.Errorf("a flat holding has no basis; avgCost = %s, want 0", pos.AvgCost.RatString())
	}
}

// A zero-quantity entry must not corrupt a holding folded afterwards.
func TestZeroQuantityEntryLeavesLaterFoldsCorrect(t *testing.T) {
	b := NewBook("PF")

	b.Apply(trade("z1", "AAPL", "0", "999999", 2, 2))
	b.Apply(trade("t1", "AAPL", "100", "150", 2, 2))

	pos := b.Positions["AAPL"]
	if pos.Qty.Cmp(big.NewRat(100, 1)) != 0 {
		t.Errorf("qty = %s, want 100", pos.Qty.RatString())
	}
	if pos.AvgCost.Cmp(big.NewRat(150, 1)) != 0 {
		t.Errorf("avgCost = %s, want 150 — the 999999 price on the zero-quantity entry "+
			"leaked into the basis", pos.AvgCost.RatString())
	}
}

// A full close nets to zero through the opposite-direction arm and must also survive.
func TestFullCloseNetsToZeroWithoutPanicking(t *testing.T) {
	b := NewBook("PF")
	b.Apply(trade("t1", "AAPL", "100", "150", 2, 2))
	b.Apply(trade("t2", "AAPL", "-100", "170", 3, 3))

	if got := b.Positions["AAPL"].Qty; got.Sign() != 0 {
		t.Errorf("after a full close, qty = %s, want 0", got.RatString())
	}
}
