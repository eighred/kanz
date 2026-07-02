package posttrade

import "time"

// Calendar is a settlement business-day calendar (PARITY-04d): it treats
// weekends and a configured holiday set as non-settling days, so the T+N
// settlement date and fail aging count SETTLEMENT days — the real market
// convention — instead of raw calendar days. A weekend or exchange holiday
// between the settlement date and now is not an aging day, so a pending
// instruction is never false-escalated across a Fri→Mon gap or a bank holiday.
//
// The dependency-free default (a nil *Calendar, or DetectFails) keeps the old
// calendar-day behavior; the composition root injects a real market calendar
// (the licensed holiday set is DATA, loaded there, the PARITY-03 parameter
// stance).
type Calendar struct {
	holidays map[civilDate]struct{}
	weekend  [7]bool // indexed by time.Weekday
}

// civilDate is a timezone-independent calendar date for holiday/day comparison.
type civilDate struct {
	y int
	m time.Month
	d int
}

func dateOf(t time.Time) civilDate {
	u := t.UTC()
	return civilDate{u.Year(), u.Month(), u.Day()}
}

func floorDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// NewCalendar builds a calendar with Sat/Sun as the default weekend and the
// given holiday dates (time-of-day ignored; compared in UTC).
func NewCalendar(holidays ...time.Time) *Calendar {
	c := &Calendar{holidays: make(map[civilDate]struct{})}
	c.weekend[time.Saturday] = true
	c.weekend[time.Sunday] = true
	for _, h := range holidays {
		c.holidays[dateOf(h)] = struct{}{}
	}
	return c
}

// WithWeekend overrides the non-settling weekday set (e.g. Fri/Sat markets).
// Returns the receiver for chaining.
func (c *Calendar) WithWeekend(days ...time.Weekday) *Calendar {
	c.weekend = [7]bool{}
	for _, d := range days {
		if d >= time.Sunday && d <= time.Saturday {
			c.weekend[d] = true
		}
	}
	return c
}

// IsBusinessDay reports whether t is a settling day (not a weekend or holiday).
// A nil calendar treats only Sat/Sun as non-settling.
func (c *Calendar) IsBusinessDay(t time.Time) bool {
	wd := t.UTC().Weekday()
	if c == nil {
		return wd != time.Saturday && wd != time.Sunday
	}
	if c.weekend[wd] {
		return false
	}
	_, holiday := c.holidays[dateOf(t)]
	return !holiday
}

// AddBusinessDays returns the date n settling days after t — the T+N settlement
// date. n<=0 returns t's date unchanged. The result lands on a business day.
func (c *Calendar) AddBusinessDays(t time.Time, n int) time.Time {
	d := floorDay(t)
	for n > 0 {
		d = d.AddDate(0, 0, 1)
		if c.IsBusinessDay(d) {
			n--
		}
	}
	return d
}

// BusinessDaysBetween counts settling days strictly after start's date up to and
// including end's date — how many settlement days a position has aged past its
// settlement date. Returns 0 when end is on or before start (not yet aged).
func (c *Calendar) BusinessDaysBetween(start, end time.Time) int {
	s := floorDay(start)
	e := floorDay(end)
	if !e.After(s) {
		return 0
	}
	n := 0
	for d := s.AddDate(0, 0, 1); !d.After(e); d = d.AddDate(0, 0, 1) {
		if c.IsBusinessDay(d) {
			n++
		}
	}
	return n
}

// agePastSettlement is the days a settlement has aged past its date: business
// days when a calendar is supplied (PARITY-04d), calendar days otherwise (the
// preserved default).
func agePastSettlement(cal *Calendar, settlementDate, now time.Time) int {
	if cal == nil {
		return daysBetween(settlementDate, now)
	}
	return cal.BusinessDaysBetween(settlementDate, now)
}
