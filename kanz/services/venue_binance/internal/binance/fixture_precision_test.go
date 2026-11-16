package binance

import (
	"math/big"
	"testing"

	"github.com/eighred/kanz/internal/dec"
)

func TestDecimalFixturePreservesExactValues(t *testing.T) {
	for _, tc := range []struct {
		text string
		want *big.Rat
	}{
		{"1000000000000", big.NewRat(1000000000000, 1)},
		{"1234567890123456789", big.NewRat(1234567890123456789, 1)},
		{"0.12345678901", big.NewRat(12345678901, 100000000000)},
		{"-0.0000000001", big.NewRat(-1, 10000000000)},
		{"0", big.NewRat(0, 1)},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got, ok := dec.FromProtoChecked(bdec(tc.text))
			if !ok || got.Cmp(tc.want) != 0 {
				t.Fatalf("fixture changed %s: got %v, want %s", tc.text, got, tc.want.RatString())
			}
		})
	}
}

func TestDecimalFixtureRefusesInvalidOrUnrepresentableValues(t *testing.T) {
	for _, text := range []string{"", "not-a-decimal", "1/3", "9223372036854775808", "1e100"} {
		t.Run(text, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid fixture must fail loudly instead of testing a substituted value")
				}
			}()
			bdec(text)
		})
	}
}
