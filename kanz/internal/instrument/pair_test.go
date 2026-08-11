package instrument

import "testing"

func TestParseID(t *testing.T) {
	for _, tc := range []struct {
		id          string
		base, quote string
		ok          bool
		why         string
	}{
		{"BTC-USDT", "BTC", "USDT", true, "the canonical form"},
		{"btc-usdt", "BTC", "USDT", true, "case is normalised, so a lowercase manifest entry is not a second instrument"},
		{"BTC-USD", "BTC", "USD", true, "the id is read as written — whether it is TRUE is Resolve's question"},
		{"AAPL", "", "", false, "an equity has no pair and must not be given a nonsense one"},
		{"", "", "", false, "empty"},
		{"BTC-", "", "", false, "half a pair is not a pair"},
		{"-USDT", "", "", false, "half a pair is not a pair"},
		{"BTC-USD-PERP", "", "", false, "ambiguous rather than clever: two separators could split three ways"},
	} {
		p, ok := ParseID(tc.id)
		if ok != tc.ok {
			t.Errorf("ParseID(%q) ok = %v, want %v — %s", tc.id, ok, tc.ok, tc.why)
			continue
		}
		if ok && (p.Base != tc.base || p.Quote != tc.quote) {
			t.Errorf("ParseID(%q) = %s/%s, want %s/%s", tc.id, p.Base, p.Quote, tc.base, tc.quote)
		}
	}
}

// Every live venue writes the same pair differently. All of these are real
// formats; the last group is the set this must REFUSE to answer for, because a
// confident wrong answer here refuses a correctly configured venue at startup.
func TestQuoteOfVenueSymbol(t *testing.T) {
	for _, tc := range []struct {
		base, symbol string
		quote        string
		ok           bool
		why          string
	}{
		{"BTC", "BTCUSDT", "USDT", true, "Binance concatenates"},
		{"BTC", "BTC-USDT", "USDT", true, "OKX hyphenates"},
		{"BTC", "BTC/USDT", "USDT", true, "slash"},
		{"BTC", "BTC_USDT", "USDT", true, "underscore"},
		{"btc", "btcusdt", "USDT", true, "case is normalised on both sides"},
		{"BTC", " BTCUSDT ", "USDT", true, "a manifest entry with stray whitespace is not a different venue"},
		{"ETH", "ETHEUR", "EUR", true, "a genuinely EUR-quoted pair"},

		{"BTC", "XBTUSDT", "", false, "the venue uses its own ticker for the asset — we cannot read it, and must not pretend"},
		{"BTC", "BTC", "", false, "nothing left after the base"},
		{"BTC", "", "", false, "no symbol"},
		{"", "BTCUSDT", "", false, "no base to strip"},
	} {
		q, ok := QuoteOfVenueSymbol(tc.base, tc.symbol)
		if ok != tc.ok {
			t.Errorf("QuoteOfVenueSymbol(%q, %q) ok = %v, want %v — %s", tc.base, tc.symbol, ok, tc.ok, tc.why)
			continue
		}
		if ok && q != tc.quote {
			t.Errorf("QuoteOfVenueSymbol(%q, %q) = %q, want %q", tc.base, tc.symbol, q, tc.quote)
		}
	}
}

// THE CASE THIS PACKAGE EXISTS FOR. The estate shipped with BTC-USD mapped to
// BTCUSDT on Binance and BTC-USDT on OKX: the id says dollars, both exchanges
// trade a stablecoin. The platform must report what is actually bought and must
// say the two disagree.
func TestResolveReportsTheVenuesQuoteAndNamesTheDisagreement(t *testing.T) {
	for _, symbol := range []string{"BTCUSDT", "BTC-USDT"} {
		res, isPair := Resolve("BTC-USD", symbol)
		if !isPair {
			t.Fatalf("Resolve(BTC-USD, %s): not recognised as a pair", symbol)
		}
		if !res.Known {
			t.Fatalf("Resolve(BTC-USD, %s): quote unknown, but this symbol is readable", symbol)
		}
		if res.Pair.Quote != "USDT" {
			t.Errorf("quote = %q, want USDT — the VENUE decides what is bought, not the id", res.Pair.Quote)
		}
		if res.IDQuote != "USD" {
			t.Errorf("id quote = %q, want USD — both sides must be nameable, or an operator cannot see the disagreement", res.IDQuote)
		}
		if !res.Mismatched {
			t.Errorf("Resolve(BTC-USD, %s) reports agreement; this is the exact mapping that records a "+
				"USDT position as dollars", symbol)
		}
	}
}

// A CORRECTED MAPPING IS SILENT. Without this the test above is satisfied by a
// function that flags everything, which would refuse every venue once the check
// is armed.
func TestResolveIsQuietWhenTheIDTellsTheTruth(t *testing.T) {
	res, isPair := Resolve("BTC-USDT", "BTCUSDT")
	if !isPair || !res.Known {
		t.Fatalf("Resolve(BTC-USDT, BTCUSDT) = %+v", res)
	}
	if res.Mismatched {
		t.Error("a correctly named pair is reported as mismatched — arming the check would refuse every venue")
	}
	if res.Pair.Base != "BTC" || res.Pair.Quote != "USDT" {
		t.Errorf("pair = %s/%s, want BTC/USDT", res.Pair.Base, res.Pair.Quote)
	}
}

// AN UNREADABLE SYMBOL IS NOT A MISMATCH. It is "the venue did not tell us", and
// treating it as disagreement would refuse a correctly configured adapter whose
// exchange happens to use its own ticker — an outage caused by a string-handling
// opinion. The id's claim stands, and Known=false is how the caller knows the
// platform is believing a human's typing.
func TestResolveDoesNotInventAMismatchItCannotSee(t *testing.T) {
	res, isPair := Resolve("BTC-USD", "XBTUSD")
	if !isPair {
		t.Fatal("BTC-USD is a pair id")
	}
	if res.Known {
		t.Fatal("claimed to know the quote of a symbol it cannot decompose")
	}
	if res.Mismatched {
		t.Error("reported a mismatch from a symbol it could not read — this refuses a working venue at startup")
	}
	if res.Pair.Quote != "USD" {
		t.Errorf("quote = %q, want the id's own claim USD as the fallback", res.Pair.Quote)
	}
}

// A non-pair instrument is not this package's business, and must not acquire a
// base or a quote. An equity with a quote asset is a nonsense record that
// exposure-by-currency would then aggregate on.
func TestResolveLeavesNonPairInstrumentsAlone(t *testing.T) {
	if res, isPair := Resolve("AAPL", "AAPL"); isPair {
		t.Errorf("AAPL was decomposed into a pair: %+v", res)
	}
}
