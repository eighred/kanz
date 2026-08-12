package bars

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketedge/trades"
	"github.com/eighred/kanz/pkg/bus"
)

// Subject is where completed candles publish.
//
// market.crypto.* ALREADY EXISTS and maps to the provisioned Kafka topic
// market.crypto (infra/kafka/topics-job.yaml), which market.crypto.trade uses.
// Riding it means no new topic, no new archival decision, and no new NATS
// grant — and it inherits that topic's existing "not archived" exemption, which
// is the honest place for a candle that can be re-derived from the venue.
const Subject = "market.crypto.bar"

const domain = "market"

// Publisher is the bus publish surface — satisfied by *bus.Producer, the same
// seam ingest.Engine takes for book snapshots.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Collector is the concurrent face of Fold: it observes trades as they arrive on
// each feed's goroutine, and publishes completed candles.
//
// FOLD ITSELF STAYS SINGLE-THREADED AND PURE — this is where the locking lives,
// exactly as trades.Tape owns its own mutex while the arithmetic beneath it does
// not. The two callers genuinely race: Observe runs on a feed goroutine per
// series while Flush runs on the shared tick loop, and a Flush interleaved
// mid-Observe would emit a candle missing the trade being applied.
type Collector struct {
	mu   sync.Mutex
	fold *Fold

	pub    Publisher
	tenant string
	logger *slog.Logger

	// late counts prints that arrived for a minute already published. A counter
	// rather than a log line per event: a disordered feed produces them in
	// floods, and a flood of log lines is how the one that mattered gets lost.
	late atomic.Int64
}

// NewCollector returns a Collector publishing to pub.
func NewCollector(pub Publisher, tenant string, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.Default()
	}
	return &Collector{fold: New(), pub: pub, tenant: tenant, logger: logger}
}

// Observe folds one trade and publishes the candle it closed, if any.
//
// PUBLISHING HERE RATHER THAN ON THE TICK is deliberate: a candle is complete
// the moment a trade from the next minute arrives, and holding it until a timer
// fires would add latency for no gain and make the publish order depend on a
// clock rather than on the data.
func (c *Collector) Observe(ctx context.Context, s Series, tr trades.Trade) {
	c.mu.Lock()
	closed, late := c.fold.Add(s, tr)
	c.mu.Unlock()

	if late {
		c.late.Add(1)
		return
	}
	if closed != nil {
		c.publish(ctx, *closed)
	}
}

// Flush publishes every candle whose minute has ended.
//
// It is what closes a QUIET series: Observe only discovers a boundary when a
// later trade arrives, so on a thin instrument the last candle of a lull would
// otherwise sit open indefinitely.
func (c *Collector) Flush(ctx context.Context, now time.Time) {
	c.mu.Lock()
	done := c.fold.Flush(now)
	c.mu.Unlock()

	for _, b := range done {
		c.publish(ctx, b)
	}
}

// LatePrints is how many trades arrived for a minute already published.
//
// NON-ZERO IS NOT FATAL AND IS NOT NOTHING. A disordered feed means the affected
// candles are missing volume they should have had, and the fold refuses to
// restate a published bar to fix it — so this number is the only evidence that
// the series is understated.
func (c *Collector) LatePrints() int64 { return c.late.Load() }

// publish maps a folded candle onto market.v1 and puts it on the bus.
//
// A CANDLE THAT CANNOT BE REPRESENTED EXACTLY IS DROPPED, LOUDLY, rather than
// published with a rounded price. dec.ToProtoScaled is the same boundary the
// rest of the capital path uses; a price that will not convert is a defect in
// the feed, and a rounded one does not read as wrong downstream — it reads as a
// real trade at a price nobody made.
func (c *Collector) publish(ctx context.Context, b Bar) {
	open, okOpen := dec.ToProtoScaled(b.Open)
	high, okHigh := dec.ToProtoScaled(b.High)
	low, okLow := dec.ToProtoScaled(b.Low)
	cl, okClose := dec.ToProtoScaled(b.Close)
	vol, okVol := dec.ToProtoScaled(b.Volume)
	if !okOpen || !okHigh || !okLow || !okClose || !okVol {
		c.logger.Error("BAR DROPPED — a price or volume is not representable as an exact Decimal, "+
			"and publishing a rounded one would put a trade at a price nobody made into the series "+
			"every indicator and backtest reads",
			"instrument", b.Series.InstrumentID, "venue", b.Series.Venue, "bucket", b.BucketStart)
		return
	}

	ev := &marketpb.MarketDataEvent{
		InstrumentId: b.Series.InstrumentID,
		Mic:          b.Series.Venue,
		// The event time is the bar's CLOSE, matching market.v1.Bar's own
		// contract: "The enclosing MarketDataEvent.event_time is the close_time."
		EventTime: timestamppb.New(b.BucketStart.Add(Resolution)),
		Data: &marketpb.MarketDataEvent_Bar{Bar: &marketpb.Bar{
			OpenTime:   timestamppb.New(b.BucketStart),
			CloseTime:  timestamppb.New(b.BucketStart.Add(Resolution)),
			Open:       open,
			High:       high,
			Low:        low,
			Close:      cl,
			Volume:     vol,
			TradeCount: uint64(b.TradeCount),
		}},
	}

	if err := c.pub.Publish(ctx, bus.Event{
		Subject:       Subject,
		EventType:     Subject,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        domain,
		EventTime:     b.BucketStart.Add(Resolution),
		// PARTITIONED BY INSTRUMENT, as every other market.v1 event is, so a
		// consumer folding one instrument's series reads it in order.
		PartitionKey: b.Series.InstrumentID,
		TenantID:     c.tenant,
		Payload:      ev,
	}); err != nil && ctx.Err() == nil {
		// NOT FATAL, AND NOT SILENT. A dropped candle is a hole in the series
		// that no downstream consumer can see — it does not look like an error,
		// it looks like a minute in which nothing traded.
		c.logger.Error("bar publish failed — this minute is now missing from the series and will "+
			"read downstream as a quiet market rather than as a gap",
			"instrument", b.Series.InstrumentID, "venue", b.Series.Venue,
			"bucket", b.BucketStart, "err", err)
	}
}

// Tee wraps a TradeSource so every trade reaches the collector on its way to the
// tape.
//
// A TEE RATHER THAN A SECOND CONSUMER, because a TradeSource has exactly one:
// two Recv loops on one feed would each see half the prints, and each would
// build a candle that looked plausible and was wrong. This also leaves
// internal/marketedge/trades untouched — the tape does not need to know a second
// thing reads its input.
func Tee(src trades.TradeSource, s Series, c *Collector) trades.TradeSource {
	return &tee{src: src, series: s, c: c}
}

type tee struct {
	src    trades.TradeSource
	series Series
	c      *Collector
}

func (t *tee) Recv(ctx context.Context) (trades.Trade, error) {
	tr, err := t.src.Recv(ctx)
	if err != nil {
		return tr, err
	}
	t.c.Observe(ctx, t.series, tr)
	return tr, nil
}
