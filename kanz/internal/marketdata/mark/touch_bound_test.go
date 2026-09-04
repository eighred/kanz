package mark_test

import (
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/mark"
)

// THE QUOTED WIDTH'S OWN STALENESS BOUND (#956).
//
// Until this file, `mark.Source` had ONE maxAge and it governed both maps. That
// number was calibrated for a MARK: its producers are the two venue adapters'
// REST ticker polls at 5s, so the OMS's 30s is six missed observations — an
// outage tolerance. The width's producer is market-ingest's book-snapshot
// ticker at 1s, so the same 30s was thirty missed publishes: five times looser
// in the only unit that matters, on the leg an execution report cannot recompute
// from anything else afterwards.

// THE WIDTH AGES OUT WHILE THE MARK OF THE SAME AGE IS STILL SERVED.
//
// This is the whole of what a second bound buys, and it is stated as ONE fold
// producing BOTH observations so the two ages are equal by construction rather
// than by arithmetic in the test. A quote writes the mid into the price map and
// the two legs into the touch map under the same asOf; the clock then moves once
// and the two accessors disagree.
//
// WHY IT MATTERS THAT ONLY ONE OF THEM REFUSES. Tightening the shared bound was
// the obvious repair and is the wrong one: maxAge governs the mark the pre-trade
// gate values a MARKET order from, so cutting it to the width's cadence turns on
// PRICE_UNAVAILABLE refusals for every MARKET and STOP order on a 5s-poll ticker
// feed — a trading outage traded for a measurement fix. If this test ever fails
// because Mark refused too, that trade has been made by accident.
func TestAWidthAgesOutWhileAMarkOfTheSameAgeIsStillServed(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 30*time.Second, mark.WithTouchMaxAge(6*time.Second))

	// Fold one: the quote that produces both observations, at `base`.
	foldQuote(t, s, "BTC-USD", dec(9990, -1), dec(10010, -1), base)

	// THE CLOCK MOVES BETWEEN THE TWO FOLDS. 10s is past the width's 6s bound and
	// well inside the mark's 30s — the window this issue is entirely about.
	now = base.Add(10 * time.Second)

	// Fold two: a different instrument, quoted at the new time. It is here so the
	// aged entry is read on a Source that has folded SINCE the clock moved (the
	// state a live spine is always in), and so the assertions below cannot pass
	// by the fold being globally broken — ETH-USD must still answer.
	foldQuote(t, s, "ETH-USD", dec(29990, -1), dec(30010, -1), now)

	got := s.Mark("BTC-USD")
	if got == nil {
		t.Fatal("Mark refused a 10s-old mark under a 30s bound — the quote-specific bound has " +
			"leaked onto the price map, which turns a measurement fix into PRICE_UNAVAILABLE " +
			"refusals for every MARKET and STOP order on a 5s-poll feed")
	}
	if got.FloatString(1) != "1000.0" {
		t.Fatalf("Mark = %s, want the quote's own mid 1000.0", got.FloatString(1))
	}

	if _, _, _, ok := s.Touch("BTC-USD"); ok {
		t.Fatal("Touch answered for a width 10s past its 6s bound. The spread leg of this " +
			"decision's attribution would be measured against a market that is ten seconds " +
			"gone, and a stale width is usually a TIGHTER one — so the cost comes back too " +
			"small and the residual is charged to the algorithm as impact or timing")
	}

	if _, _, _, ok := s.Touch("ETH-USD"); !ok {
		t.Fatal("Touch refused a width quoted at the current instant — the bound is refusing " +
			"everything, so the case above proves nothing")
	}
}

// UNSET, THE WIDTH KEEPS THE MARK'S BOUND. This is what made the second bound a
// change no existing caller had to notice: tv-sync (maxAge 0) and the compliance
// monitor (COMPLIANCE_PRICE_MAX_AGE) construct a Source with no option and must
// behave exactly as they did before the field existed.
//
// A NON-POSITIVE OPTION IS IGNORED rather than read as "never expires", and the
// same assertion covers it: a width outliving the mark taken from the very same
// quote is the one arrangement neither bound was meant to express, and
// WithTouchMaxAge(0) is what a caller reaching for an off switch would write.
func TestAWidthInheritsTheMarksBoundWhenNoOptionSetsOne(t *testing.T) {
	cases := map[string][]mark.Option{
		"no option":           nil,
		"zero is ignored":     {mark.WithTouchMaxAge(0)},
		"negative is ignored": {mark.WithTouchMaxAge(-5 * time.Second)},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			now := base
			s := mark.New(func() time.Time { return now }, 30*time.Second, opts...)
			foldQuote(t, s, "BTC-USD", dec(9990, -1), dec(10010, -1), base)

			now = base.Add(10 * time.Second)
			if _, _, _, ok := s.Touch("BTC-USD"); !ok {
				t.Fatal("Touch refused a 10s-old width under an inherited 30s bound — adding the " +
					"second bound changed a caller that never asked for one")
			}

			now = base.Add(31 * time.Second)
			if _, _, _, ok := s.Touch("BTC-USD"); ok {
				t.Fatal("Touch answered past the inherited 30s bound — the inheritance is not a " +
					"bound at all, it is an off switch")
			}
		})
	}
}

// THE SWEEP DROPS ON THE WIDTH'S BOUND, THE TOMBSTONE STILL ON THE MARK'S.
//
// touchUsableLocked and sweepTouchesLocked have to read the SAME number or the
// map keeps entries no accessor can answer from — memory held for a population
// with no reader, and a TouchStats held count that describes it as coverage. The
// price map must be untouched by that: at the instant below the mark is exactly
// 30s old and therefore NOT expired (the check is a strict >), so its price must
// survive the same sweep that deletes the width beside it.
func TestTheSweepDropsAWidthOnItsOwnBoundAndLeavesTheMarkAlone(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 30*time.Second, mark.WithTouchMaxAge(6*time.Second))

	// Fold one. This also arms the sweep throttle (lastSweep = base), so the next
	// sweep is due sweepInterval later.
	foldQuote(t, s, "BTC-USD", dec(9990, -1), dec(10010, -1), base)

	// The clock advances between the folds by exactly sweepInterval, which is the
	// soonest the throttle allows a second sweep to run at all.
	now = base.Add(30 * time.Second)

	// Fold two: the event that runs the sweep.
	foldQuote(t, s, "ETH-USD", dec(29990, -1), dec(30010, -1), now)

	held, live := s.TouchStats()
	if held != 1 || live != 1 {
		t.Fatalf("TouchStats = (held %d, live %d), want (1, 1) — the sweep must have deleted "+
			"BTC-USD's expired width and kept ETH-USD's fresh one. held > live here means the "+
			"sweep is testing the MARK's bound and retaining widths Touch already refuses",
			held, live)
	}

	if s.Mark("BTC-USD") == nil {
		t.Fatal("the sweep tombstoned a mark that is exactly maxAge old — the width's shorter " +
			"bound has reached the price map")
	}
	if _, live := s.Stats(); live != 2 {
		t.Fatalf("Stats live = %d, want 2 — both marks are within the 30s bound", live)
	}
}

// A SOURCE WITH NO MARK BOUND AND A REAL WIDTH BOUND STILL SWEEPS ITS WIDTHS.
//
// It is expressible now that the two bounds are independent, and sweepLocked
// returning early on maxAge alone would have left every expired width in the map
// forever while Touch correctly refused it — an unbounded map behind a correct
// accessor, which is the failure mode that is hardest to see because nothing
// reads wrong.
func TestAWidthBoundSweepsEvenWhenMarksNeverExpire(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 0, mark.WithTouchMaxAge(6*time.Second))

	foldQuote(t, s, "BTC-USD", dec(9990, -1), dec(10010, -1), base)
	now = base.Add(30 * time.Second)
	foldQuote(t, s, "ETH-USD", dec(29990, -1), dec(30010, -1), now)

	if held, _ := s.TouchStats(); held != 1 {
		t.Fatalf("TouchStats held = %d, want 1 — the expired width was retained on a Source "+
			"whose marks never expire, so the touch map grows without bound", held)
	}
	if s.Mark("BTC-USD") == nil {
		t.Fatal("a mark expired on a Source constructed with maxAge 0, which means never")
	}
}
