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

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"

	"github.com/kanz-eng/kanz/internal/marketdata/store"
)

// Writer is the store boundary the Ingestor depends on — just the write side,
// so the ingester can be tested against a fake without the read surface.
type Writer interface {
	Put(ctx context.Context, obs []store.Observation) error
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
	obs, err := TranslateEvent(env, &ev)
	if err != nil {
		return err
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
