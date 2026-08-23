package execution

import (
	"fmt"
	"sort"
	"strings"
)

// THE SYMBOL MAP DECIDES WHAT GETS BOUGHT, AND IT WAS PARSED BY TWO COPIES OF A
// FUNCTION THAT ACCEPTED FOUR KINDS OF WRONG SILENTLY.
//
// `BINANCE_SYMBOLS` / `OKX_SYMBOLS` are `instrument=venueSymbol` lists. The
// parser lived — byte-identical — in both venue composition roots, and each
// dropped or absorbed the following without a word:
//
//	an entry with no `=`     the instrument is simply absent from the map, so an
//	                         order for it is refused with "no symbol" for a pair
//	                         the operator can see in the config
//	a duplicate instrument   last one wins; the operator's two lines disagree and
//	                         one of them is invisible
//	a duplicate VENUE SYMBOL two instrument ids resolve to ONE exchange symbol —
//	                         see below, it is the dangerous one
//	an empty key or value    a mapping to or from nothing
//
// # Two instruments to one venue symbol is the one that costs money
//
// The platform then believes it holds TWO positions where the exchange holds
// ONE. Every reconciliation against the venue attributes the whole position to
// whichever id it looked up first; fills for either land against a single
// exchange position; and the id→symbol map cannot be inverted at all, so a
// venue-reported liquidation price (collateral.v1.VenueLiquidationPrice carries
// the exchange's own spelling and no instrument id) can be attributed to no
// instrument. That inversion is one of the two things #408 control 4's
// exemption names as missing, and it is undefinable rather than merely unwritten
// while the map may be many-to-one.
//
// It is the same class as the quote-mismatch check above it, which this
// repository already treats as a startup-refusable defect: a config that
// misdescribes what it trades.

// ParseSymbolMap parses an `id=symbol,id=symbol` list into a symbol map,
// REFUSING anything it cannot represent faithfully.
//
// It is deliberately strict where the two copies it replaces were silent. A
// venue adapter starting on a map it has quietly repaired is the shape this
// estate keeps paying for; the composition root turns the error into a refusal
// to start, which is recoverable in a minute, unlike a position attributed to
// the wrong instrument.
//
// An EMPTY string yields an empty map and no error: an adapter deployed with no
// symbols trades nothing, which is a legitimate posture (venue_identity.go says
// so of the resulting empty Instruments list) and is not the same as a
// malformed one.
func ParseSymbolMap(s string) (StaticSymbolMap, error) {
	out := StaticSymbolMap{}
	bySymbol := map[string]string{} // venue symbol -> the id that claimed it
	var problems []string

	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		id, symbol, ok := strings.Cut(pair, "=")
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%q is not an instrument=symbol pair; it would be dropped, and an order for that "+
					"instrument refused for having no symbol while the operator can see it here", pair))
			continue
		}
		id, symbol = strings.TrimSpace(id), strings.TrimSpace(symbol)
		if id == "" || symbol == "" {
			problems = append(problems, fmt.Sprintf("%q maps to or from nothing", pair))
			continue
		}
		if prev, dup := out[id]; dup {
			problems = append(problems, fmt.Sprintf(
				"instrument %q is mapped twice (%q and %q); one of the two lines is invisible and "+
					"which one wins is not something this file states", id, prev, symbol))
			continue
		}
		if claimant, dup := bySymbol[symbol]; dup {
			problems = append(problems, fmt.Sprintf(
				"venue symbol %q is claimed by both %q and %q. The platform would believe it holds "+
					"TWO positions where the exchange holds ONE: reconciliation attributes the whole "+
					"position to whichever id it looks up first, fills for either land against a "+
					"single exchange position, and a venue-reported liquidation price cannot be "+
					"attributed to an instrument at all", symbol, claimant, id))
			continue
		}
		out[id] = symbol
		bySymbol[symbol] = id
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("symbol map: %d problem(s) — this map decides what is actually "+
			"bought, so it is refused rather than repaired:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
	return out, nil
}

// Instrument is the INVERSE lookup: which instrument this platform calls the
// exchange's symbol.
//
// IT IS ONLY DEFINABLE BECAUSE ParseSymbolMap REFUSES A MANY-TO-ONE MAP. With
// two ids on one symbol there is no answer to return and no way to know that
// there isn't — which is exactly why #408 control 4's exemption records the
// map as "a ONE-WAY instrument -> symbol table".
//
// A map built by hand rather than through ParseSymbolMap may still be
// many-to-one; this scans it and reports NOT FOUND for an ambiguous symbol
// rather than returning an arbitrary one of the candidates. An arbitrary answer
// here attributes a venue position to the wrong instrument, which is the whole
// failure being avoided.
func (m StaticSymbolMap) Instrument(venueSymbol string) (string, bool) {
	found, n := "", 0
	for id, symbol := range m {
		if symbol == venueSymbol {
			found, n = id, n+1
		}
	}
	if n != 1 {
		return "", false
	}
	return found, true
}
