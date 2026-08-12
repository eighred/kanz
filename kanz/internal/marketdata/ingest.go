// Package marketdata is the market-data ingestion module (MODEL-01b): the
// bus→store bridge that folds market.v1 events into the point-in-time price
// history (internal/marketdata/store). The risk *analytics* that read the
// history live elsewhere (MODEL-01c+); this package owns translation + write.
//
// # Scope split (mirrors the risk-engine RISK-04/05 split)
//
// Ingestor is the thin routing/translation layer: it discriminates the market
// oneof, maps each variant to the store.Observation shape, and writes through
// the Writer boundary. The store owns durability + the point-in-time read; this
// package owns "which market event becomes which kind of mark".
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/store"
)

// Writer is the store boundary the Ingestor depends on — just the write side,
// so the ingester can be tested against a fake without the read surface.
//
// PutBars IS PART OF THIS INTERFACE, NOT AN OPTION, and that is deliberate
// (#425). A market.v1.Bar carries open, high, low, close, volume and trade_count;
// for the platform's whole life ingest kept the close and dropped the rest, and
// nothing anywhere reported a loss — the series simply did not exist, so no
// query missed it. Making the bar sink optional would recreate exactly that: an
// ingestor wired without one would run green while discarding the data every
// indicator, model feature and backtest is made of.
//
// It is one method on one interface with one production implementation. The
// cost of requiring it is a line in a test fake; the cost of not requiring it is
// silence.
type Writer interface {
	Put(ctx context.Context, obs []store.Observation) error
	PutBars(ctx context.Context, bars []store.Bar) error
}

// Ingestor is the bus.EventHandler that folds market.v1 events into the price
// history. Construct once and pass Handler to as many bus.Consumer.Subscribe
// calls as there are market subjects.
type Ingestor struct {
	writer Writer
}

// NewIngestor returns an Ingestor writing to w.
func NewIngestor(w Writer) (*Ingestor, error) {
	if w == nil {
		return nil, errors.New("marketdata: writer is nil")
	}
	return &Ingestor{writer: w}, nil
}

// Handler is the bus.EventHandler value wired into bus.Consumer.Subscribe. It
// unmarshals a market.v1.MarketDataEvent, translates it to an Observation, and
// writes it. A non-nil return nacks/DLQs the delivery (EVT-17e), so a malformed
// or untranslatable market event surfaces to the operator rather than being
// silently dropped — market data loss must be loud.
//
// Batch carriage (market.v1.MarketDataBatch) is not wired here: no batching
// producer exists yet and the envelope event_type does not distinguish a batch
// from a single event. TranslateBatch is provided for when that convention
// lands — translation is already per-event, so a batch is just a loop.
func (i *Ingestor) Handler(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	var ev marketpb.MarketDataEvent
	if err := proto.Unmarshal(payload, &ev); err != nil {
		return fmt.Errorf("marketdata: %s unmarshal: %w", env.GetEventType(), err)
	}
	// Prices off market.> are the platform's least trusted numbers, and this is the
	// store-writing path rather than the mark fold that was bounded first (#95).
	// TranslateEvent hands the event's Decimals on undecoded, so this is the last
	// point the whole message is in one place to refuse it.
	if field, in := dec.InDomainDeep(&ev); !in {
		return fmt.Errorf("marketdata: %s carries an out-of-domain exponent at %s", env.GetEventType(), field)
	}
	obs, err := TranslateEvent(env, &ev)
	if err != nil {
		return err
	}
	// THE CANDLE IS KEPT WHOLE, AND THE CLOSE IS STILL A MARK.
	//
	// Both, not either. The scalar observation above is what the risk and pricing
	// planes already read as "the mark"; the bar is what an indicator, a model
	// feature and a backtest are made of. Writing only the first is the defect
	// #425 exists to close; writing only the second would break every existing
	// consumer of the price series.
	//
	// The bar goes FIRST: if it fails the delivery is nacked and redelivered, and
	// the observation write is idempotent on its bitemporal key, so a retry
	// re-runs both harmlessly. The other order would leave a mark with no candle
	// behind it after a partial failure, which is the silence this is fixing.
	if bar, ok, err := TranslateBar(env, &ev); err != nil {
		return err
	} else if ok {
		if err := i.writer.PutBars(ctx, []store.Bar{bar}); err != nil {
			return err
		}
	}
	return i.writer.Put(ctx, []store.Observation{obs})
}

// TranslateEvent maps one market.v1.MarketDataEvent to an Observation. The
// envelope supplies the bitemporal knowledge_time (ingestion_time — when Kanz
// received the event) and the partition_key / event_time fallbacks.
//
// Mark mapping by market variant:
//   - Bar   → CLOSE  (bar.close at the bar's close_time)
//   - Trade → LAST   (the transacted price)
//   - Quote → MID    (mid of bid/ask)
//
// currency_code is left empty — market.v1 carries none; a later reference-data
// join (reference.v1.InstrumentReference.currency_code) fills it.
func TranslateEvent(env *envelopepb.Envelope, ev *marketpb.MarketDataEvent) (store.Observation, error) {
	instrument := ev.GetInstrumentId()
	if instrument == "" {
		instrument = env.GetPartitionKey()
	}
	obsTime := tsToTime(ev.GetEventTime())
	if obsTime.IsZero() {
		obsTime = tsToTime(env.GetEventTime())
	}
	// knowledge_time = when Kanz learned the value: ingestion_time, falling back
	// to publish_time, then the observation time itself.
	knowTime := tsToTime(env.GetIngestionTime())
	if knowTime.IsZero() {
		knowTime = tsToTime(env.GetPublishTime())
	}
	if knowTime.IsZero() {
		knowTime = obsTime
	}

	obs := store.Observation{
		InstrumentID:    instrument,
		ObservationTime: obsTime,
		KnowledgeTime:   knowTime,
	}
	switch d := ev.GetData().(type) {
	case *marketpb.MarketDataEvent_Bar:
		obs.Kind = store.PriceKindClose
		obs.Price = d.Bar.GetClose()
		if ct := tsToTime(d.Bar.GetCloseTime()); !ct.IsZero() {
			obs.ObservationTime = ct
		}
	case *marketpb.MarketDataEvent_Trade:
		obs.Kind = store.PriceKindLast
		obs.Price = d.Trade.GetPrice()
	case *marketpb.MarketDataEvent_Quote:
		obs.Kind = store.PriceKindMid
		obs.Price = midDecimal(d.Quote.GetBidPrice(), d.Quote.GetAskPrice())
	default:
		return store.Observation{}, fmt.Errorf("marketdata: %s has no market data variant", env.GetEventType())
	}
	if obs.Price == nil {
		return store.Observation{}, fmt.Errorf("marketdata: %s missing price for %s", env.GetEventType(), instrument)
	}
	return obs, nil
}

// TranslateBar turns a market.v1 Bar event into the durable candle (#425).
//
// ok IS FALSE FOR A NON-BAR EVENT — a trade or a quote is not a candle, and this
// is not an error. It is an error only when the event IS a bar and cannot be
// stored as one, because that is data the platform was given and could not keep.
//
// THE INTERVAL DECIDES THE SERIES. market.v1.Bar carries open_time and
// close_time and no resolution field, so the series is derived from the two. An
// interval matching no resolution this platform stores is REFUSED rather than
// filed under an invented name: a 47-second candle is a defect in whatever
// produced it, and keeping it quietly creates a series nothing queries and
// nobody knows exists.
//
// THE VENUE COMES FROM THE EVENT'S mic, and a bar without one is refused. A
// candle that cannot be matched to the book an order would execute against is a
// price the platform cannot honestly trade on (#407, one field over).
func TranslateBar(env *envelopepb.Envelope, ev *marketpb.MarketDataEvent) (store.Bar, bool, error) {
	d, isBar := ev.GetData().(*marketpb.MarketDataEvent_Bar)
	if !isBar {
		return store.Bar{}, false, nil
	}
	b := d.Bar
	openTime, closeTime := tsToTime(b.GetOpenTime()), tsToTime(b.GetCloseTime())
	if openTime.IsZero() || closeTime.IsZero() {
		return store.Bar{}, false, fmt.Errorf("marketdata: %s is a bar with no open_time/close_time, "+
			"so the interval it covers is unknowable", env.GetEventType())
	}
	res, ok := store.ResolutionOf(openTime, closeTime)
	if !ok {
		return store.Bar{}, false, fmt.Errorf("marketdata: %s is a bar covering %s, which is not a "+
			"resolution this platform stores", env.GetEventType(), closeTime.Sub(openTime))
	}
	// ONLY 1m IS INGESTED. The coarser series are ROLLUPS derived from this one,
	// and this is the seam where that stops being a paragraph.
	//
	// The store can hold 1h and 1d — a rollup job writes them — but accepting them
	// from a venue too would give one candle two sources, and the day they
	// disagree there is nothing to arbitrate between them: no rule says which is
	// right, and both are stamped as observed fact. The failure is silent, because
	// each is individually well-formed.
	//
	// Refusing nacks the delivery, which surfaces as a DLQ entry rather than a
	// gap. That is the intended direction: a venue whose hourly feed we do not
	// want is an operator decision to make once, loudly, not a series that
	// quietly fills with a second opinion.
	if res != store.Resolution1m {
		return store.Bar{}, false, fmt.Errorf("marketdata: %s is a %s bar — only 1m is INGESTED, and "+
			"1h/1d are derived from it by rollup; a second source for the same candle is two answers "+
			"to one question", env.GetEventType(), res)
	}
	if ev.GetMic() == "" {
		return store.Bar{}, false, fmt.Errorf("marketdata: %s is a bar with no mic — a candle that "+
			"cannot be attributed to a venue cannot be matched to the book an order executes against",
			env.GetEventType())
	}

	knowTime := tsToTime(env.GetIngestionTime())
	if knowTime.IsZero() {
		knowTime = tsToTime(env.GetPublishTime())
	}
	if knowTime.IsZero() {
		knowTime = closeTime
	}

	bar := store.Bar{
		InstrumentID:  ev.GetInstrumentId(),
		Venue:         ev.GetMic(),
		Resolution:    res,
		BucketStart:   openTime,
		Open:          b.GetOpen(),
		High:          b.GetHigh(),
		Low:           b.GetLow(),
		Close:         b.GetClose(),
		Volume:        b.GetVolume(),
		TradeCount:    int64(b.GetTradeCount()),
		KnowledgeTime: knowTime,
	}
	if err := bar.Validate(); err != nil {
		return store.Bar{}, false, fmt.Errorf("marketdata: %s: %w", env.GetEventType(), err)
	}
	return bar, true, nil
}

// TranslateBatch maps a MarketDataBatch to one Observation per event, sharing
// the batch envelope. Provided for when batch carriage is wired (see Handler).
func TranslateBatch(env *envelopepb.Envelope, batch *marketpb.MarketDataBatch) ([]store.Observation, error) {
	events := batch.GetEvents()
	out := make([]store.Observation, 0, len(events))
	for _, ev := range events {
		o, err := TranslateEvent(env, ev)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// midDecimal returns (bid+ask)/2 exactly: align exponents to the more precise
// side, sum the coefficients, then divide by two as ×5 with exponent−1 (so the
// halving never loses a fractional unit). nil on either side ⇒ nil.
func midDecimal(bid, ask *commonpb.Decimal) *commonpb.Decimal {
	if bid == nil || ask == nil {
		return nil
	}
	exp := bid.Exponent
	if ask.Exponent < exp {
		exp = ask.Exponent
	}
	b := bid.Coefficient * pow10(bid.Exponent-exp)
	a := ask.Coefficient * pow10(ask.Exponent-exp)
	return &commonpb.Decimal{Coefficient: (a + b) * 5, Exponent: exp - 1}
}

// pow10 returns 10^n for small non-negative n (exponent alignment deltas).
func pow10(n int32) int64 {
	out := int64(1)
	for ; n > 0; n-- {
		out *= 10
	}
	return out
}

// tsToTime converts a proto timestamp to a UTC time.Time, mapping nil to the
// zero time so callers can apply fallbacks (AsTime on nil yields the epoch,
// which would defeat the zero-check).
func tsToTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}
