package bars

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketedge/trades"
	"github.com/eighred/kanz/pkg/bus"
)

type capture struct {
	events []bus.Event
	err    error
}

func (c *capture) Publish(_ context.Context, e bus.Event) error {
	if c.err != nil {
		return c.err
	}
	c.events = append(c.events, e)
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func collector(pub Publisher) *Collector { return NewCollector(pub, "acme", quiet()) }

func barOf(t *testing.T, e bus.Event) *marketpb.Bar {
	t.Helper()
	ev, ok := e.Payload.(*marketpb.MarketDataEvent)
	if !ok {
		t.Fatalf("payload is %T, want *marketpb.MarketDataEvent", e.Payload)
	}
	d, ok := ev.GetData().(*marketpb.MarketDataEvent_Bar)
	if !ok {
		t.Fatalf("event carries %T, want a Bar", ev.GetData())
	}
	return d.Bar
}

// THE CANDLE REACHES THE BUS IN THE SHAPE market-data ALREADY STORES.
//
// internal/marketdata.TranslateBar derives the resolution from open_time and
// close_time and REFUSES anything that is not exactly one minute, requires a
// mic, and validates the OHLC relationship. A producer that got any of those
// wrong would publish candles the consumer nacks — and the series would stay
// empty for a reason nobody would look for here.
func TestAClosedCandleIsPublishedAsAOneMinuteBar(t *testing.T) {
	cap := &capture{}
	c := collector(cap)
	ctx := context.Background()

	c.Observe(ctx, btc(), tr(100, 1, 0))
	c.Observe(ctx, btc(), tr(130, 2, 10))
	c.Observe(ctx, btc(), tr(90, 3, 20))
	c.Observe(ctx, btc(), tr(110, 4, 30))
	c.Observe(ctx, btc(), tr(200, 1, 60)) // closes minute 0

	if len(cap.events) != 1 {
		t.Fatalf("published %d events, want 1", len(cap.events))
	}
	e := cap.events[0]
	if e.Subject != Subject || e.EventType != Subject {
		t.Errorf("subject/type = %q/%q, want %q", e.Subject, e.EventType, Subject)
	}
	if e.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event class = %v, want FACT — a candle records what happened and instructs nobody",
			e.EventClass)
	}
	if e.PartitionKey != "BTC-USDT" {
		t.Errorf("partition key = %q, want the instrument, so one series is read in order", e.PartitionKey)
	}
	if e.TenantID != "acme" {
		t.Errorf("tenant = %q, want acme", e.TenantID)
	}

	b := barOf(t, e)
	if got := b.GetCloseTime().AsTime().Sub(b.GetOpenTime().AsTime()); got != time.Minute {
		t.Fatalf("interval = %s, want 1m — TranslateBar derives the resolution from these two and "+
			"refuses anything else", got)
	}
	// COMPARED AS VALUES, not as coefficients. ToProtoScaled normalises to a
	// fixed scale, so 100 is not coefficient 100 — asserting the raw coefficient
	// would pin an internal representation and break the day that scale changes,
	// while saying nothing about whether the price is right.
	for _, f := range []struct {
		name string
		got  *commonpb.Decimal
		want string
	}{
		{"open", b.GetOpen(), "100"}, {"high", b.GetHigh(), "130"},
		{"low", b.GetLow(), "90"}, {"close", b.GetClose(), "110"},
		{"volume", b.GetVolume(), "10"},
	} {
		if got := dec.FromProto(f.got).RatString(); got != f.want {
			t.Errorf("%s = %s, want %s", f.name, got, f.want)
		}
	}
	if b.GetTradeCount() != 4 {
		t.Errorf("trade_count = %d, want 4", b.GetTradeCount())
	}

	// The event must carry the venue: TranslateBar refuses a bar with no mic,
	// because a candle that cannot be attributed to a venue cannot be matched to
	// the book an order executes against.
	ev := e.Payload.(*marketpb.MarketDataEvent)
	if ev.GetMic() != "XBIN" {
		t.Errorf("mic = %q, want XBIN", ev.GetMic())
	}
	// And it must survive the wire.
	if _, err := proto.Marshal(ev); err != nil {
		t.Fatalf("the published event does not marshal: %v", err)
	}
}

// FLUSH IS WHAT CLOSES A QUIET SERIES, and it must publish exactly once.
func TestFlushPublishesACompletedCandleOnce(t *testing.T) {
	cap := &capture{}
	c := collector(cap)
	ctx := context.Background()

	c.Observe(ctx, btc(), tr(100, 1, 0))
	c.Flush(ctx, t0.Add(90*time.Second))
	if len(cap.events) != 1 {
		t.Fatalf("published %d events, want 1", len(cap.events))
	}
	c.Flush(ctx, t0.Add(10*time.Minute))
	if len(cap.events) != 1 {
		t.Fatalf("a second flush republished the candle: %d events", len(cap.events))
	}
}

// THE MINUTE IN PROGRESS IS NOT PUBLISHED. A partial bar in a store whose
// contract is "this is what happened" is a lie the next reader cannot detect.
func TestFlushPublishesNothingForTheMinuteInProgress(t *testing.T) {
	cap := &capture{}
	c := collector(cap)
	ctx := context.Background()

	c.Observe(ctx, btc(), tr(100, 1, 0))
	c.Flush(ctx, t0.Add(30*time.Second))
	if len(cap.events) != 0 {
		t.Fatalf("published the in-progress minute: %d events", len(cap.events))
	}
}

// A LATE PRINT IS COUNTED, not published and not folded. Non-zero is the only
// evidence that the affected candles are understated, because the fold refuses
// to restate a bar it already published.
func TestLatePrintsAreCountedAndNotPublished(t *testing.T) {
	cap := &capture{}
	c := collector(cap)
	ctx := context.Background()

	c.Observe(ctx, btc(), tr(100, 1, 0))
	c.Observe(ctx, btc(), tr(200, 1, 60)) // closes minute 0
	c.Observe(ctx, btc(), tr(999, 5, 30)) // belongs to the closed minute

	if got := c.LatePrints(); got != 1 {
		t.Errorf("late prints = %d, want 1 — a disordered feed would otherwise understate the "+
			"series with nothing to show for it", got)
	}
	if len(cap.events) != 1 {
		t.Fatalf("a late print produced an extra publish: %d events", len(cap.events))
	}
}

// A PUBLISH FAILURE MUST NOT PANIC OR WEDGE THE FEED. The candle is lost, which
// is a hole in the series — logged, and deliberately not fatal: taking the trade
// feed down because one bar did not publish trades a gap for an outage.
func TestAPublishFailureIsSurvived(t *testing.T) {
	cap := &capture{err: errors.New("broker down")}
	c := collector(cap)
	ctx := context.Background()

	c.Observe(ctx, btc(), tr(100, 1, 0))
	c.Observe(ctx, btc(), tr(200, 1, 60))
	c.Flush(ctx, t0.Add(5*time.Minute))
	// Reaching here without a panic is the assertion; the fold must also have
	// moved on rather than retaining the failed candle forever.
	if got := c.LatePrints(); got != 0 {
		t.Errorf("late prints = %d, want 0", got)
	}
}

// The tee passes every trade through untouched — the tape must still receive
// exactly what the feed sent, or the volume side of MarketView silently changes.
func TestTeePassesTradesThroughUnchanged(t *testing.T) {
	cap := &capture{}
	c := collector(cap)
	src := &fakeSource{out: []trades.Trade{tr(100, 1, 0), tr(200, 2, 60)}}

	teed := Tee(src, btc(), c)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		got, err := teed.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if got.Price.Cmp(src.out[i].Price) != 0 || got.Size.Cmp(src.out[i].Size) != 0 {
			t.Fatalf("trade %d was altered in transit: %v", i, got)
		}
	}
	// And the collector saw them: the second trade closed the first minute.
	if len(cap.events) != 1 {
		t.Fatalf("the tee did not feed the collector: %d events", len(cap.events))
	}
}

// A source error propagates and does NOT reach the collector — a failed Recv is
// not a trade, and folding one would invent volume.
func TestTeeDoesNotFoldAFailedRecv(t *testing.T) {
	cap := &capture{}
	c := collector(cap)
	src := &fakeSource{err: errors.New("feed closed")}

	if _, err := Tee(src, btc(), c).Recv(context.Background()); err == nil {
		t.Fatal("the tee swallowed a source error")
	}
	if len(cap.events) != 0 {
		t.Fatalf("a failed Recv produced %d publish(es)", len(cap.events))
	}
}

type fakeSource struct {
	out []trades.Trade
	i   int
	err error
}

func (f *fakeSource) Recv(context.Context) (trades.Trade, error) {
	if f.err != nil {
		return trades.Trade{}, f.err
	}
	if f.i >= len(f.out) {
		return trades.Trade{}, io.EOF
	}
	tr := f.out[f.i]
	f.i++
	return tr, nil
}
