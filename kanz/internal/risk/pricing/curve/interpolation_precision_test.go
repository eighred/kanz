package curve

import (
	"math"
	"math/big"
	"testing"
)

func TestInterpolationPreservesInteriorPillarsAndAdjacentQueries(t *testing.T) {
	for _, interp := range []Interpolation{LinearZero, LogLinearDF} {
		// A non-dyadic knot can change the last bit even in z*t/t. A known
		// pillar is a stored input, so no interpolation transform is needed.
		for _, row := range [][3]float64{{1.3, 1e-100, 0}, {2.7, 0, 1e-4}} {
			c, err := NewZeroCurve([]float64{.25, row[0], 5}, []float64{.03, row[1], .04}, Continuous, interp)
			if err != nil {
				t.Fatal(err)
			}
			if row[2] != 0 {
				c = (Parallel{Bp: 1}).Apply(c)
			}
			if got := c.Zero(row[0]); math.Float64bits(got) != math.Float64bits(row[1]+row[2]) {
				t.Errorf("non-dyadic pillar: %.17g want %.17g", got, row[1]+row[2])
			}
		}
		for _, middle := range []float64{0x1p-80, -0x1p-80, 0} {
			rates := []float64{1.0 / 32, middle, -1.0 / 16}
			c, err := NewZeroCurve([]float64{1, 2, 3}, rates, Continuous, interp)
			if err != nil {
				t.Fatal(err)
			}
			for _, bump := range []float64{0, 1e-4} {
				shifted := c
				if bump != 0 {
					shifted = (Parallel{Bp: 1}).Apply(c)
				}
				if got := shifted.Zero(2); math.Float64bits(got) != math.Float64bits(middle+bump) {
					t.Errorf("interp=%d bump=%g pillar=%.17g want %.17g", interp, bump, got, middle+bump)
				}
				for _, at := range []float64{1, math.Nextafter(1, 2), math.Nextafter(2, 1), 2, math.Nextafter(2, 3), math.Nextafter(3, 2), 3} {
					want := rationalInterpolation(at, rates, bump, interp)
					got := shifted.Zero(at)
					if !finite(got) || math.Abs(got-want) > 16*0x1p-52*math.Abs(want) {
						t.Errorf("interp=%d middle=%g bump=%g at=%.17g: %.17g want %.17g", interp, middle, bump, at, got, want)
					}
				}
			}
		}
	}
}

// Exact rational evaluation of the declared interpolant, rounded once at the
// result. Inputs include the pre-interpolation binary64 pillar bump. No curve
// evaluator, discount transform or floating-point interpolation is reused.
func rationalInterpolation(at float64, rates []float64, bump float64, interp Interpolation) float64 {
	a, b, index := 1.0, 2.0, 0
	if at > 2 {
		a, b, index = 2, 3, 1
	}
	r := func(v float64) *big.Rat { return new(big.Rat).SetFloat64(v) }
	left := new(big.Rat).Mul(r(rates[index]+bump), new(big.Rat).Sub(r(b), r(at)))
	right := new(big.Rat).Mul(r(rates[index+1]+bump), new(big.Rat).Sub(r(at), r(a)))
	if interp == LogLinearDF {
		left.Mul(left, r(a))
		right.Mul(right, r(b))
	}
	value := new(big.Rat).Quo(new(big.Rat).Add(left, right), r(b-a))
	if interp == LogLinearDF {
		value.Quo(value, r(at))
	}
	want, _ := value.Float64()
	return want
}

func TestMixedCalibrationPreservesSmallInteriorQuote(t *testing.T) {
	for _, interp := range []Interpolation{LinearZero, LogLinearDF} {
		for _, tiny := range []float64{-1e-20, 0, 1e-20} {
			quotes := []RateQuote{{Kind: Deposit, Tenor: 1, Value: .03}, {Kind: Deposit, Tenor: 2, Value: tiny}, {Kind: Deposit, Tenor: 3, Value: .04}}
			c, err := Calibrate(quotes, interp)
			if err != nil {
				t.Errorf("interp=%d quote=%g: %v", interp, tiny, err)
				continue
			}
			for _, q := range quotes {
				if got := c.QuoteRate(q); !finite(got) || math.Abs(got-q.Value) > 128*0x1p-52*math.Abs(q.Value) {
					t.Errorf("interp=%d tenor=%g: %.17g want %.17g", interp, q.Tenor, got, q.Value)
				}
			}
		}
	}
}
