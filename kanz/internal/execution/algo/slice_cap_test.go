package algo

import (
	"errors"
	"math/big"
	"testing"
)

func TestRegisteredAlgosRejectNegativeMaxSlice(t *testing.T) {
	for _, implementation := range Registered() {
		t.Run(string(implementation.Name()), func(t *testing.T) {
			for _, cap := range []*big.Rat{big.NewRat(-1, 1), big.NewRat(-1, 1_000_000_000)} {
				p := sliceCapPlan(implementation.Name(), cap)
				market := newCurveMarket(p, 100, 100, 100, 100, 100, 100)
				children, err := Run(p, ParentState{OrderID: "negative-cap"}, market)
				if !errors.Is(err, ErrCapUnsatisfiable) || len(children) != 0 {
					t.Fatalf("cap %s: got %d children, %v; positive children cannot satisfy a negative size bound",
						cap.RatString(), len(children), err)
				}
				if len(market.asked) != 0 {
					t.Fatal("an invalid size bound must refuse before consulting market volume")
				}
			}
		})
	}
}

func TestRegisteredAlgosPreserveNonnegativeMaxSliceContract(t *testing.T) {
	for _, implementation := range Registered() {
		t.Run(string(implementation.Name()), func(t *testing.T) {
			for _, tc := range []struct {
				name string
				cap  *big.Rat
			}{
				{"unset", nil},
				{"zero remains uncapped", new(big.Rat)},
				{"positive equality", big.NewRat(10, 1)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					p := sliceCapPlan(implementation.Name(), tc.cap)
					children, err := Run(p, ParentState{OrderID: "valid-cap"}, newCurveMarket(p, 100, 100, 100, 100, 100, 100))
					if err != nil || len(children) != 6 {
						t.Fatalf("got %d children, %v; want six equal children", len(children), err)
					}
					// The independent quantity oracle is 60 / 6 = 10. Every bucket
					// expects 100, so POV's 1/10 rate also permits exactly 10.
					for _, child := range children {
						if child.Quantity.Cmp(big.NewRat(10, 1)) != 0 {
							t.Fatalf("child %d quantity = %s, want 10", child.Index, child.Quantity.RatString())
						}
					}
				})
			}
			t.Run("positive violation", func(t *testing.T) {
				p := sliceCapPlan(implementation.Name(), big.NewRat(9, 1))
				children, err := Run(p, ParentState{OrderID: "small-cap"}, newCurveMarket(p, 100, 100, 100, 100, 100, 100))
				if !errors.Is(err, ErrCapUnsatisfiable) || len(children) != 0 {
					t.Fatalf("cap 9: got %d children, %v; every required child is 10", len(children), err)
				}
			})
		})
	}
}

func sliceCapPlan(name Name, cap *big.Rat) Plan {
	return Plan{
		Algo:             name,
		InstrumentID:     "BTC-USDT",
		Total:            big.NewRat(60, 1),
		Start:            volStart,
		End:              volEnd,
		Slices:           6,
		MaxSlice:         cap,
		MaxParticipation: big.NewRat(1, 10),
	}
}
