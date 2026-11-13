package algo

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

// UnixNano and time.Sub both lose the very range this regression exercises.
// Build the independent oracle from wide seconds plus nanoseconds instead.
func windowInstantNS(at time.Time) *big.Int {
	n := new(big.Int).Mul(big.NewInt(at.Unix()), big.NewInt(1_000_000_000))
	return n.Add(n, big.NewInt(int64(at.Nanosecond())))
}

func windowInstantAfter(start time.Time, span *big.Int) time.Time {
	n := new(big.Int).Add(windowInstantNS(start), span)
	seconds, nanos := new(big.Int), new(big.Int)
	seconds.QuoRem(n, big.NewInt(1_000_000_000), nanos)
	return time.Unix(seconds.Int64(), nanos.Int64()).UTC()
}

func windowMaxNS() *big.Int {
	return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 63), big.NewInt(1))
}

func windowPlan(name Name, start, end time.Time) Plan {
	return Plan{Algo: name, InstrumentID: "BTC-USDT", Total: big.NewRat(21, 1),
		Start: start, End: end, Slices: 3, MaxParticipation: big.NewRat(1, 10)}
}

type windowMarket struct {
	UnknownMarket
	calls int
}

func (m *windowMarket) ExpectedVolume(string, time.Time, time.Time) (*big.Rat, bool) {
	m.calls++
	return big.NewRat(100, 1), true
}

func TestRegisteredAlgosRefuseSaturatedWindow(t *testing.T) {
	start := time.Date(2000, 1, 1, 0, 0, 0, 123, time.UTC)
	for _, implementation := range Registered() {
		t.Run(string(implementation.Name()), func(t *testing.T) {
			for _, tc := range []struct {
				name string
				end  time.Time
			}{
				{"one_nanosecond_over", windowInstantAfter(start, new(big.Int).Add(windowMaxNS(), big.NewInt(1)))},
				{"four_centuries", time.Date(2400, 1, 1, 0, 0, 0, 123, time.UTC)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					span := new(big.Int).Sub(windowInstantNS(tc.end), windowInstantNS(start))
					if span.Cmp(windowMaxNS()) <= 0 {
						t.Fatal("oracle fixture does not exceed the duration domain")
					}
					p := windowPlan(implementation.Name(), start, tc.end)
					market := &windowMarket{}
					children, err := Run(p, ParentState{OrderID: "wide-window"}, market)
					if !errors.Is(err, ErrWindowTooLong) || len(children) != 0 {
						t.Fatalf("unrepresentable window produced %d children, error %v", len(children), err)
					}
					if market.calls != 0 {
						t.Fatal("window refusal happened after a market read")
					}
				})
			}
		})
	}
}

func TestRegisteredAlgosPreserveRepresentableWindow(t *testing.T) {
	for _, start := range []time.Time{
		time.Date(1800, 2, 3, 4, 5, 6, 987, time.UTC),
		time.Date(2000, 1, 1, 0, 0, 0, 123, time.FixedZone("plus-three", 3*60*60)),
	} {
		for _, implementation := range Registered() {
			t.Run(start.Format("2006")+"/"+string(implementation.Name()), func(t *testing.T) {
				for _, tc := range []struct {
					name string
					span *big.Int
				}{
					{"ordinary", big.NewInt(int64(time.Hour) + 7)},
					{"just_within", new(big.Int).Sub(windowMaxNS(), big.NewInt(1))},
					{"exact_maximum", windowMaxNS()},
				} {
					t.Run(tc.name, func(t *testing.T) {
						end := windowInstantAfter(start, tc.span).In(time.FixedZone("minus-seven", -7*60*60))
						p := windowPlan(implementation.Name(), start, end)
						market := &windowMarket{}
						children, err := Run(p, ParentState{OrderID: "representable-window"}, market)
						if err != nil || len(children) != p.Slices {
							t.Fatalf("representable window produced %d children, error %v", len(children), err)
						}
						for i, child := range children {
							from, to, ok := p.Interval(i)
							if !ok || !from.Equal(child.Due) || child.Quantity.Cmp(big.NewRat(7, 1)) != 0 {
								t.Fatalf("child %d lost its interval or exact quantity", i)
							}
							for _, point := range []struct {
								index int
								at    time.Time
							}{{i, from}, {i + 1, to}} {
								// Multiply/divide in independent wide integers, never
								// using Duration, Sub, UnixNano or the production boundary.
								want := new(big.Int).Mul(tc.span, big.NewInt(int64(point.index)))
								want.Quo(want, big.NewInt(int64(p.Slices)))
								want.Add(want, windowInstantNS(start))
								if windowInstantNS(point.at).Cmp(want) != 0 || point.at.Location() != time.UTC {
									t.Fatalf("boundary %d = %v, want UTC epoch nanoseconds %s", point.index, point.at, want)
								}
							}
						}
						_, last, ok := p.Interval(p.Slices - 1)
						if !ok || !last.Equal(end) {
							t.Fatal("last interval did not end at the exact supplied instant")
						}
					})
				}
			})
		}
	}
}

func TestCheckedWindowPreservesNonForwardRefusal(t *testing.T) {
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, end := range []time.Time{start, start.Add(-time.Nanosecond)} {
		p := windowPlan(NameTWAP, start, end)
		children, err := Run(p, ParentState{}, &windowMarket{})
		if !errors.Is(err, ErrEmptyWindow) || len(children) != 0 {
			t.Error("non-forward window lost its existing refusal identity")
		}
		if from, to, ok := p.Interval(0); ok || !from.IsZero() || !to.IsZero() {
			t.Error("non-forward window acquired a measurable interval")
		}
	}
}

func TestPlanIntervalRefusesSaturatedWindow(t *testing.T) {
	start := time.Date(2000, 1, 1, 0, 0, 0, 123, time.UTC)
	p := windowPlan(NameTWAP, start, windowInstantAfter(start, new(big.Int).Add(windowMaxNS(), big.NewInt(1))))
	for i := range p.Slices {
		from, to, ok := p.Interval(i)
		if ok || !from.IsZero() || !to.IsZero() {
			t.Errorf("slice %d exposed a truncated interval: %v to %v, ok=%v", i, from, to, ok)
		}
	}
}
