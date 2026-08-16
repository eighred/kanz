// SameCandle / SameTradeCount (#509).
//
// UNGATED, unlike most of this package's bar tests: these are pure functions, so
// they run on a bare checkout rather than skipping when TEST_POSTGRES_URL is
// unset. That matters more here than usual, because these two decide whether a
// re-run WRITES, and both callers re-run on every tick.
package store

import (
	"testing"
	"time"
)

// aBar is a candle with every comparable field set to a distinct value, so a
// comparison that dropped one field is not accidentally satisfied by another.
func aBar() Bar {
	n := int64(42)
	return Bar{
		InstrumentID: "BTC-USDT",
		Venue:        "XBIN",
		Resolution:   Resolution1h,
		BucketStart:  time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
		Open:         dec(10000, -2), // 100.00
		High:         dec(11000, -2),
		Low:          dec(9000, -2),
		Close:        dec(10500, -2),
		Volume:       dec(250, 0),
		TradeCount:   &n,
	}
}

// EVERY COMPARED FIELD IS ACTUALLY COMPARED.
//
// A copy-paste in a six-line conjunction — `dec.Cmp(a.High, b.High)` written
// twice and Low never checked — is invisible on review and silent in production:
// the missed field simply stops being able to trigger a restatement, so a real
// correction to it is dropped forever. One subtest per field is what makes that
// a failure rather than a coincidence.
func TestSameCandleComparesEveryOHLCVField(t *testing.T) {
	other := int64(43)
	for name, mutate := range map[string]func(*Bar){
		"open":        func(b *Bar) { b.Open = dec(10001, -2) },
		"high":        func(b *Bar) { b.High = dec(11001, -2) },
		"low":         func(b *Bar) { b.Low = dec(9001, -2) },
		"close":       func(b *Bar) { b.Close = dec(10501, -2) },
		"volume":      func(b *Bar) { b.Volume = dec(251, 0) },
		"trade count": func(b *Bar) { b.TradeCount = &other },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := aBar(), aBar()
			mutate(&b)
			if SameCandle(a, b) {
				t.Errorf("a bar differing only in %s compared equal — that field can no longer "+
					"trigger a restatement, so a real correction to it is dropped and the stored "+
					"bar stays wrong forever", name)
			}
		})
	}

	// NON-VACUITY. A comparison that returned false unconditionally would satisfy
	// every case above, and would make every re-run write a restatement of
	// nothing — the opposite failure, and the one the callers are built around.
	if !SameCandle(aBar(), aBar()) {
		t.Error("two identical bars compared unequal — every idempotent re-run would write a " +
			"fresh knowledge_time row for an unchanged bar, burying real corrections in noise")
	}
}

// EQUAL VALUES IN DIFFERENT SCALES ARE THE SAME PRICE.
//
// 100.00 and 100 are one number written two ways, and a venue is free to change
// which it sends. Comparing representations rather than values would report a
// restatement on every bar the day a feed changed its formatting — thousands of
// them, all spurious, and indistinguishable from a mass correction.
func TestSameCandleComparesValuesNotRepresentations(t *testing.T) {
	a, b := aBar(), aBar()
	b.Open = dec(100, 0)      // 100, vs a's 10000e-2
	b.Volume = dec(25000, -2) // 250.00, vs a's 250

	if !SameCandle(a, b) {
		t.Error("100.00 did not equal 100 — the comparison is on representation rather than " +
			"value, so a venue changing its decimal formatting would restate every bar it sends")
	}
}

// THE IDENTITY AND BITEMPORAL FIELDS ARE DELIBERATELY IGNORED.
//
// KnowledgeTime is the one that matters: it is the ANSWER this comparison feeds.
// A caller compares candles to decide whether a new knowledge_time row is
// warranted, so including it would make every comparison false and turn every
// re-run into a restatement — precisely the bug SameCandle exists to prevent.
func TestSameCandleIgnoresIdentityAndKnowledgeTime(t *testing.T) {
	a, b := aBar(), aBar()
	b.KnowledgeTime = a.KnowledgeTime.Add(72 * time.Hour)
	b.InstrumentID = "ETH-USDT"
	b.Venue = "XNAS"
	b.Resolution = Resolution1d
	b.BucketStart = a.BucketStart.Add(time.Hour)

	if !SameCandle(a, b) {
		t.Error("SameCandle looked past the OHLCV — it answers whether two versions of a bar say " +
			"the same thing about the market, and identity fields are what make two bars two bars " +
			"rather than two versions of one")
	}
}

// UNREPORTED IS NOT ZERO, AND IT IS NOT A WILDCARD (#432).
//
// TradeCount is a *int64 precisely because a venue not reporting a count and a
// venue reporting zero trades are different claims. So:
//
//   - nil vs nil is EQUAL — neither run learned a count, nothing changed.
//   - nil vs 0 is DIFFERENT — the venue started telling us, and "no trades" is
//     information.
//   - 0 vs 0 is equal, and must not be confused with the nil case.
func TestSameTradeCountTreatsUnreportedAsItsOwnState(t *testing.T) {
	zero, alsoZero, n := int64(0), int64(0), int64(42)

	for _, c := range []struct {
		name string
		a, b *int64
		want bool
	}{
		{"neither reported", nil, nil, true},
		{"unreported vs zero", nil, &zero, false},
		{"zero vs unreported", &zero, nil, false},
		{"unreported vs a count", nil, &n, false},
		{"zero vs zero", &zero, &alsoZero, true},
		{"same count, different pointers", &n, ptr(int64(42)), true},
	} {
		if got := SameTradeCount(c.a, c.b); got != c.want {
			t.Errorf("%s: SameTradeCount = %v, want %v", c.name, got, c.want)
		}
	}
}

// THE POINTER-COMPARISON TRAP, ASSERTED DIRECTLY.
//
// `a.TradeCount == b.TradeCount` compiles, type-checks, and is wrong: it compares
// addresses. Two bars both reporting 42 hold different pointers, so every
// re-fetched candle would look restated and the store would grow a fresh
// knowledge_time row per bar per run. This case exists so that regression is a
// test failure rather than a silently doubling table.
func TestSameTradeCountComparesValuesNotAddresses(t *testing.T) {
	a, b := ptr(int64(42)), ptr(int64(42))
	if a == b {
		t.Fatal("the fixture handed out the same pointer twice — this test cannot detect the " +
			"address comparison it exists for")
	}
	if !SameTradeCount(a, b) {
		t.Error("two distinct pointers to 42 compared unequal — that is an address comparison, " +
			"and it restates every unchanged bar on every run")
	}
}

func ptr(v int64) *int64 { return &v }
