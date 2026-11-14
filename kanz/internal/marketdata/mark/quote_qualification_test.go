package mark_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/mark"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func unusableLiveQuotes() []struct {
	name     string
	bid, ask *commonpb.Decimal
} {
	return []struct {
		name     string
		bid, ask *commonpb.Decimal
	}{
		{"negative-bid", dec(-100, 0), dec(110, 0)},
		{"negative-ask", dec(110, 0), dec(-100, 0)},
		{"zero-bid", dec(0, 0), dec(2, 0)},
		{"zero-ask", dec(2, 0), dec(0, 0)},
		{"crossed", dec(2, 0), dec(1, 0)},
		{"bid-exponent-high", dec(1, 65), dec(2, 0)},
		{"bid-exponent-low", dec(1, -65), dec(2, 0)},
		{"ask-exponent-high", dec(1, 0), dec(2, 65)},
		{"ask-exponent-low", dec(1, 0), dec(2, -65)},
		{"both-negative", dec(-2, 0), dec(-1, 0)},
		{"both-zero", dec(0, 0), dec(0, 0)},
	}
}

func TestLiveQuoteQualificationPreservesPublicState(t *testing.T) {
	for _, tc := range unusableLiveQuotes() {
		for _, state := range []string{"cold", "warm"} {
			t.Run(tc.name+"-"+state, func(t *testing.T) {
				now, clockReads := base, 0
				s := mark.New(func() time.Time { clockReads++; return now }, time.Minute, mark.WithTouchMaxAge(10*time.Second))
				if state == "warm" {
					foldQuote(t, s, "QUOTE", dec(100, 0), dec(102, 0), base)
				}
				now = base.Add(5 * time.Second)
				beforeReads := clockReads
				if err := s.Handle(t.Context(), env("market.equity.quote"), quoteEvent(t, "QUOTE", tc.bid, tc.ask, now.Add(time.Hour))); err != nil {
					t.Fatalf("unusable quote changed the nil/ACK policy: %v", err)
				}
				price, at, seen := s.Lookup("QUOTE")
				if state == "cold" {
					if seen || price != nil {
						t.Fatalf("unusable tuple created a live valuation: price=%v seen=%v", price, seen)
					}
				} else if !seen || price == nil || price.Cmp(big.NewRat(101, 1)) != 0 || !at.Equal(base) {
					t.Fatalf("unusable tuple changed prior price/age: price=%v asOf=%v seen=%v", price, at, seen)
				}
				if clockReads != beforeReads {
					t.Fatal("refused tuple reached clock clamping or the state sweep")
				}
				bid, ask, touchAt, touchKnown := s.Touch("QUOTE")
				if state == "warm" {
					if !touchKnown || bid.Cmp(big.NewRat(100, 1)) != 0 || ask.Cmp(big.NewRat(102, 1)) != 0 || !touchAt.Equal(base) {
						t.Fatal("refused tuple replaced or refreshed the prior width")
					}
				} else if touchKnown {
					t.Fatal("refused tuple established quote coverage")
				}
				wantHeld := 0
				if state == "warm" {
					wantHeld = 1
				}
				if held, live := s.Stats(); held != wantHeld || live != wantHeld {
					t.Fatalf("mark coverage held=%d live=%d want=%d", held, live, wantHeld)
				}
				if held, live := s.TouchStats(); held != wantHeld || live != wantHeld {
					t.Fatalf("touch coverage held=%d live=%d want=%d", held, live, wantHeld)
				}
				now = base.Add(time.Minute + time.Nanosecond)
				if s.Mark("QUOTE") != nil {
					t.Fatal("refused tuple extended the original mark's lifetime")
				}
				if _, _, _, ok := s.Touch("QUOTE"); ok {
					t.Fatal("refused tuple extended the original touch's lifetime")
				}
			})
		}
	}
}

func TestLiveQuoteQualificationPreservesExactAndAvailabilityPolicies(t *testing.T) {
	for _, tc := range []struct {
		name, exact string
		bid, ask    *commonpb.Decimal
		touch       bool
	}{
		{"ordered", "81", dec(80, 0), dec(82, 0), true},
		{"locked-positive", "81", dec(81, 0), dec(810, -1), false},
		{"sub-float-spread", "1200000000000000001/1000000000000000000", dec(1200000000000000000, -18), dec(1200000000000000002, -18), true},
		{"mixed-scales", "41/50", dec(81, -2), dec(830, -3), true},
		{"absent-bid", "", nil, dec(81, 0), false},
		{"absent-ask", "", dec(81, 0), nil, false},
		{"both-absent", "", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mark.New(func() time.Time { return base }, time.Minute)
			foldQuote(t, s, "QUOTE", tc.bid, tc.ask, base)
			got := s.Mark("QUOTE")
			if tc.exact == "" {
				if got != nil {
					t.Fatal("live Mark acquired FX's one-sided fallback policy")
				}
			} else {
				want, ok := new(big.Rat).SetString(tc.exact)
				if !ok || got == nil || got.Cmp(want) != 0 {
					t.Fatalf("exact midpoint=%v, want %s", got, tc.exact)
				}
			}
			if _, _, _, ok := s.Touch("QUOTE"); ok != tc.touch {
				t.Fatalf("Touch known=%v want=%v", ok, tc.touch)
			}
			if tc.name == "locked-positive" {
				// A locked quote adds no width. An already observed ordered
				// touch retains its own time rather than inheriting the new mid.
				foldQuote(t, s, "PRIOR", dec(80, 0), dec(82, 0), base)
				foldQuote(t, s, "PRIOR", tc.bid, tc.ask, base.Add(time.Second))
				bid, ask, at, known := s.Touch("PRIOR")
				if !known || bid.Cmp(big.NewRat(80, 1)) != 0 || ask.Cmp(big.NewRat(82, 1)) != 0 || !at.Equal(base) {
					t.Fatal("locked mid replaced or refreshed a prior ordered touch")
				}
			}
		})
	}
}

func TestLiveQuoteRepairDoesNotInheritIgnoredTimestamp(t *testing.T) {
	for _, later := range []bool{false, true} {
		name, at := "before-invalid-future-time", base.Add(2*time.Second)
		if later {
			name, at = "after-invalid-future-time", base.Add(2*time.Hour)
		}
		t.Run(name, func(t *testing.T) {
			now := base
			s := mark.New(func() time.Time { return now }, time.Minute)
			foldQuote(t, s, "QUOTE", dec(100, 0), dec(102, 0), base)
			foldQuote(t, s, "QUOTE", dec(2, 0), dec(1, 0), base.Add(time.Hour))
			now = at
			foldQuote(t, s, "QUOTE", dec(201, 0), dec(203, 0), at)
			price, gotAt, seen := s.Lookup("QUOTE")
			if !seen || price == nil || price.Cmp(big.NewRat(202, 1)) != 0 || !gotAt.Equal(at) {
				t.Fatalf("valid repair=%v at=%v seen=%v", price, gotAt, seen)
			}
		})
	}
}

func TestLiveQuoteRefusalDoesNotSweepOtherMarks(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, time.Minute)
	if err := s.Handle(context.Background(), env("market.equity.trade"), tradeEvent(t, "BYSTANDER", dec(100, 0), base)); err != nil {
		t.Fatal(err)
	}
	now = base.Add(2 * time.Minute)
	foldQuote(t, s, "BAD", dec(-100, 0), dec(110, 0), now)
	// Lookup is diagnostic: an expired but unswept observation is still present.
	// A discarded input cannot tombstone that unrelated state as a side effect.
	if price, at, seen := s.Lookup("BYSTANDER"); !seen || price == nil || !at.Equal(base) {
		t.Fatal("discarded quote ran a state sweep")
	}
}
