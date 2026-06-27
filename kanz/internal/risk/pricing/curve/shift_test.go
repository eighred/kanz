package curve

import "testing"

func newFlat(t *testing.T) *Curve {
	t.Helper()
	c, err := NewZeroCurve([]float64{1, 5, 10}, []float64{0.04, 0.04, 0.04}, Continuous, LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParallelShift(t *testing.T) {
	c := newFlat(t)
	s := Parallel{Bp: 50}.Apply(c)
	for _, ten := range []float64{1, 5, 10} {
		approx(t, "parallel +50bp", s.Zero(ten), 0.04+0.0050, 1e-12)
	}
	// Base curve unmutated.
	approx(t, "base unchanged", c.Zero(5), 0.04, 1e-12)
}

func TestSteepener(t *testing.T) {
	c := newFlat(t)
	s := Steepener{ShortBp: -25, LongBp: 25}.Apply(c)
	// Short end down, long end up, belly ~unchanged (linear in tenor).
	if !(s.Zero(1) < c.Zero(1)) {
		t.Fatalf("short end should fall: %.6f vs %.6f", s.Zero(1), c.Zero(1))
	}
	if !(s.Zero(10) > c.Zero(10)) {
		t.Fatalf("long end should rise: %.6f vs %.6f", s.Zero(10), c.Zero(10))
	}
	approx(t, "short −25bp", s.Zero(1), 0.04-0.0025, 1e-12)
	approx(t, "long +25bp", s.Zero(10), 0.04+0.0025, 1e-12)
}

func TestButterfly(t *testing.T) {
	c := newFlat(t)
	s := Butterfly{BellyBp: 30}.Apply(c)
	// Belly (mid pillar, tenor 5 — close to the midpoint 5.5) up; wings down.
	if !(s.Zero(5) > c.Zero(5)) {
		t.Fatalf("belly should rise: %.6f vs %.6f", s.Zero(5), c.Zero(5))
	}
	if !(s.Zero(1) < c.Zero(1)) || !(s.Zero(10) < c.Zero(10)) {
		t.Fatalf("wings should fall: short %.6f long %.6f base %.6f", s.Zero(1), s.Zero(10), c.Zero(1))
	}
}

func TestWithPillarBump_IsLocal(t *testing.T) {
	c := newFlat(t) // pillars at 1,5,10
	b := c.WithPillarBump(1, 1e-4)
	// Bumped pillar moves; the endpoints (flat-extrapolated past their own
	// pillars) are unaffected at their own tenors.
	approx(t, "bumped pillar", b.Zero(5), 0.04+1e-4, 1e-12)
	approx(t, "pillar 0 untouched", b.Zero(1), 0.04, 1e-12)
	approx(t, "pillar 2 untouched", b.Zero(10), 0.04, 1e-12)
}
