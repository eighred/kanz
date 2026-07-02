package posttrade

import (
	"testing"
	"time"
)

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestAddBusinessDaysT2SkipsWeekend(t *testing.T) {
	cal := NewCalendar()
	// Trade Thursday 2026-01-01 is a holiday? No — it's a Thursday. T+2 → skip
	// Sat/Sun, landing on the following Monday.
	trade := date(2026, 1, 1) // Thursday
	got := cal.AddBusinessDays(trade, 2)
	want := date(2026, 1, 5) // Fri(+1), Mon(+2)
	if !got.Equal(want) {
		t.Errorf("T+2 from Thu = %s want %s (Mon)", got.Format("2006-01-02 Mon"), want.Format("2006-01-02 Mon"))
	}
}

func TestAddBusinessDaysSkipsHoliday(t *testing.T) {
	// 2026-01-19 is a Monday; mark it a holiday. T+1 from Fri 2026-01-16 skips
	// Sat/Sun AND the Monday holiday, landing Tuesday.
	cal := NewCalendar(date(2026, 1, 19))
	got := cal.AddBusinessDays(date(2026, 1, 16), 1) // Friday
	want := date(2026, 1, 20)                        // Tuesday
	if !got.Equal(want) {
		t.Errorf("T+1 over a holiday weekend = %s want %s", got.Format("2006-01-02 Mon"), want.Format("2006-01-02 Mon"))
	}
}

func TestBusinessDaysBetweenExcludesWeekend(t *testing.T) {
	cal := NewCalendar()
	// Fri 2026-01-16 → Mon 2026-01-19: only Monday is a settling day after Fri.
	if got := cal.BusinessDaysBetween(date(2026, 1, 16), date(2026, 1, 19)); got != 1 {
		t.Errorf("Fri→Mon business days = %d want 1 (weekend excluded)", got)
	}
	// Same span in calendar days is 3.
	if got := daysBetween(date(2026, 1, 16), date(2026, 1, 19)); got != 3 {
		t.Errorf("Fri→Mon calendar days = %d want 3", got)
	}
	if got := cal.BusinessDaysBetween(date(2026, 1, 19), date(2026, 1, 16)); got != 0 {
		t.Errorf("reversed span = %d want 0", got)
	}
}

// The business-day calendar prevents a weekend from false-escalating a pending
// instruction: aged over Fri→Mon it is 1 business day (WARNING), not 3 calendar
// days (which would trip CRITICAL under the default policy).
func TestDetectFailsWithCalendarNoWeekendFalseEscalation(t *testing.T) {
	s := &Settlement{
		InstructionID:  "i1",
		SettlementDate: date(2026, 1, 16), // Friday
		Status:         StatusInstructed,
	}
	now := date(2026, 1, 20) // Tuesday

	// Business days Fri→Tue = Mon,Tue = 2 → WARNING (past the 1-day grace, below
	// the 3-day CRITICAL threshold).
	calFails := DetectFailsWithCalendar([]*Settlement{s}, DefaultAgingPolicy, NewCalendar(), now)
	if len(calFails) != 1 {
		t.Fatalf("business-day: got %d fails want 1", len(calFails))
	}
	if calFails[0].AgeDays != 2 || calFails[0].Severity != SeverityWarning {
		t.Errorf("business-day fail = age %d sev %s want age 2 WARNING", calFails[0].AgeDays, calFails[0].Severity)
	}

	// Calendar-day aging (the old behavior) sees 4 days → CRITICAL, the false
	// escalation the settlement calendar fixes.
	calendarDayFails := DetectFails([]*Settlement{s}, DefaultAgingPolicy, now)
	if calendarDayFails[0].AgeDays != 4 || calendarDayFails[0].Severity != SeverityCritical {
		t.Errorf("calendar-day fail = age %d sev %s want age 4 CRITICAL", calendarDayFails[0].AgeDays, calendarDayFails[0].Severity)
	}
}
