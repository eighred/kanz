package livequote

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kanz-eng/kanz/internal/risk/pricing/curve"
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
// one RateQuote per configured instrument that has a positive latest mid.
// Instruments with no tick yet (or a garbage/non-positive mid) are skipped,
// yielding a partial strip; the calibrator rejects an unusable set and its
// deny-on-garbage stance leaves the prior curve serving. asOf is unused (the
// cache is always "latest"); the calibrator stamps the curve point-in-time.
func (s *SnapshotRateSource) RateQuotes(_ context.Context, currency string, _ time.Time) ([]curve.RateQuote, error) {
	var out []curve.RateQuote
	for _, in := range s.instruments {
		if in.Currency != currency {
			continue
		}
		ev, ok := s.quotes.Latest(in.InstrumentID)
		if !ok {
			continue
		}
		mid, ok := midPrice(ev)
		if !ok || mid <= 0 {
			continue
		}
		out = append(out, curve.RateQuote{Kind: in.Kind, Tenor: in.Tenor, Span: in.Span, Value: mid})
	}
	return out, nil
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
