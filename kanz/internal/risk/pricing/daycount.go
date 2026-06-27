package pricing

import "time"

// Day-count conventions (FI-01c) — the accrual basis for coupon interest and the
// year fractions bond analytics discount over. Mirrors
// reference.v1.DayCountConvention; the bond library dispatches on it directly
// rather than parsing a free-text string.

// DayCount is an accrual convention.
type DayCount int

const (
	// ActualActual is ACT/ACT (ISDA): actual days, split across calendar years
	// by 365/366. The government-bond basis.
	ActualActual DayCount = iota
	// Actual365Fixed is ACT/365F: actual days / 365.
	Actual365Fixed
	// Actual360 is ACT/360: actual days / 360 (money-market basis).
	Actual360
	// Thirty360 is 30/360 (US/Bond basis): each month 30 days, year 360.
	Thirty360
)

// YearFraction returns the accrual fraction between start and end under the
// convention. end before start yields a negative fraction (callers pass
// start ≤ end).
func YearFraction(start, end time.Time, dc DayCount) float64 {
	switch dc {
	case Actual360:
		return actualDays(start, end) / 360
	case Actual365Fixed:
		return actualDays(start, end) / 365
	case Thirty360:
		return thirty360Days(start, end) / 360
	default: // ActualActual (ISDA)
		return actActISDA(start, end)
	}
}

// actualDays is the signed actual day count between two dates.
func actualDays(start, end time.Time) float64 {
	return end.Sub(start).Hours() / 24
}

// thirty360Days is the 30/360 (US) day count: months are 30 days, with the
// standard end-of-month adjustments.
func thirty360Days(start, end time.Time) float64 {
	d1, d2 := start.Day(), end.Day()
	if d1 == 31 {
		d1 = 30
	}
	if d2 == 31 && d1 == 30 {
		d2 = 30
	}
	return float64(360*(end.Year()-start.Year()) + 30*(int(end.Month())-int(start.Month())) + (d2 - d1))
}

// actActISDA is ACT/ACT (ISDA): the actual days in each calendar year the period
// spans, each divided by that year's length (365 or 366), summed.
func actActISDA(start, end time.Time) float64 {
	if !end.After(start) {
		if end.Equal(start) {
			return 0
		}
		return -actActISDA(end, start)
	}
	if start.Year() == end.Year() {
		return actualDays(start, end) / yearLength(start.Year())
	}
	frac := 0.0
	// First (partial) year.
	yearEnd := time.Date(start.Year()+1, 1, 1, 0, 0, 0, 0, start.Location())
	frac += actualDays(start, yearEnd) / yearLength(start.Year())
	// Whole years in between.
	for y := start.Year() + 1; y < end.Year(); y++ {
		frac++
	}
	// Last (partial) year.
	yearStart := time.Date(end.Year(), 1, 1, 0, 0, 0, 0, end.Location())
	frac += actualDays(yearStart, end) / yearLength(end.Year())
	return frac
}

func yearLength(y int) float64 {
	if isLeap(y) {
		return 366
	}
	return 365
}

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }
