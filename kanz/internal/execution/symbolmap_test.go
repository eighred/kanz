package execution_test

import (
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/execution"
)

// THE SYMBOL MAP DECIDES WHAT GETS BOUGHT.
//
// It was parsed by two byte-identical copies in the venue composition roots,
// each of which absorbed four kinds of wrong without a word. Every case below is
// one of those.

func TestParseSymbolMapReadsAWellFormedList(t *testing.T) {
	m, err := execution.ParseSymbolMap("BTC-USDT=BTCUSDT, ETH-USDT=ETHUSDT")
	if err != nil {
		t.Fatalf("ParseSymbolMap: %v", err)
	}
	if got, ok := m.Symbol("BTC-USDT"); !ok || got != "BTCUSDT" {
		t.Errorf("Symbol(BTC-USDT) = %q,%v", got, ok)
	}
	if got, ok := m.Symbol("ETH-USDT"); !ok || got != "ETHUSDT" {
		t.Errorf("Symbol(ETH-USDT) = %q,%v", got, ok)
	}
	if len(m) != 2 {
		t.Errorf("map has %d entries, want 2", len(m))
	}
}

// AN EMPTY MAP IS A POSTURE, NOT A FAULT. An adapter deployed with no symbols
// trades nothing — venue_identity.go says an empty Instruments list "is an
// answer" — and that must not be confused with a malformed list.
func TestAnEmptySpecIsNotAnError(t *testing.T) {
	m, err := execution.ParseSymbolMap("")
	if err != nil {
		t.Fatalf("an empty symbol map was refused: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("empty spec produced %d entries", len(m))
	}
	// Trailing and repeated separators are ordinary ways to write a list.
	if _, err := execution.ParseSymbolMap("BTC-USDT=BTCUSDT, ,"); err != nil {
		t.Fatalf("a trailing separator was refused: %v", err)
	}
}

// THE FOUR REFUSALS, each naming what it costs.
func TestParseSymbolMapRefusesWhatItCannotRepresent(t *testing.T) {
	for _, tc := range []struct {
		name, spec, wants string
	}{
		{
			// Silently dropped before: the instrument is absent from the map, so
			// an order for it is refused for having no symbol while the operator
			// can see it in the config.
			name: "an entry with no =", spec: "BTC-USDT=BTCUSDT,ETHUSDT",
			wants: "not an instrument=symbol pair",
		},
		{
			// Last-one-wins before, and which line lost was not stated anywhere.
			name: "the same instrument twice", spec: "BTC-USDT=BTCUSDT,BTC-USDT=XBTUSDT",
			wants: "mapped twice",
		},
		{
			// THE ONE THAT COSTS MONEY.
			name: "two instruments on one venue symbol", spec: "BTC-USDT=BTCUSDT,BTC-USD=BTCUSDT",
			wants: "TWO positions where the exchange holds ONE",
		},
		{name: "an empty instrument", spec: "=BTCUSDT", wants: "maps to or from nothing"},
		{name: "an empty symbol", spec: "BTC-USDT=", wants: "maps to or from nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := execution.ParseSymbolMap(tc.spec)
			if err == nil {
				t.Fatalf("ParseSymbolMap(%q) = %v, want a refusal", tc.spec, m)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error does not say what it costs (want %q):\n%v", tc.wants, err)
			}
		})
	}
}

// The refusal names BOTH claimants of a duplicated symbol. An operator given one
// of the two has to grep for the other, and the fix is a choice between them.
func TestTheDuplicateSymbolRefusalNamesBothInstruments(t *testing.T) {
	_, err := execution.ParseSymbolMap("BTC-USDT=BTCUSDT,BTC-USD=BTCUSDT")
	if err == nil {
		t.Fatal("a many-to-one map was accepted")
	}
	for _, want := range []string{"BTC-USDT", "BTC-USD", "BTCUSDT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q:\n%v", want, err)
		}
	}
}

// ALL problems are reported, not just the first. An operator fixing a config one
// refused startup at a time is the slowest possible loop.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	_, err := execution.ParseSymbolMap("BTC-USDT=BTCUSDT,ETHUSDT,=X,SOL-USDT=BTCUSDT")
	if err == nil {
		t.Fatal("a map with three problems was accepted")
	}
	if !strings.Contains(err.Error(), "3 problem(s)") {
		t.Errorf("expected all three problems in one refusal:\n%v", err)
	}
}

// THE INVERSE LOOKUP, which is the point of refusing a many-to-one map:
// collateral.v1.VenueLiquidationPrice carries the exchange's own spelling and no
// instrument id, so nothing can pair a liquidation price with a mark without it
// (#408 control 4).
func TestInstrumentInvertsTheMap(t *testing.T) {
	m, err := execution.ParseSymbolMap("BTC-USDT=BTCUSDT,ETH-USDT=ETHUSDT")
	if err != nil {
		t.Fatalf("ParseSymbolMap: %v", err)
	}
	if got, ok := m.Instrument("BTCUSDT"); !ok || got != "BTC-USDT" {
		t.Errorf("Instrument(BTCUSDT) = %q,%v want BTC-USDT", got, ok)
	}
	if _, ok := m.Instrument("DOGEUSDT"); ok {
		t.Error("a symbol this deployment does not trade resolved to an instrument")
	}
}

// A HAND-BUILT MANY-TO-ONE MAP STILL REFUSES TO INVERT. StaticSymbolMap is a
// plain map type and a caller may construct one directly, bypassing the parser.
// Returning an arbitrary one of the candidates would attribute a venue position
// to the wrong instrument — the exact failure the parser's refusal prevents, so
// the inverse must not reintroduce it one layer down.
func TestInstrumentRefusesAnAmbiguousSymbol(t *testing.T) {
	m := execution.StaticSymbolMap{"BTC-USDT": "BTCUSDT", "BTC-USD": "BTCUSDT"}
	if got, ok := m.Instrument("BTCUSDT"); ok {
		t.Fatalf("an ambiguous symbol resolved to %q — a venue position would be attributed to "+
			"whichever id the map happened to yield", got)
	}
}
