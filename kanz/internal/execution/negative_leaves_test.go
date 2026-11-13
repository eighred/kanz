package execution

import (
	"errors"
	"math/big"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/dec"
)

func TestLeavesRemainingRejectsNegativeQuantities(t *testing.T) {
	for _, tc := range []struct {
		name                string
		ordered, cumulative int64
	}{
		{"negative cumulative", 10, -1},
		{"negative ordered", -1, 0},
		{"both negative", -1, -2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaves, err := LeavesRemaining(&commonpb.Decimal{Coefficient: tc.ordered}, &commonpb.Decimal{Coefficient: tc.cumulative})
			if !errors.Is(err, ErrLeavesUnrepresentable) || leaves != nil {
				t.Fatalf("ordered %d, cumulative %d: leaves %v, error %v; want unsupported quantity and no result",
					tc.ordered, tc.cumulative, leaves, err)
			}
		})
	}
}

func TestLeavesRemainingPreservesNonnegativeDomain(t *testing.T) {
	for _, tc := range []struct {
		name                string
		ordered, cumulative *commonpb.Decimal
		want                *big.Rat
	}{
		{"known zero", &commonpb.Decimal{}, &commonpb.Decimal{}, big.NewRat(0, 1)},
		{"none filled", &commonpb.Decimal{Coefficient: 10}, &commonpb.Decimal{}, big.NewRat(10, 1)},
		{"exactly filled", &commonpb.Decimal{Coefficient: 10}, &commonpb.Decimal{Coefficient: 10}, big.NewRat(0, 1)},
		{"partial", &commonpb.Decimal{Coefficient: 10}, &commonpb.Decimal{Coefficient: 35, Exponent: -1}, big.NewRat(13, 2)},
		{"nano residual", &commonpb.Decimal{Coefficient: 10, Exponent: -9}, &commonpb.Decimal{Coefficient: 1, Exponent: -9}, big.NewRat(9, 1_000_000_000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaves, err := LeavesRemaining(tc.ordered, tc.cumulative)
			if err != nil || leaves == nil {
				t.Fatalf("supported leaves refused: %v", err)
			}
			got := dec.FromProto(leaves)
			if got.Cmp(tc.want) != 0 || got.Sign() < 0 || got.Cmp(dec.FromProto(tc.ordered)) > 0 {
				t.Fatalf("leaves = %s, want %s inside [0, ordered]", got.RatString(), tc.want.RatString())
			}
		})
	}
}
