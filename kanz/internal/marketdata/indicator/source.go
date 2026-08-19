package indicator

import (
	"context"
	"fmt"
	"math"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

// THE SEAM THAT KEEPS THIS FROM BEING A LIBRARY NOBODY CALLS (#416 C1, #509).
//
// Source reads the point-in-time bar series and returns the indicator set as
// features, satisfying dataset.FeatureSource — the MLOPS-01f seam whose own doc
// says it is "where it joins in". It is built here rather than in
// internal/lake/dataset because an alpha engine will want the same readings
// without wanting a feature row, and one implementation is the rule.
//
// # POINT-IN-TIME IS THE WHOLE VALUE, and it is one struct field
//
// BarQuery.AsOf bounds the KNOWLEDGE horizon, not just the observation window.
// Its own doc states what it buys: "without it a backtest 'as of' last year reads
// corrections that arrived last week and reports a strategy that could not have
// existed". Every read here sets it, and it is set to the SAME asOf that bounds
// To — a query that bounded one and not the other would still leak, and would
// leak invisibly, because the numbers stay plausible.
//
// # A MISSING INDICATOR IS AN ABSENT KEY, never a zero
//
// The series functions answer (value, ok); this drops the key when ok is false.
// Writing a zero instead would put "sold off hard" (rsi_14=0) or "no volatility"
// (realized_vol_20=0) into a training row that means "we did not have enough
// history", and a trainer cannot tell those apart from the value alone. dataset's
// Row.Complete already models exactly this, so the convention matches what the
// materializer around it does with its own features.

// FeatureSource is the interface this satisfies, restated here as a compile-time
// assertion rather than imported — internal/lake/dataset imports this package's
// sibling (store) and would import this one, so depending on it back would be a
// cycle. The signature is pinned by the var below; if dataset's seam changes,
// that line stops compiling.
type FeatureSource interface {
	FeaturesAsOf(ctx context.Context, instrumentID string, asOf time.Time) (map[string]float64, error)
}

var _ FeatureSource = (*Source)(nil)

// Config parameterises which series is read and how much of it.
type Config struct {
	// Venue is the MIC whose candles to read. REQUIRED, and there is no "any"
	// value on purpose: store.Bar's own doc makes venue part of a bar's IDENTITY,
	// because a model trained on a composite candle and executed against one
	// venue's book shows a divergence that reads as "the strategy stopped
	// working" rather than as a data defect.
	Venue string

	// Resolution defaults to 1m, the base series. The coarser two are rollups.
	Resolution store.Resolution

	// Lookback is how many bars to read back from asOf. Zero ⇒ DefaultLookback.
	//
	// It must exceed the longest period any indicator here uses, and by enough
	// that the recursive ones have decayed their seed — see DefaultLookback.
	Lookback int
}

// DefaultLookback is 200 bars.
//
// NOT "the longest period", which is 26 (MACD's slow EMA) or 34 with its signal.
// A recursive average seeded with the mean of its first n carries that seed for
// roughly 3n bars, so a window sized to the period alone returns a number that is
// an EMA in form and a warm-up artifact in value — plausible, in range, and
// wrong. 200 puts every indicator here at least five decay lengths past its seed.
//
// It is also cheap: 200 one-minute bars is 200 rows for one instrument, and the
// query is indexed on exactly (instrument, venue, resolution, bucket_start).
const DefaultLookback = 200

// Source computes indicator features from the durable bar series.
type Source struct {
	bars store.BarStore
	cfg  Config
}

// NewSource wraps a bar store. It refuses an empty venue rather than reading
// across all of them, because "every venue's bars for this instrument" is a
// composite series and this platform does not have one.
func NewSource(bars store.BarStore, cfg Config) (*Source, error) {
	if bars == nil {
		return nil, fmt.Errorf("indicator: nil bar store")
	}
	if cfg.Venue == "" {
		return nil, fmt.Errorf("indicator: venue is required — a bar's venue is part of its " +
			"identity, and reading across venues would build a composite candle this platform " +
			"deliberately does not have")
	}
	if cfg.Resolution == "" {
		cfg.Resolution = store.Resolution1m
	}
	if !cfg.Resolution.Valid() {
		return nil, fmt.Errorf("indicator: %q is not a stored resolution", cfg.Resolution)
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = DefaultLookback
	}
	return &Source{bars: bars, cfg: cfg}, nil
}

// FeaturesAsOf returns the indicator readings computable from the history known
// at asOf. A store failure propagates; insufficient history does not — it simply
// yields fewer keys.
func (s *Source) FeaturesAsOf(ctx context.Context, instrumentID string, asOf time.Time) (map[string]float64, error) {
	interval, ok := s.cfg.Resolution.Interval()
	if !ok {
		return nil, fmt.Errorf("indicator: resolution %q has no interval", s.cfg.Resolution)
	}
	bars, err := s.bars.Bars(ctx, store.BarQuery{
		InstrumentID: instrumentID,
		Venue:        s.cfg.Venue,
		Resolution:   s.cfg.Resolution,
		From:         asOf.Add(-time.Duration(s.cfg.Lookback) * interval),
		To:           asOf,
		// BOTH AXES AT asOf. Bounding To without AsOf reads corrections that
		// arrived later — the numbers stay plausible and the backtest becomes a
		// strategy that could not have existed.
		AsOf: asOf,
	})
	if err != nil {
		return nil, err
	}
	return Features(bars), nil
}

// Features computes every indicator over an ascending bar series.
//
// EXPORTED SEPARATELY FROM THE STORE READ so an alpha engine holding bars in
// memory computes the identical set — the readings a strategy is backtested on
// and the readings it trades on must come from one function, or the divergence
// between them is the first thing a live loss gets blamed on and the last thing
// anyone finds.
//
// # ONLY THE CONTIGUOUS TAIL IS READ (#416)
//
// A period here is a DURATION, not an element count, and that is only true over
// bars with no holes in them. bars/fold.go emits NO BAR for a minute in which
// nothing traded, so a gapped slice is the normal shape of this input, not an
// exotic one — and over one, SMA(closes, 20) averages the last twenty ELEMENTS
// across however much wall-clock time they happen to span. It stays in range, it
// stays plausible, and it answers a different question than its name.
//
// Measured on a 1m series with nine of every ten minutes absent, the unguarded
// version reported rsi_14 = 100 ("maximally overbought") from fifteen prints
// spread over 150 minutes.
//
// store.ContiguousSuffix trims to the longest unbroken run ending at the most
// recent bar, so every reading below is over a window of the duration it claims.
// The existing (value, ok) contract then does the rest: a run too short for a
// period yields no key, which already means "not enough history" and now covers
// "not enough UNBROKEN history" as well. Both are absences, and absence is
// already what this package returns rather than a number.
func Features(bars []store.Bar) map[string]float64 {
	out := map[string]float64{}
	closes, highs, lows, ok := columns(store.ContiguousSuffix(bars))
	if !ok {
		// A NON-CONVERTIBLE PRICE IS NOT A ZERO. Returning an empty set says "no
		// readings"; substituting zeros would say the market printed at zero.
		return out
	}

	// Written out rather than looped: Go cannot spread a (value, ok) pair into a
	// helper's arguments, and every shape that works around that hides which
	// reading is which behind an index.
	if v, ok := SMA(closes, 20); ok {
		out["sma_20"] = v
	}
	if v, ok := SMA(closes, 50); ok {
		out["sma_50"] = v
	}
	if v, ok := EMA(closes, 12); ok {
		out["ema_12"] = v
	}
	if v, ok := EMA(closes, 26); ok {
		out["ema_26"] = v
	}
	if v, ok := RSI(closes, 14); ok {
		out["rsi_14"] = v
	}
	if v, ok := RealizedVol(closes, 20); ok {
		out["realized_vol_20"] = v
	}
	if v, ok := ATR(highs, lows, closes, 14); ok {
		out["atr_14"] = v
	}

	if m, ok := MACD(closes, 12, 26, 9); ok {
		out["macd"] = m.MACD
		out["macd_signal"] = m.Signal
		out["macd_histogram"] = m.Histogram
	}
	if b, ok := Bollinger(closes, 20, 2); ok {
		out["bb_middle"] = b.Middle
		out["bb_upper"] = b.Upper
		out["bb_lower"] = b.Lower
		// The normalised width is the one that compares across instruments; the
		// raw span does not, and a model fed the raw span learns the price level.
		out["bb_width"] = b.Width
	}
	return out
}

// columns splits bars into the three price series, converting Decimal to float
// ONCE at this boundary.
//
// ok=false if any price is unconvertible. All-or-nothing rather than
// best-effort: a series with holes silently shortens every window, so a 20-period
// SMA would average 20 values spanning 25 minutes and report as if it spanned 20.
func columns(bars []store.Bar) (closes, highs, lows []float64, ok bool) {
	closes = make([]float64, len(bars))
	highs = make([]float64, len(bars))
	lows = make([]float64, len(bars))
	for i, b := range bars {
		c, cok := decimalFloat(b.Close)
		h, hok := decimalFloat(b.High)
		l, lok := decimalFloat(b.Low)
		if !cok || !hok || !lok {
			return nil, nil, nil, false
		}
		closes[i], highs[i], lows[i] = c, h, l
	}
	return closes, highs, lows, true
}

// decimalFloat converts an exact Decimal to float64 for analytic use, refusing a
// nil or a value that does not survive the conversion finitely.
func decimalFloat(d *commonpb.Decimal) (float64, bool) {
	if d == nil {
		return 0, false
	}
	f := float64(d.GetCoefficient()) * math.Pow10(int(d.GetExponent()))
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}
