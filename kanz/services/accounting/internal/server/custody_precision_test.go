package server

import (
	"math/big"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

func TestCustodyReviewSeparatesExactZeroLegacyAndUnavailable(t *testing.T) {
	b := custody.Break{ValuesVerified: true, IBOR: dec.Rat("1.000000000001"), Custodian: big.NewRat(1, 1), Diff: dec.Rat("0.000000000001"), Revision: 1}
	got := toBreakView(b, breakT0)
	if got.ValuesState != "exact" || got.Difference != "0.000000000001" {
		t.Fatalf("rounded review: %+v", got)
	}
	b.Diff = new(big.Rat)
	if got := toBreakView(b, breakT0); got.ValuesState != "exact" || got.Difference != "0" {
		t.Fatalf("lost genuine zero: %+v", got)
	}
	b.ValuesVerified = false
	if got := toBreakView(b, breakT0); got.ValuesState != "legacy_unverified" || got.Difference != "" {
		t.Fatalf("legacy presented as exact: %+v", got)
	}
	b.ValuesVerified = true
	b.Diff = nil
	if got := toBreakView(b, breakT0); got.ValuesState != "unavailable" || got.Difference != "" {
		t.Fatalf("missing became zero: %+v", got)
	}
	b.Diff = big.NewRat(1, 3)
	if got := toBreakView(b, breakT0); got.ValuesState != "unavailable" || got.Difference != "" {
		t.Fatalf("nonterminating rounded: %+v", got)
	}
}
