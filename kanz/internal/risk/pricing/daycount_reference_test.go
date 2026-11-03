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
