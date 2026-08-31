package mark_test

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/mark"
)

// THE QUOTED WIDTH, TESTED AT THE LAYER THAT OWNS IT (#875).
//
// The touch fold landed with #866 and had no test in this package: every
// property below was exercised only through services/oms/internal/order, three
// layers up, where a width that is silently absent reads as an order with
// nothing to attribute rather than as a broken fold. That is the wrong place to
// find out — the OMS path cannot distinguish "the fold refused this quote" from
// "the spine never sent one", and #875 exists because nobody could tell those
// apart in production either.
//
// What each case here is really asserting is one half of the coverage gauge's
// meaning. TouchStats is about to be read by an alert, and an alert over a
// number whose definition is untested is the shape alerts/README.md is a
// cautionary record of.

// foldQuote folds one two-sided quote at `at`.
func foldQuote(t *testing.T, s *mark.Source, instrument string, bid, ask *commonpb.Decimal, at time.Time) {
	t.Helper()
	if err := s.Handle(context.Background(), env("market.crypto.quote"),
		quoteEvent(t, instrument, bid, ask, at)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestAQuoteRecordsAUsableWidth(t *testing.T) {
	s := mark.New(func() time.Time { return base }, 30*time.Second)
	foldQuote(t, s, "BTC-USD", dec(9990, -1), dec(10010, -1), base)

	bid, ask, asOf, ok := s.Touch("BTC-USD")
	if !ok {
		t.Fatal("a fresh two-sided quote is not readable as a touch — the spread leg of every " +
			"attribution on this instrument would be unmeasurable")
	}
	if bid.FloatString(1) != "999.0" || ask.FloatString(1) != "1001.0" {
		t.Fatalf("Touch = (%s, %s), want (999.0, 1001.0)", bid.FloatString(1), ask.FloatString(1))
	}
	if !asOf.Equal(base) {
		t.Fatalf("asOf = %v, want the quote's own event time %v", asOf, base)
	}
	if held, live := s.TouchStats(); held != 1 || live != 1 {
		t.Fatalf("TouchStats = (held %d, live %d), want (1, 1)", held, live)
	}
}

// THE PREMISE OF #875, EXECUTED. A trade print has no width, and this is the
// property that makes an all-trade price spine — which is what both deployed
// venue adapters publish — produce TOTAL_ONLY on every decomposition.
//
// It is asserted here rather than assumed because the gauge and the alert built
// on it are only meaningful if a trade genuinely contributes nothing: if a trade
// folded a width of any kind, coverage would read as complete on precisely the
// estate that has none.
func TestATradePrintRecordsNoWidth(t *testing.T) {
	s := mark.New(func() time.Time { return base }, 30*time.Second)
	foldTrade(t, s, "BTC-USD", 1000, base)

	if _, _, _, ok := s.Touch("BTC-USD"); ok {
		t.Fatal("a trade print produced a quoted width — a print has no bid and no ask, so any " +
			"width here was fabricated and every spread cost derived from it is invented")
	}
	if held, live := s.TouchStats(); held != 0 || live != 0 {
		t.Fatalf("TouchStats = (held %d, live %d), want (0, 0) — a trade-only spine must report "+
			"zero quote coverage, which is the signal #875's alert reads", held, live)
	}
	// The mark itself IS recorded: this fold is not blind to a trade, it simply
	// has no width to take from one. Stated so the case above cannot be passed by
	// a fold that dropped the event entirely.
	if s.Mark("BTC-USD") == nil {
		t.Fatal("the trade recorded no mark either — the fold dropped the event, so the assertion " +
			"above is about nothing")
	}
}

// A ZERO-WIDTH QUOTE IS NOT A TIGHT MARKET, AND IT IS NOT HYPOTHETICAL.
// services/datamaster/internal/feed publishes Quote{BidPrice: p, AskPrice: p} —
// one price on both legs — which is a mid dressed as a quote. Folded as a width
// it would report a spread of exactly zero: the cheapest possible execution, on
// an instrument nobody ever quoted.
func TestAZeroWidthQuoteIsRefused(t *testing.T) {
	s := mark.New(func() time.Time { return base }, 30*time.Second)
	foldQuote(t, s, "BTC-USD", dec(1000, 0), dec(1000, 0), base)

	if _, _, _, ok := s.Touch("BTC-USD"); ok {
		t.Fatal("bid == ask was folded as a width — it would attribute a zero cost of crossing to " +
			"an instrument whose book was never observed")
	}
	if held, _ := s.TouchStats(); held != 0 {
		t.Fatalf("held = %d, want 0 — a refused quote must not be counted as coverage", held)
	}
}

func TestACrossedQuoteIsRefused(t *testing.T) {
	s := mark.New(func() time.Time { return base }, 30*time.Second)
	foldQuote(t, s, "BTC-USD", dec(10010, -1), dec(9990, -1), base)

	if _, _, _, ok := s.Touch("BTC-USD"); ok {
		t.Fatal("an inverted book was folded as a width — a negative half-spread arrives in the " +
			"attribution as the fund being PAID to take liquidity")
	}
	if held, _ := s.TouchStats(); held != 0 {
		t.Fatalf("held = %d, want 0", held)
	}
}

// THE REGRESSION THE COVERAGE GAUGE EXISTS TO PREVENT (#875).
//
// sweepTouchesLocked runs only inside a fold and only once per sweepInterval, so
// an expired width survives in the map until the next Quote arrives. On a spine
// that stopped quoting — the condition being measured — that Quote never comes,
// so held stays at its high-water mark indefinitely.
//
// A gauge reading held alone would therefore report FULL coverage on a quote
// feed that has been dead for hours, and the alert built on it could never fire.
// live is computed through the same predicate Touch reads, so the two cannot
// disagree.
func TestAnExpiredWidthIsHeldButNotLive(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 30*time.Second)
	foldQuote(t, s, "BTC-USD", dec(9990, -1), dec(10010, -1), base)

	if held, live := s.TouchStats(); held != 1 || live != 1 {
		t.Fatalf("TouchStats = (held %d, live %d), want (1, 1) before expiry", held, live)
	}

	// Age past maxAge WITHOUT folding anything — no fold means no sweep, which is
	// exactly the state a stopped quote feed leaves behind.
	now = base.Add(time.Hour)

	if _, _, _, ok := s.Touch("BTC-USD"); ok {
		t.Fatal("Touch answered for a width an hour past OMS_PRICE_MAX_AGE")
	}
	held, live := s.TouchStats()
	if held != 1 {
		t.Fatalf("held = %d, want 1 — the unswept entry is still held, and held − live is the "+
			"expired-width population an operator reads to tell a stopped feed from a cold one", held)
	}
	if live != 0 {
		t.Fatalf("live = %d, want 0 — the gauge is reporting coverage Touch would refuse. An "+
			"alert over this number can never fire on a dead quote feed, which is the whole of "+
			"what #875 asked it to detect", live)
	}
}

// A TRADE MUST NOT ERASE A GOOD WIDTH, AND MUST NOT RESTAMP IT. Both halves are
// one assertion: touch.go keeps a separate map precisely so a mark-bearing trade
// — common on a thin book — neither drops the last observed width nor inherits
// the trade's own timestamp, which would make an expired width look fresh.
func TestATradeLeavesTheWidthAndItsTimestampAlone(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, time.Hour)
	foldQuote(t, s, "BTC-USD", dec(9990, -1), dec(10010, -1), base)

	now = base.Add(10 * time.Minute)
	foldTrade(t, s, "BTC-USD", 1000, now)

	bid, ask, asOf, ok := s.Touch("BTC-USD")
	if !ok {
		t.Fatal("a trade print erased the quoted width beside it — on a thin book every quote " +
			"would be wiped by the next print, and the instrument would read as unquoted")
	}
	if bid.FloatString(1) != "999.0" || ask.FloatString(1) != "1001.0" {
		t.Fatalf("Touch = (%s, %s), want the quote's own legs unchanged", bid.FloatString(1), ask.FloatString(1))
	}
	if !asOf.Equal(base) {
		t.Fatalf("asOf = %v, want the QUOTE's time %v — a width stamped with the trade's time is a "+
			"width claimed to have been observed at an instant it was not, and it would outlive "+
			"its own expiry", asOf, base)
	}
	if held, live := s.TouchStats(); held != 1 || live != 1 {
		t.Fatalf("TouchStats = (held %d, live %d), want (1, 1)", held, live)
	}
}

// maxAge == 0 is tv-sync's posture: marks never expire, and neither do widths.
// Asserted because live is computed from a predicate that branches on maxAge,
// and a predicate that got that branch wrong would report zero coverage for the
// one caller that configured no expiry at all.
func TestAZeroMaxAgeWidthNeverExpires(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 0)
	foldQuote(t, s, "BTC-USD", dec(9990, -1), dec(10010, -1), base)

	now = base.Add(72 * time.Hour)
	if _, _, _, ok := s.Touch("BTC-USD"); !ok {
		t.Fatal("a width expired under maxAge == 0, which disables expiry")
	}
	if held, live := s.TouchStats(); held != 1 || live != 1 {
		t.Fatalf("TouchStats = (held %d, live %d), want (1, 1) — with expiry disabled every held "+
			"width is live by definition", held, live)
	}
}

// COVERAGE IS PER INSTRUMENT, not per fold. A spine that quotes one instrument
// and prints trades on two others has partial coverage, and the gauge must say
// so rather than reporting the fold as covered because SOMETHING was quoted.
func TestCoverageIsCountedPerInstrument(t *testing.T) {
	s := mark.New(func() time.Time { return base }, 30*time.Second)
	foldQuote(t, s, "BTC-USD", dec(9990, -1), dec(10010, -1), base)
	foldTrade(t, s, "ETH-USD", 50, base)
	foldTrade(t, s, "SOL-USD", 20, base)

	held, live := s.TouchStats()
	if held != 1 || live != 1 {
		t.Fatalf("TouchStats = (held %d, live %d), want (1, 1) — only BTC-USD was quoted", held, live)
	}
	if markHeld, markLive := s.Stats(); markHeld != 3 || markLive != 3 {
		t.Fatalf("Stats = (held %d, live %d), want (3, 3) — the mark fold saw all three, and it is "+
			"the ratio between the two folds that names the gap", markHeld, markLive)
	}
}
