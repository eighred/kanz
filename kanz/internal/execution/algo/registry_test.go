package algo

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// THE SEAM, AND THE PROOF THAT PUTTING TWAP BEHIND IT CHANGED NOTHING (#868).
//
// twap_test.go asserts what TWAP computes. This file asserts three different
// things:
//
//  1. that reaching TWAP through the registry produces byte-identical schedules
//     and identical refusals to calling it directly — the refactor's whole claim;
//  2. that an algorithm nobody implements is REFUSED rather than defaulted, which
//     is what stops a mislabelled execution corrupting order attribution;
//  3. that MarketView can answer UNKNOWN and that an algorithm can refuse on it,
//     which is the property #867's volume profile needs and the reason the
//     interface returns a `known bool` instead of a zero.

var (
	seamStart = time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC)
	seamEnd   = time.Date(2026, 8, 15, 15, 0, 0, 0, time.UTC)
)

// seamPlans is the matrix the equivalence proof runs over. It deliberately
// includes the REFUSALS as well as the schedules: an equivalence that only
// covered the happy path would not notice a seam that swallowed
// ErrCapUnsatisfiable, and a swallowed refusal is a parent that rests forever.
func seamPlans() []struct {
	name string
	plan Plan
} {
	rat := func(i int64) *big.Rat { return new(big.Rat).SetInt64(i) }
	return []struct {
		name string
		plan Plan
	}{
		{"even division", Plan{Total: rat(60), Start: seamStart, End: seamEnd, Slices: 6}},
		{"inexact division", Plan{Total: rat(10), Start: seamStart, End: seamEnd, Slices: 3}},
		{"one slice", Plan{Total: rat(7), Start: seamStart, End: seamEnd, Slices: 1}},
		{"many slices", Plan{Total: rat(1000), Start: seamStart, End: seamEnd, Slices: 997}},
		{"cap honoured", Plan{Total: rat(100), Start: seamStart, End: seamEnd, Slices: 10, MaxSlice: rat(10)}},
		{"cap unsatisfiable", Plan{Total: rat(1000), Start: seamStart, End: seamEnd, Slices: 10, MaxSlice: rat(10)}},
		{"empty quantity", Plan{Total: rat(0), Start: seamStart, End: seamEnd, Slices: 6}},
		{"nil quantity", Plan{Start: seamStart, End: seamEnd, Slices: 6}},
		{"no slices", Plan{Total: rat(60), Start: seamStart, End: seamEnd}},
		{"zero-length window", Plan{Total: rat(60), Start: seamStart, End: seamStart, Slices: 6}},
		{"backwards window", Plan{Total: rat(60), Start: seamEnd, End: seamStart, Slices: 6}},
	}
}

// ===== (1) THE REFACTOR'S CLAIM =====

// TWAP THROUGH THE SEAM IS THE SAME SCHEDULE, SLICE FOR SLICE.
//
// #868 is a refactor and its entire risk is that the parent orders already
// working on this platform start being sliced differently. So the oracle is the
// function that was there before: for every plan shape, including every shape it
// refuses, Run must produce what TWAP produces — same indices, same due times to
// the nanosecond, same quantities as exact rationals, same error text.
//
// Quantities are compared with Cmp rather than by their decimal rendering: a
// slice of 10/3 is 10/3, and two schedules that both print 3.333333 can still
// differ in the remainder that decides whether the parent ever completes.
func TestAlgoRegistry_TWAPThroughTheSeamIsTheSameSchedule(t *testing.T) {
	for _, tt := range seamPlans() {
		t.Run(tt.name, func(t *testing.T) {
			direct, derr := TWAP(tt.plan)

			viaSeam := tt.plan
			viaSeam.Algo = NameTWAP
			seam, serr := Run(viaSeam, ParentState{OrderID: "p1"}, UnknownMarket{})

			switch {
			case derr == nil && serr != nil:
				t.Fatalf("TWAP accepted this plan and the seam refused it: %v — the registry has "+
					"changed which orders can be worked", serr)
			case derr != nil && serr == nil:
				t.Fatalf("TWAP refused this plan (%v) and the seam accepted it — a refusal the "+
					"platform relied on has been swallowed", derr)
			case derr != nil:
				if derr.Error() != serr.Error() {
					t.Fatalf("refusals differ:\n  direct: %v\n    seam: %v\nan operator reads this "+
						"text to learn what to change", derr, serr)
				}
				return
			}

			if len(seam) != len(direct) {
				t.Fatalf("seam produced %d slices, TWAP produced %d", len(seam), len(direct))
			}
			for i := range direct {
				switch {
				case seam[i].Index != direct[i].Index:
					t.Errorf("slice %d: index %d via the seam, %d direct", i, seam[i].Index, direct[i].Index)
				case !seam[i].Due.Equal(direct[i].Due):
					t.Errorf("slice %d: due %s via the seam, %s direct", i,
						seam[i].Due.Format(time.RFC3339Nano), direct[i].Due.Format(time.RFC3339Nano))
				case seam[i].Quantity.Cmp(direct[i].Quantity) != 0:
					t.Errorf("slice %d: quantity %s via the seam, %s direct — the children no longer "+
						"divide the parent the way they did", i,
						seam[i].Quantity.RatString(), direct[i].Quantity.RatString())
				}
			}
			// AND CONSERVATION SURVIVES THE SEAM, asserted here as well as inside
			// TWAP's own tests: the equivalence above would pass if BOTH sides
			// lost the same remainder, and this is the property #435 named first.
			if Sum(seam).Cmp(tt.plan.Total) != 0 {
				t.Errorf("children through the seam sum to %s, parent is %s",
					Sum(seam).RatString(), tt.plan.Total.RatString())
			}
		})
	}
}

// ===== (2) AN UNKNOWN NAME IS REFUSED, NEVER DEFAULTED =====

// AN ALGORITHM THIS BUILD DOES NOT IMPLEMENT IS REFUSED UNDER ITS OWN SENTINEL.
//
// Defaulting to TWAP is the failure this whole seam exists to prevent, and it is
// worse than it looks: the order would be worked, the fills would arrive, and the
// execution record would say VWAP. Every downstream reader — attribution, TCA,
// the operator explaining the day — would be reading a label for an algorithm
// that never ran.
func TestAlgoRegistry_AnUnknownNameIsRefusedNotDefaulted(t *testing.T) {
	base := Plan{Total: new(big.Rat).SetInt64(60), Start: seamStart, End: seamEnd, Slices: 6}

	for _, name := range []Name{"VWAP", "POV", "IS", "twap", "TWAP ", "", "EXECUTION_ALGO_TWAP"} {
		t.Run(string("name="+name), func(t *testing.T) {
			p := base
			p.Algo = name

			slices, err := Run(p, ParentState{OrderID: "p1"}, UnknownMarket{})
			if !errors.Is(err, ErrUnknownAlgo) {
				t.Fatalf("Run(%q) err = %v, want ErrUnknownAlgo — an algorithm nobody implements "+
					"must be refused, not worked as TWAP under another name", name, err)
			}
			if slices != nil {
				t.Errorf("Run(%q) returned %d slices alongside its refusal — a refused schedule "+
					"must produce no children", name, len(slices))
			}
			// THE REFUSAL MUST NAME WHAT THIS BUILD DOES IMPLEMENT. An operator on
			// a rolling deploy needs to know whether they asked for something that
			// does not exist or something this pod has not shipped yet.
			if !strings.Contains(err.Error(), string(NameTWAP)) {
				t.Errorf("refusal %q does not say which algorithms this build implements", err)
			}
		})
	}
}

// AN UNKNOWN NAME IS REFUSED BEFORE THE ARITHMETIC, not after it. A plan that is
// unworkable AND names an unimplemented algorithm must come back as the naming
// refusal: fixing the window would otherwise reveal a second refusal the operator
// was never told about, and each round trip costs them a market.
func TestAlgoRegistry_TheNameIsCheckedBeforeThePlan(t *testing.T) {
	p := Plan{Algo: "VWAP", Total: new(big.Rat).SetInt64(60), Start: seamEnd, End: seamStart, Slices: 0}

	_, err := Run(p, ParentState{OrderID: "p1"}, UnknownMarket{})
	if !errors.Is(err, ErrUnknownAlgo) {
		t.Fatalf("err = %v, want ErrUnknownAlgo first", err)
	}
}

// EVERY REGISTERED ALGORITHM ANSWERS TO THE NAME IT IS REGISTERED UNDER, and no
// two share one. A registry whose entry disagrees with its own Name() is
// unreachable — Lookup walks Registered comparing Name() — so this is what stops
// an algorithm being shipped that no order can ever select.
func TestAlgoRegistry_EveryEntryIsReachableByItsOwnName(t *testing.T) {
	all := Registered()
	if len(all) == 0 {
		t.Fatal("Registered() is empty — this build can work no order at all, and every test " +
			"below would pass vacuously")
	}
	seen := map[Name]bool{}
	for _, a := range all {
		got, err := Lookup(a.Name())
		if err != nil {
			t.Fatalf("Lookup(%q) failed for a registered algorithm: %v", a.Name(), err)
		}
		if got.Name() != a.Name() {
			t.Errorf("Lookup(%q) returned %q", a.Name(), got.Name())
		}
		if seen[a.Name()] {
			t.Errorf("two registered algorithms answer to %q — Lookup returns the first, so the "+
				"second is unreachable and the order that names it is worked by the wrong one",
				a.Name())
		}
		seen[a.Name()] = true
	}
	if !seen[NameTWAP] {
		t.Errorf("TWAP is not registered — every scheduled parent order on this platform names it")
	}
}

// ===== (3) UNKNOWN IS A THIRD VALUE =====

// UnknownMarket ANSWERS UNKNOWN, NOT ZERO.
//
// The distinction is the invariant, not a nicety: a view that returned
// (0, true) for expected volume would tell a participation algorithm that nothing
// will trade, and the algorithm would work the whole parent immediately or not at
// all. Either is a schedule nobody chose.
func TestAlgoMarketView_UnknownMarketAnswersUnknownNotZero(t *testing.T) {
	var mkt MarketView = UnknownMarket{}

	if bid, ask, known := mkt.TopOfBook("BTC-USDT"); known {
		t.Errorf("TopOfBook claimed to know: bid=%v ask=%v", bid, ask)
	} else if bid != nil || ask != nil {
		t.Errorf("TopOfBook answered UNKNOWN but returned values (bid=%v ask=%v) a caller "+
			"ignoring the flag would read as a book", bid, ask)
	}

	if qty, known := mkt.ExpectedVolume("BTC-USDT", seamStart, seamEnd); known {
		t.Errorf("ExpectedVolume claimed to know: %v", qty)
	} else if qty != nil {
		t.Errorf("ExpectedVolume answered UNKNOWN but returned %v — a zero here reads as "+
			"\"nothing will trade\", which is an assertion this view cannot make", qty)
	}
}

// volumeHungry is a stand-in for the algorithms #869 will add: one that cannot
// derive a schedule without a volume profile. It is here to prove the SEAM can
// carry a refusal, not to be an algorithm — it is defined in a test file and is
// therefore not registered, which is exactly what
// test/arch/every_algo_is_reachable_test.go tolerates for tests and refuses for
// production code.
type volumeHungry struct{}

func (volumeHungry) Name() Name { return "TEST_VOLUME_HUNGRY" }

func (volumeHungry) Schedule(p Plan, st ParentState, mkt MarketView) ([]Slice, error) {
	if _, known := mkt.ExpectedVolume("BTC-USDT", p.Start, p.End); !known {
		return nil, errors.New("volume profile unknown")
	}
	if st.Sent == nil {
		return nil, errors.New("what has been sent is unknown")
	}
	return TWAP(p)
}

// AN ALGORITHM CAN REFUSE RATHER THAN DEGRADE, on both unknowns the seam carries.
//
// #867's volume profile can be absent or stale, and #868's job is to make sure
// that arrives as a refusal rather than as a zero. This asserts the shape works:
// an algorithm asked to schedule against a view that knows nothing gets a false,
// and an algorithm that cannot establish what has already been sent gets a nil
// rather than a fabricated "nothing yet".
func TestAlgoMarketView_AnAlgoCanRefuseRatherThanDegrade(t *testing.T) {
	p := Plan{Total: new(big.Rat).SetInt64(60), Start: seamStart, End: seamEnd, Slices: 6}

	if _, err := (volumeHungry{}).Schedule(p, ParentState{Sent: func(int) bool { return false }}, UnknownMarket{}); err == nil {
		t.Error("an algorithm needing a volume profile scheduled against a view that knows " +
			"nothing — UNKNOWN degraded into a number somewhere")
	}
	if _, err := (volumeHungry{}).Schedule(p, ParentState{}, knownMarket{}); err == nil {
		t.Error("an algorithm needing to know what was sent scheduled against a nil Sent — nil " +
			"was read as \"nothing was sent\", which would re-derive past everything already in " +
			"front of a venue")
	}
	if _, err := (volumeHungry{}).Schedule(p, ParentState{Sent: func(int) bool { return false }}, knownMarket{}); err != nil {
		t.Errorf("refused with both answers known: %v — the refusal is unconditional, so it "+
			"proves nothing about UNKNOWN", err)
	}
}

// knownMarket answers everything, so the refusal above is shown to be about the
// UNKNOWN rather than about the algorithm refusing whatever it is handed.
type knownMarket struct{}

func (knownMarket) TopOfBook(string) (*big.Rat, *big.Rat, bool) {
	return new(big.Rat).SetInt64(1), new(big.Rat).SetInt64(2), true
}

func (knownMarket) ExpectedVolume(string, time.Time, time.Time) (*big.Rat, bool) {
	return new(big.Rat).SetInt64(1000), true
}

// A NIL MARKET VIEW IS UNKNOWN, NOT A PANIC. A caller that passes nothing has
// asserted nothing, and an algorithm that needs data must refuse — which is the
// same outcome as UnknownMarket and the opposite of a pod crashing on the first
// scheduled parent it is handed.
func TestAlgoRun_ANilMarketViewIsUnknownNotAPanic(t *testing.T) {
	p := Plan{Algo: NameTWAP, Total: new(big.Rat).SetInt64(60), Start: seamStart, End: seamEnd, Slices: 6}

	slices, err := Run(p, ParentState{OrderID: "p1"}, nil)
	if err != nil {
		t.Fatalf("Run with a nil market view: %v", err)
	}
	if len(slices) != 6 {
		t.Errorf("got %d slices, want 6", len(slices))
	}
}
