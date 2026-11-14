package alternatives

import (
	"math"
	"testing"
	"time"
)

// Multiplying every cashflow by one positive scalar cannot change a return.
// The independent equation is -1 + 1.1/(1+r) = 0 over exactly 365 days.
func TestIRRScaleInvariant(t *testing.T) {
	start := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		scale float64
	}{{"smallest_wire_pair", 1e-127}, {"small", 1e-12}, {"unit", 1}, {"large_wire_pair", 1e146}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IRR([]CashFlow{{start, -tc.scale}, {start.AddDate(1, 0, 0), 1.1 * tc.scale}})
			if err != nil || math.Abs(got-0.1) > 1e-10 {
				t.Fatalf("IRR = %.17g, %v; want 0.1 within 1e-10", got, err)
			}
		})
	}
}

func TestIRRRefusesUnidentifiableOrNonfinite(t *testing.T) {
	start := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	for _, tc := range []struct {
		name  string
		flows []CashFlow
	}{
		{"same_instant_zero", []CashFlow{{start, -100}, {start, 100}}},
		{"all_dates_cancel", []CashFlow{{start, -100}, {start, 100}, {end, -110}, {end, 110}}},
		{"same_instant_gain", []CashFlow{{start, -100}, {start, 110}}},
		{"nan", []CashFlow{{start, -100}, {end, 110}, {end, math.NaN()}}},
		{"positive_infinity", []CashFlow{{start, -100}, {end, math.Inf(1)}}},
		{"negative_infinity", []CashFlow{{start, math.Inf(-1)}, {end, 110}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := IRR(tc.flows); err == nil {
				t.Fatalf("unidentifiable/nonfinite stream returned IRR %v", got)
			}
		})
	}
}

func TestIRRPreservesDatedNettingAndBracketEndpoints(t *testing.T) {
	start := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	for _, tc := range []struct {
		name  string
		flows []CashFlow
		want  float64
	}{
		{"same_date_netting", []CashFlow{{end, 60}, {start, -100}, {end, 50}, {start, -25}, {start, 25}}, .1},
		{"zero_return", []CashFlow{{start, -100}, {end, 100}}, 0},
		{"lower_endpoint", []CashFlow{{start, -1}, {end, 1 - .9999}}, -.9999},
		{"upper_endpoint", []CashFlow{{start, -1}, {end, 11}}, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IRR(tc.flows)
			if err != nil || math.Abs(got-tc.want) > 1e-10 {
				t.Fatalf("IRR = %.17g, %v; want %.17g within 1e-10", got, err, tc.want)
			}
		})
	}
}

func TestIRRDatedIntervalAndInputPreservation(t *testing.T) {
	start := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	// Two millennia contain 485 Gregorian leap days. Actual/365 therefore
	// solves (1+r)^(730485/365)=1.1, without a time.Duration intermediate.
	want := math.Expm1(math.Log(1.1) * 365 / 730485)
	for _, tc := range []struct {
		name  string
		flows []CashFlow
		want  float64
	}{
		{"year_one_to_2001", []CashFlow{{end, 1.1}, {start, -1}}, want},
		{"one_nanosecond_zero_return", []CashFlow{{start, -1}, {start.Add(time.Nanosecond), 1}}, 0},
		{"one_nanosecond_zero_return_scaled", []CashFlow{{start, -100}, {start.Add(time.Nanosecond), 100}}, 0},
		{"negative_return", []CashFlow{{day(2022, 1, 1), 80}, {day(2021, 1, 1), -100}}, -.2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]CashFlow(nil), tc.flows...)
			got, err := IRR(tc.flows)
			if err != nil || math.Abs(got-tc.want) > 1e-10 {
				t.Fatalf("IRR = %.17g, %v; want %.17g within 1e-10", got, err, tc.want)
			}
			for i := range original {
				if tc.flows[i] != original[i] {
					t.Fatal("IRR changed caller-owned cashflows")
				}
			}
		})
	}
}

func TestIRRPermutationInvariant(t *testing.T) {
	start := day(2021, 1, 1)
	flows := []CashFlow{{start, -125}, {start, 25}, {day(2022, 1, 1), 110}, {day(2023, 1, 1), 7}, {day(2023, 1, 1), -7}}
	checked := 0
	var permutations func(int)
	permutations = func(first int) {
		if first == len(flows) {
			checked++
			got, err := IRR(flows)
			if err != nil || math.Abs(got-.1) > 1e-10 {
				t.Fatalf("permutation %d IRR = %.17g, %v; want .1", checked, got, err)
			}
			return
		}
		for i := first; i < len(flows); i++ {
			flows[first], flows[i] = flows[i], flows[first]
			permutations(first + 1)
			flows[first], flows[i] = flows[i], flows[first]
		}
	}
	permutations(0)
	if checked != 120 {
		t.Fatalf("checked %d permutations, want 5! = 120", checked)
	}
}

func TestIRRSmallResidualSurvivesDiscountCancellation(t *testing.T) {
	start := day(2021, 1, 1)
	for _, tc := range []struct {
		name string
		step time.Duration
		want float64
	}{
		// Independently solved with 100-digit Decimal arithmetic from the exact
		// binary float eps: x=2/(1+sqrt(1+4eps)), r=exp(-log(x)/years)-1.
		{"nanosecond", time.Nanosecond, .00031540973119240554039},
		{"microsecond", time.Microsecond, .00000031536004972597000989},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, scale := range []float64{1e-100, 1, 1e100} {
				base := []CashFlow{{start, -scale}, {start.Add(tc.step), scale}, {start.Add(2 * tc.step), 1e-20 * scale}}
				for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
					got, err := IRR([]CashFlow{base[order[0]], base[order[1]], base[order[2]]})
					if err != nil || math.Abs(got-tc.want) > 1e-10 {
						t.Fatalf("scale %g, order %v: IRR %.17g, %v; want %.17g within 1e-10", scale, order, got, err, tc.want)
					}
				}
			}
		})
	}
}
