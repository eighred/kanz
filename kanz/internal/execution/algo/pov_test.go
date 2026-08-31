package algo

import (
	"errors"
	"math/big"
	"strings"
	"testing"
)

// POV: THE CAP IS NEVER EXCEEDED, AND AN ORDER THAT CANNOT HONOUR IT IS REFUSED
// (#869).
//
// The property is one-sided and that is the whole point of a participation limit:
// being under the cap is always acceptable, being over it once is the failure. So
// the assertions below are inequalities, checked on EVERY slice of EVERY schedule
// this file produces — including the ones swept over a simulated tape, where no
// expectation is written down at all and the only thing asserted is the bound.
//
// The closed-form expectations were computed independently as exact fractions:
// 480 against a curve of 1000/2000/3000 at an 8% cap is 80/160/240, which is 8% of
// each — the boundary case, allowed — and 481 is refused.

func povPlan(total int64, capNum, capDen int64, slices int) Plan {
	p := Plan{
		Algo:         NamePOV,
		InstrumentID: "BTC-USDT",
		Total:        new(big.Rat).SetInt64(total),
		Start:        volStart,
		End:          volEnd,
		Slices:       slices,
	}
	if capDen != 0 {
		p.MaxParticipation = new(big.Rat).SetFrac64(capNum, capDen)
	}
	return p
}

// assertUnderCap is the property, applied to a whole schedule.
//
// IT IS A SEPARATE FUNCTION SO EVERY TEST IN THIS FILE USES THE SAME ONE. A cap
// check written out at each call site is how one of them ends up comparing the
// wrong pair, and a participation control that is checked three ways is checked no
// ways.
func assertUnderCap(t *testing.T, got []Slice, vols []int64, capRate *big.Rat) {
	t.Helper()
	for _, s := range got {
		v := new(big.Rat).SetInt64(vols[s.Index])
		limit := new(big.Rat).Mul(capRate, v)
		if s.Quantity.Cmp(limit) > 0 {
			rate := new(big.Rat).Quo(s.Quantity, v)
			t.Errorf("slice %d participates at %s of the %d expected in its interval, above the "+
				"cap of %s — a participation limit exceeded once is a limit that does not exist",
				s.Index, rate.RatString(), vols[s.Index], capRate.RatString())
		}
	}
}

// ===== THE ALLOCATION, AT AND BELOW THE CAP =====

// POV WORKS THE PARENT AT A CONSTANT PARTICIPATION, AND NEVER ABOVE THE CAP.
func TestPOV_AllocatesUnderTheCap(t *testing.T) {
	for _, tt := range []struct {
		name   string
		total  int64
		vols   []int64
		capN   int64
		capD   int64
		want   []string
		atCap  bool
		expect string
	}{
		{
			name:  "exactly at the cap",
			total: 480,
			vols:  []int64{1000, 2000, 3000},
			capN:  8, capD: 100,
			want:  []string{"80", "160", "240"},
			atCap: true,
		},
		{
			name:  "comfortably below the cap",
			total: 60,
			vols:  []int64{1000, 2000, 3000},
			capN:  8, capD: 100,
			want: []string{"10", "20", "30"},
		},
		{
			name:  "a curve that concentrates in one interval",
			total: 100,
			vols:  []int64{50, 50, 900},
			capN:  1, capD: 10,
			want: []string{"5", "5", "90"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := povPlan(tt.total, tt.capN, tt.capD, len(tt.vols))
			mkt := newCurveMarket(p, tt.vols...)

			got, err := Run(p, ParentState{OrderID: "p1"}, mkt)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			for i, want := range tt.want {
				w, _ := new(big.Rat).SetString(want)
				if got[i].Quantity.Cmp(w) != 0 {
					t.Errorf("slice %d = %s, want %s", i, got[i].Quantity.RatString(), w.RatString())
				}
			}
			assertUnderCap(t, got, tt.vols, p.MaxParticipation)
			if Sum(got).Cmp(p.Total) != 0 {
				t.Errorf("children sum to %s, parent is %s", Sum(got).RatString(), p.Total.RatString())
			}
			if tt.atCap {
				// THE BOUNDARY IS INCLUSIVE, and it has to be tested from the
				// inside: a `>=` where a `>` belongs refuses the largest order the
				// cap permits, and a desk would read that as the cap being wrong.
				rate := new(big.Rat).Quo(got[0].Quantity, new(big.Rat).SetInt64(tt.vols[0]))
				if rate.Cmp(p.MaxParticipation) != 0 {
					t.Errorf("the at-cap case participates at %s, not at the cap of %s — the "+
						"fixture no longer tests the boundary", rate.RatString(),
						p.MaxParticipation.RatString())
				}
			}
		})
	}
}

// ===== THE SWEEP: THE CAP HOLDS ON ANY TAPE =====

// ON EVERY TAPE THIS SWEEP GENERATES, NO CHILD EXCEEDS THE CAP.
//
// # Why a sweep and not more table rows
//
// A table asserts the arithmetic on the shapes somebody thought of. The property
// is universal — "never more than n% of prints" has no exceptions — so what has to
// be shown is that no curve produces a slice over the bound, including the ones
// nobody would write down: a curve with one interval a thousand times the others,
// one that is nearly flat, one whose values share no common factor with the
// parent.
//
// # It is deterministic, and that is deliberate
//
// The generator is a fixed linear congruential sequence with a fixed seed, so this
// test runs the same 2,000-odd schedules on every machine and in every CI run. A
// property test that varies per run either finds a failure nobody can reproduce or
// hides one behind a seed nobody recorded.
//
// The parent is swept from the smallest workable size up to EXACTLY the largest
// the cap permits (cap·V), because the bound is inclusive there and that is where
// an off-by-one in the comparison lives.
func TestPOV_NoSliceExceedsTheCapOnAnySimulatedTape(t *testing.T) {
	caps := []*big.Rat{
		new(big.Rat).SetFrac64(1, 100),
		new(big.Rat).SetFrac64(8, 100),
		new(big.Rat).SetFrac64(1, 3),
		new(big.Rat).SetInt64(1),
	}

	rng := uint64(0x9E3779B97F4A7C15)
	next := func(mod int64) int64 {
		rng = rng*6364136223846793005 + 1442695040888963407
		return int64((rng>>33)%uint64(mod)) + 1
	}

	schedules, refusals := 0, 0
	for shape := 0; shape < 60; shape++ {
		n := 2 + shape%7
		vols := make([]int64, n)
		var v big.Rat
		totalVol := new(big.Rat)
		for i := range vols {
			// A DELIBERATELY WIDE SPREAD: every fourth shape gets one interval
			// three orders of magnitude larger than its neighbours, which is the
			// thin-instrument case the cap exists for.
			vols[i] = next(1000)
			if shape%4 == 0 && i == n/2 {
				vols[i] *= 1000
			}
			v.SetInt64(vols[i])
			totalVol.Add(totalVol, &v)
		}
		for _, capRate := range caps {
			workable := new(big.Rat).Mul(capRate, totalVol)
			for _, frac := range []*big.Rat{
				new(big.Rat).SetFrac64(1, 1000),
				new(big.Rat).SetFrac64(1, 2),
				new(big.Rat).SetFrac64(999, 1000),
				new(big.Rat).SetInt64(1), // exactly cap·V — the inclusive boundary
			} {
				total := new(big.Rat).Mul(workable, frac)
				if total.Sign() <= 0 {
					continue
				}
				p := Plan{
					Algo:             NamePOV,
					InstrumentID:     "BTC-USDT",
					Total:            total,
					Start:            volStart,
					End:              volEnd,
					Slices:           n,
					MaxParticipation: capRate,
				}
				got, err := Run(p, ParentState{OrderID: "p1"}, newCurveMarket(p, vols...))
				if err != nil {
					refusals++
					t.Errorf("shape %d cap %s frac %s: refused a parent inside its own cap: %v",
						shape, capRate.RatString(), frac.RatString(), err)
					continue
				}
				schedules++
				assertUnderCap(t, got, vols, capRate)
				if Sum(got).Cmp(p.Total) != 0 {
					t.Errorf("shape %d: children sum to %s, parent is %s",
						shape, Sum(got).RatString(), p.Total.RatString())
				}
			}
		}
	}

	// NON-VACUITY. A sweep that generated no schedule would pass by asserting
	// nothing, which is how a property test survives the removal of the property.
	if schedules < 900 {
		t.Fatalf("the sweep produced only %d schedules (%d refusals) — it is no longer exercising "+
			"the property", schedules, refusals)
	}
}

// ===== THE REFUSALS =====

// A PARENT THAT CANNOT BE WORKED INSIDE ITS CAP IS REFUSED WHOLE.
//
// Not trimmed to what fits — that leaves a remainder no child will ever carry, so
// the order never completes — and not spread past the cap on one slice, which is
// the single outcome a participation limit exists to prevent. The refusal names
// the largest parent this window and this cap could have worked, because that is
// the number the desk has to decide about.
func TestPOV_RefusesAParentThatCannotBeWorkedInsideItsCap(t *testing.T) {
	vols := []int64{1000, 2000, 3000} // 6000 expected; 8% of it is 480
	p := povPlan(481, 8, 100, 3)
	mkt := newCurveMarket(p, vols...)

	got, err := Run(p, ParentState{OrderID: "p1"}, mkt)
	if !errors.Is(err, ErrParticipationCapExceeded) {
		t.Fatalf("err = %v, want ErrParticipationCapExceeded", err)
	}
	if got != nil {
		t.Fatalf("returned %d slices alongside the refusal — a refused schedule must produce no "+
			"children, and a partial one is how a parent rests forever", len(got))
	}
	if !strings.Contains(err.Error(), "480") {
		t.Errorf("refusal %q does not name the largest parent this cap could have worked", err)
	}

	// AND ONE UNIT LESS IS ACCEPTED, so the refusal is shown to be the cap rather
	// than something else about the plan.
	ok := povPlan(480, 8, 100, 3)
	if _, err := Run(ok, ParentState{OrderID: "p1"}, newCurveMarket(ok, vols...)); err != nil {
		t.Fatalf("the largest parent the cap permits was refused: %v", err)
	}
}

// POV WITHOUT A CAP IS REFUSED, NEVER WORKED UNCAPPED.
//
// An uncapped POV is not a conservative POV: it is VWAP with a different label,
// and the label is what every downstream reader of the fills will believe. The
// refusal names VWAP so an operator who genuinely wants the proportional schedule
// without a cap can ask for it by name.
func TestPOV_RefusesAPlanWithNoCap(t *testing.T) {
	for _, tt := range []struct {
		name string
		rate *big.Rat
	}{
		{"no cap at all", nil},
		{"a cap of zero", new(big.Rat)},
		{"a negative cap", new(big.Rat).SetFrac64(-1, 10)},
		{"a cap above one whole tape", new(big.Rat).SetFrac64(3, 2)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := povPlan(60, 0, 0, 3)
			p.MaxParticipation = tt.rate

			got, err := Run(p, ParentState{OrderID: "p1"}, newCurveMarket(p, 1000, 2000, 3000))
			if !errors.Is(err, ErrNoParticipationCap) {
				t.Fatalf("err = %v, want ErrNoParticipationCap", err)
			}
			if got != nil {
				t.Errorf("returned %d slices alongside the refusal", len(got))
			}
			if !strings.Contains(err.Error(), string(NameVWAP)) {
				t.Errorf("refusal %q does not name the algorithm that works this order without a "+
					"cap, so an operator cannot tell what to ask for instead", err)
			}
		})
	}
}

// THE CAP IS CHECKED BEFORE THE MARKET IS ASKED ANYTHING. A missing cap is the
// operator's own error and they fix it from the command; telling them instead that
// no volume profile exists sends them to chase a market-data problem they do not
// have. The view here would answer everything, and the refusal must still be the
// cap's.
func TestPOV_TheCapIsCheckedBeforeTheProfile(t *testing.T) {
	p := povPlan(60, 0, 0, 3)

	if _, err := Run(p, ParentState{OrderID: "p1"}, UnknownMarket{}); !errors.Is(err, ErrNoParticipationCap) {
		t.Fatalf("err = %v, want ErrNoParticipationCap ahead of ErrVolumeUnknown", err)
	}
}

// AN UNKNOWN PROFILE REFUSES POV TOO, and for the same reason it refuses VWAP: a
// participation cap against an invented curve is a control measured against a
// number nobody observed.
func TestPOV_AnUnknownProfileRefuses(t *testing.T) {
	p := povPlan(60, 8, 100, 3)

	got, err := Run(p, ParentState{OrderID: "p1"}, UnknownMarket{})
	if !errors.Is(err, ErrVolumeUnknown) {
		t.Fatalf("err = %v, want ErrVolumeUnknown", err)
	}
	if got != nil {
		t.Errorf("returned %d slices alongside the refusal", len(got))
	}
}

// POV AND VWAP ARE THE SAME ALLOCATION WHEN THE CAP DOES NOT BIND — asserted
// rather than left to be discovered, because it is the claim pov.go's doc makes
// and a reader must be able to check it. What differs is ADMISSION: the plan below
// is worked identically by both, and the plan in
// TestVWAP_DoesNotEnforceAParticipationCapItWasNotAskedFor is worked by one and
// refused by the other.
func TestPOV_MatchesVWAPWhenTheCapDoesNotBind(t *testing.T) {
	vols := []int64{100, 300, 600}
	p := povPlan(60, 50, 100, 3) // 60 of 1000 is 6%, far inside a 50% cap

	povSchedule, err := Run(p, ParentState{OrderID: "p1"}, newCurveMarket(p, vols...))
	if err != nil {
		t.Fatalf("POV: %v", err)
	}
	v := p
	v.Algo = NameVWAP
	vwapSchedule, err := Run(v, ParentState{OrderID: "p1"}, newCurveMarket(v, vols...))
	if err != nil {
		t.Fatalf("VWAP: %v", err)
	}
	for i := range povSchedule {
		if povSchedule[i].Quantity.Cmp(vwapSchedule[i].Quantity) != 0 {
			t.Fatalf("slice %d: POV %s, VWAP %s — pov.go's doc claims these coincide when the cap "+
				"does not bind, and a reader relies on that being true",
				i, povSchedule[i].Quantity.RatString(), vwapSchedule[i].Quantity.RatString())
		}
	}
}
