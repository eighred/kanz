// Package instrument answers one question the platform could not previously ask:
// what is this pair actually quoted in? (#407)
//
// A tradeable pair has two sides. The canonical id names both — "BTC-USDT" is
// BTC bought with USDT — but the id is a LABEL, written by a human into a
// symbol map, and nothing checked it against what the exchange actually trades.
// The estate shipped with "BTC-USD" mapped to Binance's BTCUSDT and OKX's
// BTC-USDT, so every one of those positions was USDT-quoted and recorded as
// dollars.
//
// THAT IS NOT A LABELLING PROBLEM. A USDT-quoted position carries USDT credit
// exposure; marking it USD assumes a peg this platform never states and cannot
// monitor, so on a depeg the books are wrong in a direction nobody is watching.
// EXPOSURE_DIMENSION_CURRENCY aggregates it into the USD bucket, and a
// concentration limit on USD silently covers something else.
//
// THE VENUE SYMBOL IS THE SOURCE OF TRUTH HERE, not the id. The symbol is what
// the adapter sends to the exchange, so it is what actually determines what is
// bought — while the id is what someone typed beside it. Where the two disagree,
// this package says so and lets the caller refuse; it never silently prefers one.
//
// IT NEVER GUESSES. Every function reports whether it could answer at all, and
// "could not tell" is returned as such rather than as a confident wrong answer.
// A false mismatch would refuse a correctly configured venue at startup, which
// is a trading outage caused by a string-handling opinion.
package instrument

import "strings"

// Pair is a tradeable pair decomposed into the asset being traded and the asset
// it is priced and settled in.
type Pair struct {
	// Base is the asset being bought or sold — "BTC" in BTC-USDT.
	Base string
	// Quote is the asset it is priced and settled in — "USDT" in BTC-USDT.
	//
	// THIS IS THE FIELD THE PLATFORM WAS MISSING. It is a different question
	// from reference.v1's currency_code (the currency an instrument is
	// DENOMINATED in, which is the right model for an equity or a bond and models
	// exactly one currency, not a pair).
	Quote string
}

// ParseID decomposes a canonical instrument id into its pair.
//
// The canonical form is BASE-QUOTE. ok is false for anything else — an equity id
// like "AAPL" has no pair and must not be given a nonsense one, and an id with
// more than one separator is ambiguous rather than clever.
func ParseID(instrumentID string) (Pair, bool) {
	base, quote, found := strings.Cut(instrumentID, "-")
	if !found || base == "" || quote == "" || strings.Contains(quote, "-") {
		return Pair{}, false
	}
	return Pair{Base: strings.ToUpper(base), Quote: strings.ToUpper(quote)}, true
}

// QuoteOfVenueSymbol reports what an exchange symbol is actually quoted in,
// given the base asset it trades.
//
// Exchanges write the same pair differently — Binance concatenates (BTCUSDT),
// OKX hyphenates (BTC-USDT), others use "/" or "_". Separators are removed and
// the base is stripped from the front; what remains is the quote.
//
// ok IS FALSE WHENEVER THAT CANNOT BE DONE HONESTLY, and the cases are real:
// a symbol that does not start with the base (a venue with its own ticker for
// the asset), an empty remainder, or an empty base. Callers must treat false as
// "this venue did not tell us", never as a mismatch — refusing a venue because
// this function could not parse its symbol format would be an outage caused by
// a string-handling opinion.
func QuoteOfVenueSymbol(base, venueSymbol string) (string, bool) {
	base = strings.ToUpper(strings.TrimSpace(base))
	symbol := strings.ToUpper(strings.TrimSpace(venueSymbol))
	for _, sep := range []string{"-", "_", "/"} {
		symbol = strings.ReplaceAll(symbol, sep, "")
	}
	if base == "" || symbol == "" || !strings.HasPrefix(symbol, base) {
		return "", false
	}
	quote := strings.TrimPrefix(symbol, base)
	if quote == "" {
		return "", false
	}
	return quote, true
}

// Resolution is what the platform can honestly say about one mapping of a
// canonical id to a venue symbol.
type Resolution struct {
	// Pair is what the platform will report for this instrument. When the venue
	// symbol could be read, Quote is the VENUE's quote — the asset actually
	// bought — not the one the id claims.
	Pair Pair

	// IDQuote is what the canonical id claims, kept separately so a caller can
	// name both sides of a disagreement rather than only the answer.
	IDQuote string

	// Known reports whether the quote could be established from the venue symbol
	// at all. When false, Pair.Quote falls back to the id's claim and the
	// platform is believing a human's typing again — which is the state that
	// produced this issue, so it must be visible rather than assumed fine.
	Known bool

	// Mismatched reports that the id and the venue disagree: the id says USD and
	// the exchange trades USDT. It is only ever true when Known is true — an
	// unreadable symbol is not evidence of a mismatch.
	Mismatched bool
}

// Resolve establishes what a mapping actually trades.
//
// It is the ONE place that decides, so the adapter that refuses a bad mapping at
// startup, the catalogue that feeds the pair picker, and the reference data that
// records base and quote all answer identically. Two implementations of this
// would eventually disagree about what a position is denominated in.
func Resolve(instrumentID, venueSymbol string) (Resolution, bool) {
	p, ok := ParseID(instrumentID)
	if !ok {
		// Not a pair id at all — an equity, a bond, a fund. There is nothing to
		// resolve and nothing is wrong.
		return Resolution{}, false
	}
	res := Resolution{Pair: p, IDQuote: p.Quote}

	venueQuote, known := QuoteOfVenueSymbol(p.Base, venueSymbol)
	if !known {
		return res, true
	}
	res.Known = true
	res.Pair.Quote = venueQuote
	res.Mismatched = venueQuote != p.Quote
	return res, true
}
