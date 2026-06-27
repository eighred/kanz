package pricing

import (
	"math"
	"testing"
	"time"
)

func TestYearFraction_Conventions(t *testing.T) {
	d := func(y, m, day int) time.Time { return time.Date(y, time.Month(m), day, 0, 0, 0, 0, time.UTC) }

	cases := []struct {
		name       string
		start, end time.Time
		dc         DayCount
		want, tol  float64
	}{
		{"ACT/360 Jan", d(2020, 1, 1), d(2020, 2, 1), Actual360, 31.0 / 360, 1e-12},
		{"ACT/365F Jan", d(2020, 1, 1), d(2020, 2, 1), Actual365Fixed, 31.0 / 365, 1e-12},
		{"30/360 Jan", d(2020, 1, 1), d(2020, 2, 1), Thirty360, 30.0 / 360, 1e-12},
		{"30/360 EOM", d(2020, 1, 31), d(2020, 2, 28), Thirty360, 28.0 / 360, 1e-12},
		{"30/360 full year", d(2020, 1, 15), d(2021, 1, 15), Thirty360, 1.0, 1e-12},
		// 2020 is a leap year (366 days); ACT/ACT ISDA across exactly the year = 1.0.
		{"ACT/ACT leap full year", d(2020, 1, 1), d(2021, 1, 1), ActualActual, 1.0, 1e-12},
		// Half of leap year 2020: Jan1→Jul1 = 182 days / 366.
		{"ACT/ACT half leap", d(2020, 1, 1), d(2020, 7, 1), ActualActual, 182.0 / 366, 1e-12},
	}
	for _, c := range cases {
		got := YearFraction(c.start, c.end, c.dc)
		if math.Abs(got-c.want) > c.tol {
			t.Errorf("%s: got %.10f want %.10f", c.name, got, c.want)
		}
	}
}
