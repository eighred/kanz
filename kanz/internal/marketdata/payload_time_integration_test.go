package marketdata

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/marketedge/coverage"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/pkg/bus"
)

type payloadTimeBroker struct {
	id, subject string
	client      *bus.NATSClient
	nc          *nats.Conn
	js          jetstream.JetStream
}

func newPayloadTimeBroker(t *testing.T, ctx context.Context) payloadTimeBroker {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires TEST_NATS_URL")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "TIME_MARKET", []string{"market.>"})
	bustest.EnsureSubjects(t, ctx, js, "TIME_DLQ", []string{"dlq.>"})
	id := fmt.Sprintf("payload-time-%d", time.Now().UnixNano())
	subject := "market.time." + id
	streamName, err := js.StreamNameBySubject(ctx, subject+".0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := js.Stream(cleanup, streamName)
		if err != nil {
			t.Error(err)
			return
		}
		names := stream.ConsumerNames(cleanup)
		for name := range names.Name() {
			if strings.HasPrefix(name, id) {
				if err := js.DeleteConsumer(cleanup, streamName, name); err != nil {
					t.Error(err)
				}
			}
		}
		if err := names.Err(); err != nil {
			t.Error(err)
		}
	})
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return payloadTimeBroker{id, subject, client, nc, js}
}

func (b payloadTimeBroker) publish(t *testing.T, ctx context.Context, frames []payloadTimeCase, at time.Time) {
	t.Helper()
	p, err := bus.NewProducer(b.client, bus.ProducerConfig{Source: "payload-time-test", ProducerVersion: "test", Tenant: "__system__"})
	if err != nil {
		t.Fatal(err)
	}
	for n, frame := range frames {
		id := payloadTimeInstrument(frame.payload)
		if err := p.Publish(ctx, bus.Event{Subject: fmt.Sprintf("%s.%d", b.subject, n), EventType: frame.eventType, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "market", PartitionKey: id, EventTime: at, IngestionTime: at, Payload: frame.payload}); err != nil {
			t.Fatalf("publish %s: %v", frame.name, err)
		}
	}
}

func payloadTimeInstrument(payload proto.Message) string {
	if ev, ok := payload.(*marketpb.MarketDataEvent); ok {
		return ev.InstrumentId
	}
	return payload.(*marketpb.IngestionCoverage).InstrumentId
}

// A fresh durable reads the original retained frames on each pass. Local dedup
// is disabled; SQL identity, framing and durable parking must hold on replay.
func (b payloadTimeBroker) consume(t *testing.T, ctx context.Context, pass int, h bus.EventHandler, frames []payloadTimeCase, refused map[string]bool) {
	t.Helper()
	consumer, err := bus.NewConsumer(b.client, bus.WithDLQ(b.client), bus.WithDedupWindow(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	dlq, err := b.nc.SubscribeSync("dlq." + b.subject + ".*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dlq.Unsubscribe() }()
	if err := b.nc.FlushTimeout(time.Second); err != nil {
		t.Fatal(err)
	}
	type result struct {
		instrument string
		err        error
	}
	seen := make(chan result, len(frames))
	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	group := fmt.Sprintf("%s-%d", b.id, pass)
	go func() {
		done <- consumer.Subscribe(subCtx, b.subject+".*", group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			err := h(ctx, env, payload)
			select {
			case seen <- result{env.GetPartitionKey(), err}:
			case <-ctx.Done():
			}
			return err
		})
	}()
	for range frames {
		select {
		case got := <-seen:
			if (got.err != nil) != refused[got.instrument] {
				t.Fatalf("pass %d %s: unexpected disposition: %v", pass, got.instrument, got.err)
			}
		case err := <-done:
			t.Fatalf("consumer ended before all frames: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// Handler completion precedes durable parking. Wait for each retained DLQ
	// frame before cancelling; a no-write assertion alone cannot prove delivery.
	parked := map[string]bool{}
	for range len(refused) {
		msg, err := dlq.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		env, _, err := bus.Unframe(msg.Data)
		if err != nil || !refused[env.GetPartitionKey()] || parked[env.GetPartitionKey()] || msg.Header.Get(bus.HeaderDLQOriginalSubject) != strings.TrimPrefix(msg.Subject, "dlq.") {
			t.Fatalf("DLQ lost original identity or parked a valid/duplicate frame: %v", err)
		}
		parked[env.GetPartitionKey()] = true
		name, err := b.js.StreamNameBySubject(ctx, msg.Subject)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := b.js.Stream(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := stream.GetLastMsgForSubject(ctx, msg.Subject)
		if err != nil {
			t.Fatal(err)
		}
		retained, _, err := bus.Unframe(stored.Data)
		if err != nil || retained.GetEventId() != env.GetEventId() {
			t.Fatal("DLQ refusal was not durably retained with its source event identity")
		}
	}
	// Observe broker acknowledgement before cancellation, including the final
	// valid delivery. Handler completion alone does not establish that ACK.
	streamName, err := b.js.StreamNameBySubject(ctx, b.subject+".0")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := b.js.Stream(ctx, streamName)
	if err != nil {
		t.Fatal(err)
	}
	names := stream.ConsumerNames(ctx)
	var durableName string
	for name := range names.Name() {
		if strings.HasPrefix(name, group+"-") {
			if durableName != "" {
				t.Fatal("multiple fixture durables for one pass")
			}
			durableName = name
		}
	}
	if err := names.Err(); err != nil {
		t.Fatal(err)
	}
	if durableName == "" {
		t.Fatal("fixture durable is missing")
	}
	durable, err := b.js.Consumer(ctx, streamName, durableName)
	if err != nil {
		t.Fatal(err)
	}
	for {
		info, err := durable.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.NumPending == 0 && info.NumAckPending == 0 && info.AckFloor.Consumer == uint64(len(frames)) {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("broker did not acknowledge every fixture delivery")
		}
	}
	stop()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("consumer did not stop")
	}
	if msg, err := dlq.NextMsg(100 * time.Millisecond); err != nats.ErrTimeout {
		t.Fatalf("unexpected extra DLQ publication: %v %v", msg != nil, err)
	}
}

func TestPayloadTimestampJetStreamPostgresIngressReplay(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" || os.Getenv("TEST_NATS_URL") == "" {
		t.Skip("requires TEST_POSTGRES_URL and TEST_NATS_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := boundaryPool(t, ctx, dsn)
	t.Log("actual PostgreSQL role and memberships: NOSUPERUSER NOBYPASSRLS; market facts are global reference data")
	st := store.NewPostgres(pool)
	ing, err := NewIngestor(st)
	if err != nil {
		t.Fatal(err)
	}
	b := newPayloadTimeBroker(t, ctx)
	at := time.Now().UTC().Truncate(time.Minute)
	frames := malformedPayloadTimeCases(at)
	badCount := len(frames)
	for _, typ := range []string{"trade", "quote", "bar"} {
		frames = append(frames, payloadTimeCase{"valid " + typ, "market.crypto." + typ, timedMarketEvent(typ, at)})
	}
	nilTime := timedMarketEvent("trade", at)
	nilTime.EventTime = nil
	frames = append(frames, payloadTimeCase{"valid nil fallback", "market.crypto.trade", nilTime})
	cov := covMsg()
	cov.BucketStart, cov.BucketEnd = timestamppb.New(at), timestamppb.New(at.Add(time.Minute))
	frames = append(frames, payloadTimeCase{"valid coverage", coverage.Subject, cov})
	refused := map[string]bool{}
	for n, frame := range frames {
		id := fmt.Sprintf("%s-%d", b.id, n)
		switch payload := frame.payload.(type) {
		case *marketpb.MarketDataEvent:
			payload.InstrumentId = id
		case *marketpb.IngestionCoverage:
			payload.InstrumentId = id
		}
		if n < badCount {
			refused[id] = true
		}
	}
	b.publish(t, ctx, frames, at.Add(2*time.Minute))
	for pass := range 2 {
		b.consume(t, ctx, pass, ing.Handler, frames, refused)
		for n, frame := range frames {
			id := payloadTimeInstrument(frame.payload)
			var marks, bars, attestations int
			if err := pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM price_observations WHERE instrument_id=$1),
				(SELECT count(*) FROM ohlcv_bars WHERE instrument_id=$1),
				(SELECT count(*) FROM ingestion_coverage WHERE instrument_id=$1)`, id).Scan(&marks, &bars, &attestations); err != nil {
				t.Fatal(err)
			}
			wantMarks, wantBars, wantCoverage := 0, 0, 0
			if n >= badCount {
				switch frame.eventType {
				case coverage.Subject:
					wantCoverage = 1
				case "market.crypto.bar":
					wantMarks, wantBars = 1, 1
				default:
					wantMarks = 1
				}
			}
			if marks != wantMarks || bars != wantBars || attestations != wantCoverage {
				t.Fatalf("pass %d %s: marks/bar/coverage=%d/%d/%d, want %d/%d/%d", pass, frame.name, marks, bars, attestations, wantMarks, wantBars, wantCoverage)
			}
			if wantMarks == 1 {
				rows, err := st.History(ctx, store.Query{InstrumentID: id})
				wantAt := at
				if frame.eventType == "market.crypto.bar" {
					wantAt = at.Add(time.Minute)
				} else if frame.name == "valid nil fallback" {
					wantAt = at.Add(2 * time.Minute)
				}
				if len(rows) != 1 || err != nil || !rows[0].ObservationTime.Equal(wantAt) || !rows[0].KnowledgeTime.Equal(at.Add(2*time.Minute)) {
					t.Fatalf("valid stored timestamp/fallback changed: %+v error=%v", rows, err)
				}
			}
		}
		t.Logf("pass %d: %d malformed frames durably parked, five valid controls retained; no partial writes", pass, badCount)
	}
}

func TestPayloadTimestampJetStreamLiveMonitor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b := newPayloadTimeBroker(t, ctx)
	at := time.Now().UTC().Truncate(time.Second)
	var frames []payloadTimeCase
	for name, stamp := range invalidPayloadTimes(at) {
		for _, typ := range []string{"trade", "quote"} {
			id := fmt.Sprintf("%s-%d", b.id, len(frames))
			seed := timedMarketEvent("quote", at)
			seed.InstrumentId = id
			bad := timedMarketEvent(typ, at)
			bad.InstrumentId, bad.EventTime = id, stamp
			if typ == "trade" {
				bad.GetTrade().Price = decv(900, 0)
			} else {
				bad.GetQuote().BidPrice, bad.GetQuote().AskPrice = decv(900, 0), decv(902, 0)
			}
			valid := timedMarketEvent("quote", at.Add(time.Nanosecond))
			valid.InstrumentId = id
			valid.GetQuote().BidPrice, valid.GetQuote().AskPrice = decv(200, 0), decv(202, 0)
			frames = append(frames,
				payloadTimeCase{name + "/seed", "market.crypto.quote", seed},
				payloadTimeCase{name + "/" + typ, "market.crypto." + typ, bad},
				payloadTimeCase{name + "/valid recovery", "market.crypto.quote", valid})
		}
	}
	b.publish(t, ctx, frames, at)
	for pass := range 2 {
		s := mark.New(func() time.Time { return at }, time.Minute)
		stages := map[string]int{}
		h := func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			if err := s.Handle(ctx, env, payload); err != nil {
				return err
			}
			id := env.GetPartitionKey()
			stage := stages[id]
			stages[id]++
			wantPrice, wantBid, wantAsk, wantAt := "101", "100", "102", at
			if stage == 2 {
				wantPrice, wantBid, wantAsk, wantAt = "201", "200", "202", at.Add(time.Nanosecond)
			}
			price, gotAt, seen := s.Lookup(id)
			bid, ask, touchAt, ok := s.Touch(id)
			if !seen || price == nil || price.FloatString(0) != wantPrice || !gotAt.Equal(wantAt) || !ok || bid.FloatString(0) != wantBid || ask.FloatString(0) != wantAsk || !touchAt.Equal(wantAt) {
				return fmt.Errorf("stage %d: price/touch changed or valid recovery lost precision", stage)
			}
			return nil
		}
		b.consume(t, ctx, pass, h, frames, nil)
		t.Logf("pass %d: %d real broker deliveries acknowledged; malformed times preserved prior price/touch, valid recovery kept nanoseconds", pass, len(frames))
	}
}
