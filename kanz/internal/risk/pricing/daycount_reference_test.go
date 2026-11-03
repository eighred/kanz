package pricing

import (
	"math"
	"math/big"
	"testing"
	"time"
)

// This reference counts individual UTC civil days rather than splitting the
// interval at year boundaries as the production ISDA implementation does.
func TestActualActualAgainstRationalDailyAccrual(t *testing.T) {
	for _, year := range []int{1899, 1999, 2000, 2099} {
		start := time.Date(year, 12, 15, 0, 0, 0, 0, time.UTC)
		want := new(big.Rat)
		for i := 0; i <= 455; i++ {
			end := start.AddDate(0, 0, i)
			ref, _ := want.Float64()
			for _, sign := range []float64{1, -1} {
				a, b := start, end
				if sign < 0 {
					a, b = b, a
				}
				got := YearFraction(a, b, ActualActual)
				if math.IsNaN(got) || math.Abs(got-sign*ref) > 1e-14 {
					t.Fatalf("%v to %v: got %.17g, reference %.17g", a, b, got, sign*ref)
				}
			}
			days := time.Date(end.Year(), 12, 31, 0, 0, 0, 0, time.UTC).YearDay()
			want.Add(want, big.NewRat(1, int64(days)))
		}
	}
}

func TestUnknownDayCountHasNoYearFraction(t *testing.T) {
	start := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, dc := range []DayCount{-1, 99} {
		if got := YearFraction(start, start.AddDate(0, 1, 0), dc); !math.IsNaN(got) {
			t.Errorf("unsupported convention %d produced accrual %g", dc, got)
		}
	}
}

func TestYieldSolverDoesNotTurnUnknownPriceIntoEndpoint(t *testing.T) {
	settle := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bond := Bond{Face: 100, Issue: settle, Maturity: settle.AddDate(1, 0, 0), DayCount: DayCount(99)}
	if y := bond.YieldFromPrice(settle, 98); !math.IsNaN(y) {
		t.Fatalf("unknown basis priced as yield %g", y)
	}
	bond.DayCount = Actual365Fixed
	for _, p := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if y := bond.YieldFromPrice(settle, p); !math.IsNaN(y) {
			t.Fatalf("nonfinite price produced yield %g", y)
		}
	}
}
