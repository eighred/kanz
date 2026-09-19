package collateral

import (
	"github.com/eighred/kanz/internal/dec"
	"testing"
)

func TestExactMarginEconomicBoundaries(t *testing.T) {
	terms := ExactCSATerms{Currency: "USD", Threshold: "0", MinimumTransfer: "50", IndependentAmount: "0", Rounding: "25"}
	for _, tc := range []struct{ exposure, held, initial, required, movement dec.Exact }{
		{"140", "100", "0", "140", "0"},
		{"260", "0", "0", "260", "275"},
		{"0", "260", "0", "0", "-250"},
		{"-100", "0", "60", "60", "75"},
		{"150", "100", "0", "150", "50"},
	} {
		got, err := CalculateMargin(tc.exposure, tc.held, tc.initial, terms)
		if err != nil || got.Required != tc.required || got.Movement != tc.movement {
			t.Fatalf("%+v: %+v %v", tc, got, err)
		}
	}
	terms.MinimumTransfer = "0"
	terms.Rounding = "0"
	got, err := CalculateMargin("9007199254740992.000000000000000001", "9007199254740992", "0", terms)
	if err != nil || got.Movement != "0.000000000000000001" {
		t.Fatalf("lost movement: %+v %v", got, err)
	}
	for _, bad := range []dec.Exact{"", "NaN", "-1", "1e1000000"} {
		if _, err := CalculateMargin("1", bad, "0", terms); err == nil {
			t.Fatalf("accepted held %q", bad)
		}
	}
}
