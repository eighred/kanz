package depth

import (
	"testing"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
)

// levelBuilders are the two REAL venue ingress converters. They are tested
// together because they are the same rule at two exchanges, and the failure #94
// found was that a copy diverges the moment only one of them is touched.
var levelBuilders = map[string]func([][]string) []*marketpb.PriceLevel{
	"binance": binanceLevels,
	"okx":     okxLevels,
}

// A LARGE DEPTH LEVEL IS CARRIED EXACTLY, NOT WRAPPED (#94/#189).
//
// This is the venue INGRESS, upstream of the book, and the book hands exact
// rationals to pkg/alpha — so a wrong number here prices and sizes a real order.
// dec.ToProto wrapped above roughly 92.2 billion units at scale 8. That is
// implausible for a price and ORDINARY for a size: a depth level holding
// trillions of tokens is normal on both these venues, and 1e12 came through as
// 77662796314.5224192.
//
// A wrapped size is worse than a missing one, because it is perfectly
// representable — the refusal added to translate.toDec (#187) cannot catch it,
// and nothing downstream can tell it from real liquidity.
func TestVenueLevelsCarryALargeSizeExactly(t *testing.T) {
	const bigSize = "1000000000000" // 1e12 tokens on one level

	for name, build := range levelBuilders {
		t.Run(name, func(t *testing.T) {
			out := build([][]string{{"0.00001234", bigSize}})
			if len(out) != 1 {
				t.Fatalf("got %d levels, want 1 — a large but representable level must be kept", len(out))
			}
			if got := dec.Str(dec.FromProto(out[0].GetSize())); got != bigSize {
				t.Fatalf("size = %s, want %s — the book would report liquidity that is not there, "+
					"and an order would be sized against it", got, bigSize)
			}
			if got := dec.Str(dec.FromProto(out[0].GetPrice())); got != "0.00001234" {
				t.Fatalf("price = %s, want 0.00001234", got)
			}
		})
	}
}

// An unparseable level is still dropped — the pre-existing contract ("drop it
// rather than guess a price"), which the #189 decision deliberately matched
// rather than replaced.
func TestVenueLevelsStillDropUnreadableOnes(t *testing.T) {
	for name, build := range levelBuilders {
		t.Run(name, func(t *testing.T) {
			out := build([][]string{
				{"100", "1"},     // good
				{"abc", "1"},     // unparseable price
				{"100", "n/a"},   // unparseable size
				{"onlyonefield"}, // malformed pair
				{"101", "2"},     // good
			})
			if len(out) != 2 {
				t.Fatalf("got %d levels, want 2 — only the two readable ones may survive", len(out))
			}
		})
	}
}

// NON-VACUITY: ordinary levels pass through untouched, so a converter that
// dropped everything would not satisfy the tests above.
func TestVenueLevelsKeepOrdinaryDepth(t *testing.T) {
	for name, build := range levelBuilders {
		t.Run(name, func(t *testing.T) {
			out := build([][]string{{"50000.25", "1.5"}, {"50000.24", "0.00000001"}})
			if len(out) != 2 {
				t.Fatalf("got %d levels, want 2", len(out))
			}
			if got := dec.Str(dec.FromProto(out[0].GetPrice())); got != "50000.25" {
				t.Errorf("price = %s, want 50000.25", got)
			}
			if got := dec.Str(dec.FromProto(out[1].GetSize())); got != "0.00000001" {
				t.Errorf("size = %s, want 0.00000001 (the smallest level must survive too)", got)
			}
		})
	}
}

// A zero size is the venue's "remove this level" instruction and must survive the
// conversion — dropping it would leave a filled level in the book forever.
func TestVenueLevelsPreserveAZeroSize(t *testing.T) {
	for name, build := range levelBuilders {
		t.Run(name, func(t *testing.T) {
			out := build([][]string{{"100", "0"}})
			if len(out) != 1 {
				t.Fatalf("got %d levels, want 1 — a zero size is a REMOVE instruction, not noise", len(out))
			}
			if !dec.IsZero(out[0].GetSize()) {
				t.Fatal("zero size did not survive the conversion")
			}
		})
	}
}
