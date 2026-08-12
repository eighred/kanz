package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	// Aliased: this package's tests already bind the identifier `dec` to a
	// Decimal constructor, and a collision here would be a build error in test
	// builds only — green locally, red the moment anyone runs the suite.
	decimal "github.com/eighred/kanz/internal/dec"
)

// Resolution is a bar's interval — the series it belongs to.
//
// IT IS PART OF A BAR'S IDENTITY, not a property of it. A 1-minute candle and
// the 1-hour candle covering it are different observations of the same market at
// the same instant, and a store that mixed them into one series would answer
// "what did BTC-USDT do at 12:00" with two rows and no way to say which was
// meant.
type Resolution string

const (
	Resolution1m Resolution = "1m"
	Resolution1h Resolution = "1h"
	Resolution1d Resolution = "1d"
)

// resolutions is the set this platform stores, and the interval each one means.
//
// ONE MINUTE IS THE BASE and the coarser two are ROLLUPS DERIVED FROM IT, never
// ingested independently (the horizon ruling on #416). Two sources for the same
// candle is two answers to one question, and the day they disagree there is
// nothing to arbitrate between them.
var resolutions = map[Resolution]time.Duration{
	Resolution1m: time.Minute,
	Resolution1h: time.Hour,
	Resolution1d: 24 * time.Hour,
}

// Interval returns the duration r covers.
func (r Resolution) Interval() (time.Duration, bool) {
	d, ok := resolutions[r]
	return d, ok
}

// Valid reports whether r is a resolution this platform stores.
func (r Resolution) Valid() bool {
	_, ok := resolutions[r]
	return ok
}

// ResolutionOf names the series a bar belongs to, from its own interval.
//
// market.v1.Bar carries open_time and close_time and NO interval field, so the
// resolution is DERIVED rather than declared. An interval matching no known
// resolution is REFUSED (ok=false) rather than stored under an invented name: a
// 47-second bar is a defect in whatever produced it, and quietly keeping it
// creates a series nothing queries and nobody knows exists.
func ResolutionOf(openTime, closeTime time.Time) (Resolution, bool) {
	d := closeTime.Sub(openTime)
	for r, want := range resolutions {
		if d == want {
			return r, true
		}
	}
	return "", false
}

// Bar is one OHLCV candle — the durable shape of market.v1.Bar.
//
// PRICES AND VOLUME ARE EXACT common.v1.Decimal, never float, for the reason
// every other money path here is: a candle's close becomes a mark, a mark values
// an order, and a rounding error there is a wrong number that looks right.
type Bar struct {
	// InstrumentID is the canonical Kanz id, as on market.v1.MarketDataEvent.
	InstrumentID string

	// Venue is the ISO 10383 MIC the candle came from, and it is part of the
	// bar's IDENTITY rather than a label on it.
	//
	// A bar from one exchange is not a bar from another: different books,
	// different last trades, a different close for the same minute. A model
	// trained on a composite candle and executed against one venue's book shows a
	// backtest/live divergence that reads as "the strategy stopped working"
	// rather than as a data defect. #407 established the same rule one field
	// over — this platform does not smooth over which venue a price came from.
	Venue string

	// Resolution is the series this bar belongs to.
	Resolution Resolution

	// BucketStart is the inclusive start of the interval — the bar's
	// observation time, the domain time it applies to.
	BucketStart time.Time

	Open, High, Low, Close *commonpb.Decimal

	// Volume is the total traded quantity over the interval.
	Volume *commonpb.Decimal

	// TradeCount is how many trades were aggregated. ZERO IS MEANINGFUL: an
	// interval in which nothing traded is a real observation, not a gap, and an
	// indicator that cannot tell the two apart will invent movement where the
	// market was simply closed.
	TradeCount int64

	// KnowledgeTime is when Kanz learned this bar.
	//
	// A VENUE RESTATING A CANDLE IS A NEW KnowledgeTime, NOT AN OVERWRITE — the
	// bitemporal contract price_observations already keeps. A backtest as of T
	// reads only KnowledgeTime <= T, so a correction that arrived afterwards
	// cannot leak backwards into it. A store that overwrote would make every
	// historical evaluation quietly optimistic, in the direction nobody checks.
	KnowledgeTime time.Time
}

// ErrInvalidBar rejects a malformed bar before it reaches the series.
var ErrInvalidBar = errors.New("store: invalid bar")

// Validate refuses a bar that would corrupt the series.
//
// IT CHECKS THE OHLC RELATIONSHIP, not merely presence. A high below the low, or
// a close outside [low, high], is a bar no market produced — and once stored it
// becomes an indicator's input, a model's feature and a backtest's price.
// Refusing at this boundary is the only place it stays cheap: every layer after
// it treats a stored bar as fact.
func (b Bar) Validate() error {
	switch {
	case b.InstrumentID == "":
		return fmt.Errorf("%w: instrument_id is required", ErrInvalidBar)
	case b.Venue == "":
		return fmt.Errorf("%w: venue is required — a candle with no venue cannot be matched to the "+
			"book an order would execute against", ErrInvalidBar)
	case !b.Resolution.Valid():
		return fmt.Errorf("%w: resolution %q is not one this platform stores", ErrInvalidBar, b.Resolution)
	case b.BucketStart.IsZero():
		return fmt.Errorf("%w: bucket_start is required", ErrInvalidBar)
	case b.KnowledgeTime.IsZero():
		return fmt.Errorf("%w: knowledge_time is required — without it the bar cannot be read "+
			"point-in-time, which is this store's whole contract", ErrInvalidBar)
	case b.TradeCount < 0:
		return fmt.Errorf("%w: trade_count is negative", ErrInvalidBar)
	}
	for _, f := range []struct {
		name string
		d    *commonpb.Decimal
	}{{"open", b.Open}, {"high", b.High}, {"low", b.Low}, {"close", b.Close}, {"volume", b.Volume}} {
		if f.d == nil {
			return fmt.Errorf("%w: %s is required", ErrInvalidBar, f.name)
		}
		if !decimal.InDomain(f.d) {
			return fmt.Errorf("%w: %s is outside the computable decimal domain", ErrInvalidBar, f.name)
		}
	}
	// THE CANDLE MUST BE A CANDLE. Compared through dec.Cmp rather than a float
	// conversion: these are exact base-10 values, and the point of storing them
	// that way is not to leave the domain in order to compare them.
	if decimal.Cmp(b.Low, b.High) > 0 {
		return fmt.Errorf("%w: low is above high — no market produced this bar", ErrInvalidBar)
	}
	for _, f := range []struct {
		name string
		d    *commonpb.Decimal
	}{{"open", b.Open}, {"close", b.Close}} {
		if decimal.Cmp(f.d, b.High) > 0 {
			return fmt.Errorf("%w: %s is above high", ErrInvalidBar, f.name)
		}
		if decimal.Cmp(b.Low, f.d) > 0 {
			return fmt.Errorf("%w: %s is below low", ErrInvalidBar, f.name)
		}
	}
	return nil
}

// BarQuery reads a point-in-time-correct bar series.
type BarQuery struct {
	InstrumentID string
	Venue        string
	Resolution   Resolution

	// From is inclusive, To exclusive, on BucketStart.
	From, To time.Time

	// AsOf is the KNOWLEDGE HORIZON: only bars known by then are returned, and
	// each BucketStart collapses to its latest such version. Zero means now.
	//
	// This field is what makes a backtest honest. Without it a run "as of" last
	// year reads corrections that arrived last week and reports a strategy that
	// could not have existed.
	AsOf time.Time
}

// BarStore is the durable OHLCV series.
//
// SEPARATE FROM Store, deliberately. The scalar series answers "what was the
// mark"; this answers "what did the market do". They share the bitemporal
// contract — which is why they live in one package, so that contract is not
// implemented twice — but a consumer of one has no business reaching the other
// by accident.
type BarStore interface {
	// PutBars stores bars idempotently: re-putting an identical
	// (instrument, venue, resolution, bucket_start, knowledge_time) tuple is a
	// no-op. A restatement is a new knowledge_time, never a mutation. An invalid
	// bar fails the whole batch.
	PutBars(ctx context.Context, bars []Bar) error

	// Bars returns the point-in-time-correct series, ordered by BucketStart
	// ascending.
	Bars(ctx context.Context, q BarQuery) ([]Bar, error)
}

// ReadWriter is the whole market-data store: the scalar mark series and the
// candles.
//
// It exists so a composition root can hand ONE value to consumers that need
// both, without either interface growing methods that belong to the other. The
// two concrete stores (Memory, Postgres) implement it; a narrower consumer keeps
// taking Store or BarStore and is unable to reach the half it has no business
// with.
type ReadWriter interface {
	Store
	BarStore
}
