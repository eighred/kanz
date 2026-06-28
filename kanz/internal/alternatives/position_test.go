package alternatives

import (
	"math/big"
	"testing"
	"time"
)

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad rat " + s)
	}
	return r
}

func day(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func ev(id string, t EventType, amt string, date time.Time) *Event {
	return &Event{EventID: id, CommitmentID: "C1", Type: t, Amount: rat(amt), Date: date}
}

func samplePosition() *Position {
	return Replay("C1", []*Event{
		ev("e1", EventCommit, "1000", day(2020, 1, 1)),
		ev("e2", EventCall, "300", day(2020, 3, 1)),
		ev("e3", EventCall, "200", day(2020, 9, 1)),
		ev("e4", EventDistribution, "150", day(2021, 6, 1)),
		ev("e5", EventNAVMark, "500", day(2022, 1, 1)),
	})
}

func TestUncalledCommitmentAccounting(t *testing.T) {
	p := samplePosition()
	if p.Committed.Cmp(big.NewRat(1000, 1)) != 0 {
		t.Fatalf("committed: %s", p.Committed.RatString())
	}
	if p.Called.Cmp(big.NewRat(500, 1)) != 0 {
		t.Fatalf("called: want 500 got %s", p.Called.RatString())
	}
	// uncalled = committed - called = 1000 - 500 = 500.
	if p.Uncalled().Cmp(big.NewRat(500, 1)) != 0 {
		t.Fatalf("uncalled: want 500 got %s", p.Uncalled().RatString())
	}
	if p.Distributed.Cmp(big.NewRat(150, 1)) != 0 {
		t.Fatalf("distributed: %s", p.Distributed.RatString())
	}
	if p.NAV.Cmp(big.NewRat(500, 1)) != 0 {
		t.Fatalf("nav: %s", p.NAV.RatString())
	}
}

func TestUncalledFlooredAtZero(t *testing.T) {
	p := Replay("C1", []*Event{
		ev("e1", EventCommit, "100", day(2020, 1, 1)),
		ev("e2", EventCall, "150", day(2020, 3, 1)), // over-call beyond commitment
	})
	if p.Uncalled().Sign() != 0 {
		t.Fatalf("over-called uncalled should floor at 0, got %s", p.Uncalled().RatString())
	}
}

func TestReplayIdempotentAndDeterministic(t *testing.T) {
	events := []*Event{
		ev("e1", EventCommit, "1000", day(2020, 1, 1)),
		ev("e2", EventCall, "300", day(2020, 3, 1)),
		ev("e1", EventCommit, "1000", day(2020, 1, 1)), // duplicate id ⇒ ignored
	}
	p := Replay("C1", events)
	if p.Committed.Cmp(big.NewRat(1000, 1)) != 0 {
		t.Fatalf("duplicate event double-counted: committed=%s", p.Committed.RatString())
	}
	// Order-independent: shuffled input folds to the same called amount.
	shuffled := []*Event{events[1], events[0]}
	if Replay("C1", shuffled).Called.Cmp(p.Called) != 0 {
		t.Fatal("replay not order-independent")
	}
}

func TestJCurveDipsThenRecovers(t *testing.T) {
	p := samplePosition()
	curve := p.JCurve()
	if len(curve) != 3 { // two calls + one distribution
		t.Fatalf("j-curve points: want 3 got %d", len(curve))
	}
	// After the two calls: cumulative = -500 (deepest dip).
	if curve[1].Cumulative != -500 {
		t.Fatalf("j-curve trough: want -500 got %v", curve[1].Cumulative)
	}
	// After the distribution: recovers to -350.
	if curve[2].Cumulative != -350 {
		t.Fatalf("j-curve recovery: want -350 got %v", curve[2].Cumulative)
	}
}
