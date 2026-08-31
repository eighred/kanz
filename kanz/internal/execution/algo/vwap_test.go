package algo

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// VWAP: THE ALLOCATION, AND THE REFUSAL THAT MAKES THE LABEL TRUE (#869).
//
// Two properties are asserted here and they are not the same property:
//
//  1. the allocation is EXACTLY proportional to the volume expected in each
//     slice's own interval, and the children still sum to the parent exactly;
//  2. an UNKNOWN profile REFUSES rather than degrading — which is what stops a
//     VWAP order silently becoming a TWAP order, an outcome #866's attribution
//     could not detect because the fills of the two are indistinguishable once
//     they exist.
//
// EVERY EXPECTED QUANTITY BELOW WAS COMPUTED OUTSIDE THIS PACKAGE. A schedule is
// arithmetic, and an expectation read off the implementation asserts only that
// the code does what it does. The values come from an independent evaluation of
// Total·vᵢ/Σv as exact fractions.

var (
	volStart = time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	volEnd   = time.Date(2026, 8, 15, 6, 0, 0, 0, time.UTC)
)

// curveMarket answers a fixed volume per slice interval, keyed by the interval's
// start, and records every question it was asked.
//
// KEYED BY THE INTERVAL, NOT BY A COUNTER, so a test can assert that an algorithm
// asked about the intervals its children are DUE in. A counter-driven fake would
// answer the same sequence to an algorithm that asked about the wrong hours.
type curveMarket struct {
	vols  map[time.Time]*big.Rat
	asked []time.Time
	inst  string
}

// newCurveMarket keys the curve by the PLAN'S OWN slice boundaries.
//
// NOT BY start + i·step, WHICH IS A DIFFERENT SET OF INSTANTS. Plan.boundary
// computes each offset as span·i/N in integer nanoseconds, so for a slice count
// that does not divide the window evenly — 7 slices of six hours — the boundaries
// are not multiples of a truncated step, and a fixture keyed that way answers
// UNKNOWN to a schedule that asked a perfectly good question. That is a fixture
// bug that reads exactly like the refusal under test, which is the most expensive
// kind: it fails a correct implementation and would be "fixed" by loosening the
// refusal.
func newCurveMarket(p Plan, vols ...int64) *curveMarket {
	m := &curveMarket{vols: map[time.Time]*big.Rat{}, inst: "BTC-USDT"}
	for i, v := range vols {
		m.vols[p.boundary(i)] = new(big.Rat).SetInt64(v)
	}
	return m
}

func (m *curveMarket) TopOfBook(string) (*big.Rat, *big.Rat, bool) { return nil, nil, false }

func (m *curveMarket) ExpectedVolume(instrumentID string, from, _ time.Time) (*big.Rat, bool) {
	m.asked = append(m.asked, from.UTC())
	if instrumentID != m.inst {
		return nil, false
	}
	v, ok := m.vols[from.UTC()]
	if !ok {
		return nil, false
	}
	return new(big.Rat).Set(v), true
}

// blindAt answers everything except one interval, which it reports UNKNOWN.
//
// ONE UNKNOWN IN THE MIDDLE OF A KNOWN CURVE is the case that separates "refuses
// when it knows nothing" from "refuses when it cannot size EVERY slice". A
// schedule that skipped the one interval it could not size, or spread it over the
// others, would pass the first and fail the second — and it would be a VWAP order
// worked against a curve partly invented.
type blindAt struct {
	*curveMarket
	blind time.Time
}

func (b blindAt) ExpectedVolume(instrumentID string, from, to time.Time) (*big.Rat, bool) {
	if from.UTC().Equal(b.blind.UTC()) {
		return nil, false
	}
	return b.curveMarket.ExpectedVolume(instrumentID, from, to)
}

func vwapPlan(total int64, slices int) Plan {
	return Plan{
		Algo:         NameVWAP,
		InstrumentID: "BTC-USDT",
		Total:        new(big.Rat).SetInt64(total),
		Start:        volStart,
		End:          volEnd,
		Slices:       slices,
	}
}

// ===== (1) THE ALLOCATION =====

// THE PARENT IS DIVIDED IN PROPORTION TO THE EXPECTED CURVE, EXACTLY.
//
// The expectations are independent: 600 against a curve of 100/200/300/100/50/250
// (which sums to 1000) is 0.6 of each bucket, and 10 against 1/2/3 is 5/3, 10/3, 5
// — a case chosen because two of its three children have no finite decimal
// expansion. A decimal implementation would leave a remainder there and the parent
// would never complete; conservation is asserted against the parent's own total
// rather than against the sum of the expectations, so a schedule that lost the
// remainder in BOTH places still fails.
func TestVWAP_AllocatesInProportionToTheExpectedCurve(t *testing.T) {
	for _, tt := range []struct {
		name  string
		total int64
		vols  []int64
		want  []string // exact rationals, computed independently
	}{
		{
			name:  "six hourly buckets",
			total: 600,
			vols:  []int64{100, 200, 300, 100, 50, 250},
			want:  []string{"60", "120", "180", "60", "30", "150"},
		},
		{
			name:  "a division with no decimal expansion",
			total: 10,
			vols:  []int64{1, 2, 3},
			want:  []string{"5/3", "10/3", "5"},
		},
		{
			name:  "the whole parent lands in the one busy bucket's share",
			total: 100,
			vols:  []int64{1, 1, 1, 97},
			want:  []string{"1", "1", "1", "97"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := vwapPlan(tt.total, len(tt.vols))
			mkt := newCurveMarket(p, tt.vols...)

			got, err := Run(p, ParentState{OrderID: "p1"}, mkt)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d slices, want %d", len(got), len(tt.want))
			}
			for i, want := range tt.want {
				w, _ := new(big.Rat).SetString(want)
				if got[i].Quantity.Cmp(w) != 0 {
					t.Errorf("slice %d = %s, want %s — the allocation is not the expected curve's "+
						"share of the parent", i, got[i].Quantity.RatString(), w.RatString())
				}
				// AND IT IS DUE WHERE TWAP WOULD HAVE PUT IT. VWAP changes the SIZE
				// of a child, not when it is sent; a schedule that also moved the
				// due times would have been sized for an interval it is not worked
				// in.
				if want := p.boundary(i); !got[i].Due.Equal(want) {
					t.Errorf("slice %d due %s, want %s", i, got[i].Due, want)
				}
			}
			if Sum(got).Cmp(p.Total) != 0 {
				t.Errorf("children sum to %s, parent is %s — a remainder nobody works leaves the "+
					"parent resting forever", Sum(got).RatString(), p.Total.RatString())
			}
		})
	}
}

// IT ASKS ABOUT THE INTERVAL EACH CHILD IS WORKED IN, not about the window as a
// whole and not about the wrong hours. A schedule sized against one number for the
// whole window is TWAP; a schedule sized against the wrong intervals is worse than
// TWAP, because it is confidently misaligned.
func TestVWAP_SizesEachSliceAgainstItsOwnInterval(t *testing.T) {
	p := vwapPlan(600, 6)
	mkt := newCurveMarket(p, 100, 200, 300, 100, 50, 250)

	if _, err := Run(p, ParentState{OrderID: "p1"}, mkt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(mkt.asked) != 6 {
		t.Fatalf("asked the view %d times for a 6-slice plan: %v", len(mkt.asked), mkt.asked)
	}
	for i, at := range mkt.asked {
		if want := p.boundary(i); !at.Equal(want) {
			t.Errorf("question %d was about %s, want %s", i, at, want)
		}
	}
}

// ===== (2) THE REFUSAL — #869'S NON-NEGOTIABLE PROPERTY =====

// AN UNKNOWN PROFILE REFUSES. IT DOES NOT DEGRADE TO TWAP.
//
// This is the assertion the whole issue turns on. The comparison against TWAP is
// explicit rather than implied: the test derives what TWAP would have produced for
// the same plan and fails if VWAP returned it, because "returned an error" and
// "did not silently return the TWAP schedule" are different claims and only the
// second one is the property.
func TestVWAP_AnUnknownProfileRefusesRatherThanDegradingToTWAP(t *testing.T) {
	p := vwapPlan(600, 6)

	twapSchedule, err := TWAP(p)
	if err != nil {
		t.Fatalf("TWAP oracle: %v", err)
	}

	for _, tt := range []struct {
		name string
		mkt  MarketView
	}{
		{"a view that knows nothing", UnknownMarket{}},
		{"no view at all", nil},
		{
			name: "one interval of six is UNKNOWN",
			mkt: blindAt{
				curveMarket: newCurveMarket(p, 100, 200, 300, 100, 50, 250),
				blind:       volStart.Add(3 * time.Hour),
			},
		},
		{
			name: "the view knows a different instrument",
			mkt:  &curveMarket{vols: map[time.Time]*big.Rat{}, inst: "ETH-USDT"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Run(p, ParentState{OrderID: "p1"}, tt.mkt)
			if !errors.Is(err, ErrVolumeUnknown) {
				t.Fatalf("err = %v, want ErrVolumeUnknown", err)
			}
			if got != nil {
				t.Fatalf("VWAP returned %d slices alongside its refusal — a refused schedule must "+
					"produce no children", len(got))
			}
			// THE DEGRADATION, NAMED. If VWAP ever answers a schedule here, this is
			// the one it would answer: equal quantity at equal intervals. An order
			// labelled VWAP that this platform worked as TWAP is a mislabelled
			// execution, and every downstream reader — attribution, TCA, the
			// operator explaining the day — would be reading the wrong algorithm.
			if len(got) == len(twapSchedule) && len(got) > 0 &&
				got[0].Quantity.Cmp(twapSchedule[0].Quantity) == 0 {
				t.Fatal("VWAP degraded to the TWAP schedule on an UNKNOWN profile")
			}
		})
	}
}

// A VOLUME-DRIVEN PLAN THAT NAMES NO INSTRUMENT IS REFUSED, and under its own
// sentinel rather than as an unknown profile: a view asked about "" is being asked
// about nothing, and whatever it answered would size real children.
func TestVWAP_RefusesAPlanThatNamesNoInstrument(t *testing.T) {
	p := vwapPlan(600, 6)
	p.InstrumentID = ""
	mkt := newCurveMarket(p, 100, 200, 300, 100, 50, 250)

	if _, err := Run(p, ParentState{OrderID: "p1"}, mkt); !errors.Is(err, ErrNoInstrument) {
		t.Fatalf("err = %v, want ErrNoInstrument", err)
	}
}

// A SLICE NOTHING IS EXPECTED TO TRADE IN REFUSES THE WHOLE PLAN.
//
// The alternatives both leave a parent resting forever: a child of zero quantity
// is not placeable at any venue this platform reaches, and a schedule that omits
// that index can never satisfy services/oms/internal/schedule.Complete, which asks
// whether every index in [0, Slices) exists. So the plan is refused whole, and the
// refusal names the slice so an operator can move the window or cut the slice
// count.
func TestVWAP_RefusesASliceNothingIsExpectedToTradeIn(t *testing.T) {
	p := vwapPlan(600, 4)
	mkt := newCurveMarket(p, 100, 0, 300, 200)

	got, err := Run(p, ParentState{OrderID: "p1"}, mkt)
	if !errors.Is(err, ErrNoVolumeInBucket) {
		t.Fatalf("err = %v, want ErrNoVolumeInBucket", err)
	}
	if got != nil {
		t.Errorf("returned %d slices alongside the refusal", len(got))
	}
	if !strings.Contains(err.Error(), "slice 1 of 4") {
		t.Errorf("refusal %q does not name which slice could not be sized", err)
	}
}

// THE SIZE CAP STILL APPLIES, and it refuses the plan WHOLE. VWAP concentrates
// quantity into the busy intervals by design, so a parent that is comfortable
// under an even split can breach a per-child cap under a volume-weighted one —
// which is precisely the case an operator needs told rather than trimmed.
func TestVWAP_HonoursTheSizeCapAndRefusesWhole(t *testing.T) {
	p := vwapPlan(600, 4)
	p.MaxSlice = new(big.Rat).SetInt64(200)      // an even split would be 150 each
	mkt := newCurveMarket(p, 100, 100, 100, 700) // 700/1000 ⇒ 420

	if _, err := Run(p, ParentState{OrderID: "p1"}, mkt); !errors.Is(err, ErrCapUnsatisfiable) {
		t.Fatalf("err = %v, want ErrCapUnsatisfiable", err)
	}
	if twapOK, err := TWAP(p); err != nil || len(twapOK) != 4 {
		t.Fatalf("the same plan must be workable as TWAP or this test proves nothing about the "+
			"volume weighting: %d slices, err %v", len(twapOK), err)
	}
}

// A VWAP PLAN CARRYING A PARTICIPATION CAP IS NOT QUIETLY CAPPED.
//
// VWAP does not read Plan.MaxParticipation. An algorithm that silently enforced a
// cap the operator did not select by naming POV would be a second control nobody
// could see — and one that could not be found by reading the order, because the
// order would say VWAP.
func TestVWAP_DoesNotEnforceAParticipationCapItWasNotAskedFor(t *testing.T) {
	p := vwapPlan(600, 3)
	p.MaxParticipation = new(big.Rat).SetFrac64(1, 100) // 1%, far below 600/1000
	mkt := newCurveMarket(p, 100, 300, 600)

	got, err := Run(p, ParentState{OrderID: "p1"}, mkt)
	if err != nil {
		t.Fatalf("VWAP refused under a cap it does not claim to enforce: %v", err)
	}
	if Sum(got).Cmp(p.Total) != 0 {
		t.Errorf("children sum to %s, parent is %s", Sum(got).RatString(), p.Total.RatString())
	}
	// AND POV REFUSES THE SAME PLAN, which is what makes the two labels mean
	// different things rather than being two names for one schedule.
	pov := p
	pov.Algo = NamePOV
	if _, err := Run(pov, ParentState{OrderID: "p1"}, mkt); !errors.Is(err, ErrParticipationCapExceeded) {
		t.Fatalf("POV err = %v, want ErrParticipationCapExceeded — if both algorithms accept this "+
			"plan the cap is not a control", err)
	}
}

// VWAP IS DETERMINISTIC: the same plan and the same view produce the same
// schedule, which is what makes a parent recoverable by a pod that has just
// booted. Asserted here because the property is the reason this package may not
// hold state, and a fake view is the one place a test can vary nothing at all.
func TestVWAP_IsDeterministic(t *testing.T) {
	p := vwapPlan(600, 6)
	first, err := Run(p, ParentState{OrderID: "p1"}, newCurveMarket(p, 100, 200, 300, 100, 50, 250))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	second, err := Run(p, ParentState{OrderID: "p1"}, newCurveMarket(p, 100, 200, 300, 100, 50, 250))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i := range first {
		if first[i].Quantity.Cmp(second[i].Quantity) != 0 || !first[i].Due.Equal(second[i].Due) {
			t.Fatalf("slice %d differs between two derivations of the same plan", i)
		}
	}
}
