package livequote

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// RateInstrument binds one cached instrument to its role in a currency's rate-
// curve calibration strip. Which listed instrument is a 3M deposit vs a 5Y par
// swap is reference metadata the composition root supplies (the "regulatory /
// reference parameters are data" stance), not a wire field.
type RateInstrument struct {
	InstrumentID string          // cache key (MarketDataEvent.instrument_id)
	Currency     string          // the curve it calibrates (e.g. "USD")
	Kind         curve.QuoteKind // deposit / future / swap
	Tenor        float64         // maturity in years (a future's period start)
	Span         float64         // Future only: underlying period length (0 ⇒ 0.25)
}

// SnapshotRateSource adapts the LiveQuotes cache to curve.QuoteSource over a
// fixed instrument universe: on each Refresh it reads the latest mid per
// configured instrument and emits the requested currency's strip.
type SnapshotRateSource struct {
	quotes      *LiveQuotes
	instruments []RateInstrument
}

// NewSnapshotRateSource binds an instrument universe to the cache. The universe
// is copied so later caller mutation cannot change the strip mid-run.
func NewSnapshotRateSource(quotes *LiveQuotes, instruments []RateInstrument) *SnapshotRateSource {
	cp := make([]RateInstrument, len(instruments))
	copy(cp, instruments)
	return &SnapshotRateSource{quotes: quotes, instruments: cp}
}

// Currencies returns the distinct configured currencies in first-seen order —
// the scheduler's job set.
func (s *SnapshotRateSource) Currencies() []string {
	seen := map[string]bool{}
	var out []string
	for _, in := range s.instruments {
		if !seen[in.Currency] {
			seen[in.Currency] = true
			out = append(out, in.Currency)
		}
	}
	return out
}

// RateQuotes implements curve.QuoteSource: for the requested currency it emits
// one RateQuote per configured instrument that has a positive latest mid,
// TOGETHER WITH how much of that currency's configured strip those quotes are.
//
// Instruments with no tick yet (or a garbage/non-positive mid) are still
// skipped — a calibration cannot use a price that does not exist, and refusing
// the whole currency for one dead instrument would take its curve out of
// service for a fixable data problem. WHAT CHANGED IN #908 IS THAT THE SKIP IS
// NOW STATED. The returned Strip names every configured instrument that did not
// make it and why, so a curve calibrated from six of nine points is no longer
// the same artifact as one calibrated from a six-point strip that is complete.
//
// COVERAGE IS COUNTED OVER THE REQUESTED CURRENCY ONLY. An instrument of
// another currency is not "missing" from this strip — it belongs to a different
// curve with its own job and its own report, and folding it in here would make
// every currency look permanently short by the size of its siblings.
//
// asOf is unused (the cache is always "latest"); the calibrator stamps the
// curve point-in-time.
func (s *SnapshotRateSource) RateQuotes(_ context.Context, currency string, _ time.Time) (curve.Strip, error) {
	strip := curve.Strip{}
	for _, in := range s.instruments {
		if in.Currency != currency {
			continue
		}
		strip.Coverage.Configured++
		ev, ok := s.quotes.Latest(in.InstrumentID)
		if !ok {
			strip.Coverage.Missing = append(strip.Coverage.Missing,
				curve.MissingQuote{InstrumentID: in.InstrumentID, Reason: curve.MissingNoQuote})
			continue
		}
		mid, ok := midPrice(ev)
		if !ok || mid <= 0 {
			strip.Coverage.Missing = append(strip.Coverage.Missing,
				curve.MissingQuote{InstrumentID: in.InstrumentID, Reason: curve.MissingUnusableMid})
			continue
		}
		strip.Quotes = append(strip.Quotes, curve.RateQuote{Kind: in.Kind, Tenor: in.Tenor, Span: in.Span, Value: mid})
		strip.Coverage.Quoted++
	}
	return strip, nil
}

// ConfiguredCurrencies reports how many instruments the strip declares per
// currency — the denominator, read off the configuration BEFORE anything has
// ticked. The composition root logs it at startup so an operator can see what
// this pod believes each curve needs, in the same place it already reads the
// cached instrument universe and the retention horizon.
func (s *SnapshotRateSource) ConfiguredCurrencies() map[string]int {
	out := make(map[string]int, len(s.instruments))
	for _, in := range s.instruments {
		out[in.Currency]++
	}
	return out
}

var _ curve.QuoteSource = (*SnapshotRateSource)(nil)

// ParseRateInstruments parses the calibration reference spec into the rate
// universe. Each comma-separated entry is
//
//	<instrument_id>:<currency>:<kind>:<tenor>[:<span>]
//
// where kind is deposit|future|swap, tenor is years to maturity, and the
// optional span is a future's underlying period length in years. An empty spec
// yields nil (calibration stays disabled). A malformed entry is a hard error —
// a mistyped reference set must fail loud at startup, not silently drop an
// instrument from the strip.
func ParseRateInstruments(spec string) ([]RateInstrument, error) {
	var out []RateInstrument
	for _, raw := range strings.Split(spec, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 4 || len(parts) > 5 {
			return nil, fmt.Errorf("livequote: rate instrument %q: want instrument:ccy:kind:tenor[:span]", entry)
		}
		id, ccy := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if id == "" || ccy == "" {
			return nil, fmt.Errorf("livequote: rate instrument %q: empty instrument or currency", entry)
		}
		kind, err := parseQuoteKind(parts[2])
		if err != nil {
			return nil, fmt.Errorf("livequote: rate instrument %q: %w", entry, err)
		}
		tenor, err := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
		if err != nil || tenor <= 0 {
			return nil, fmt.Errorf("livequote: rate instrument %q: tenor must be a positive number", entry)
		}
		in := RateInstrument{InstrumentID: id, Currency: ccy, Kind: kind, Tenor: tenor}
		if len(parts) == 5 {
			span, err := strconv.ParseFloat(strings.TrimSpace(parts[4]), 64)
			if err != nil || span < 0 {
				return nil, fmt.Errorf("livequote: rate instrument %q: span must be a non-negative number", entry)
			}
			in.Span = span
		}
		out = append(out, in)
	}
	return out, nil
}

func parseQuoteKind(s string) (curve.QuoteKind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "deposit", "dep":
		return curve.Deposit, nil
	case "future", "fut", "fra":
		return curve.Future, nil
	case "swap":
		return curve.Swap, nil
	default:
		return 0, fmt.Errorf("unknown quote kind %q (want deposit|future|swap)", s)
	}
}
