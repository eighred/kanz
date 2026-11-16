package mark_test

import (
	"context"
	"errors"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/mark"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type quoteConsumerID struct{ stream, name string }

type quoteBroadcastClient struct {
	*bus.NATSClient
	created chan quoteConsumerID
}

func (c *quoteBroadcastClient) SubscribeBroadcastObserved(ctx context.Context, filter string, h bus.Handler, ready func(), created bus.ConsumerCreated) error {
	return c.NATSClient.SubscribeBroadcastObserved(ctx, filter, h, ready, func(stream, name string) {
		if created != nil {
			created(stream, name)
		}
		c.created <- quoteConsumerID{stream, name}
	})
}

func TestLiveQuoteCanonicalBroadcastACKRetainedRestartAndRepair(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires TEST_NATS_URL with canonical MARKET stream")
	}
	for _, tc := range []struct {
		name     string
		bid, ask *commonpb.Decimal
	}{
		{"negative-bid", dec(-100, 0), dec(110, 0)},
		{"zero-bid", dec(0, 0), dec(2, 0)},
		{"crossed", dec(2, 0), dec(1, 0)},
		{"out-of-domain", dec(1, 65), dec(2, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			instrument := "LIVE-QUOTE-" + uuid.NewString()
			const topic = "market.equity.quote"
			control, err := nats.Connect(url)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(control.Close)
			js, err := jetstream.New(control)
			if err != nil {
				t.Fatal(err)
			}
			name, err := js.StreamNameBySubject(ctx, topic)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := js.Stream(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			info, err := stream.Info(ctx)
			if err != nil || name != "MARKET" || info.Config.MaxAge != 24*time.Hour || info.Config.Retention != jetstream.LimitsPolicy || info.Config.MaxMsgsPerSubject == 1 {
				t.Fatalf("requires canonical MARKET retention, no fixture fallback: %+v %v", info, err)
			}
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: instrument})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "marketdata/live-quote-test", ProducerVersion: "test", Tenant: "__system__"})
			if err != nil {
				t.Fatal(err)
			}
			var clock atomic.Int64
			clock.Store(base.UnixNano())
			fresh := func() *mark.Source {
				return mark.New(func() time.Time { return time.Unix(0, clock.Load()).UTC() }, time.Minute, mark.WithTouchMaxAge(10*time.Second))
			}
			publish := func(bid, ask *commonpb.Decimal, at time.Time) string {
				t.Helper()
				id, err := uuid.NewV7()
				if err != nil {
					t.Fatal(err)
				}
				if err := producer.Publish(ctx, bus.Event{EventID: id.String(), Subject: topic, EventType: topic,
					EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "market", SchemaVersion: 1,
					EventTime: at, PartitionKey: instrument, PayloadSchemaRef: "market.v1.MarketDataEvent:1",
					Payload: &marketpb.MarketDataEvent{InstrumentId: instrument, EventTime: timestamppb.New(at),
						Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{BidPrice: bid, AskPrice: ask}}}}); err != nil {
					t.Fatal(err)
				}
				return id.String()
			}
			type delivery struct {
				id  string
				err error
			}
			type subscription struct {
				consumer jetstream.Consumer
				seen     <-chan delivery
				stop     func()
			}
			start := func(s *mark.Source) subscription {
				t.Helper()
				transport := &quoteBroadcastClient{NATSClient: client, created: make(chan quoteConsumerID, 1)}
				consumer, err := bus.NewConsumer(transport)
				if err != nil {
					t.Fatal(err)
				}
				subCtx, stop := context.WithCancel(ctx)
				seen, done := make(chan delivery, 32), make(chan error, 1)
				go func() {
					done <- consumer.SubscribeBroadcast(subCtx, topic, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
						err := s.Handle(ctx, env, payload)
						if env.GetPartitionKey() == instrument {
							select {
							case seen <- delivery{env.GetEventId(), err}:
							case <-subCtx.Done():
							}
						}
						return err
					})
				}()
				var id quoteConsumerID
				var once sync.Once
				finish := func() {
					once.Do(func() {
						stop()
						select {
						case err := <-done:
							if err != nil && !errors.Is(err, context.Canceled) {
								t.Error(err)
							}
						case <-time.After(5 * time.Second):
							t.Error("live quote broadcast did not join")
						}
						if id.name != "" {
							cleanup, end := context.WithTimeout(context.Background(), 5*time.Second)
							defer end()
							if err := js.DeleteConsumer(cleanup, id.stream, id.name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
								t.Error(err)
							}
						}
					})
				}
				t.Cleanup(finish) // join/delete before the bus and control sockets close
				select {
				case id = <-transport.created:
				case <-ctx.Done():
					t.Fatal("production broadcast did not create its consumer")
				}
				owned, err := js.Consumer(ctx, id.stream, id.name)
				if err != nil {
					t.Fatal(err)
				}
				return subscription{owned, seen, finish}
			}
			awaitACK := func(sub subscription, id string) {
				t.Helper()
				for {
					select {
					case got := <-sub.seen:
						if got.id != id {
							continue
						}
						if got.err != nil {
							t.Fatalf("live quote changed the nil/ACK policy: %v", got.err)
						}
					case <-ctx.Done():
						t.Fatal("canonical quote did not reach the real consumer")
					}
					break
				}
				for {
					info, err := sub.consumer.Info(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if info.NumPending == 0 && info.NumAckPending == 0 && info.AckFloor.Consumer > 0 {
						return
					}
					select {
					case <-time.After(10 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal("canonical live quote was not ACKed")
					}
				}
			}
			badID := publish(tc.bid, tc.ask, base.Add(time.Hour))
			cold := fresh()
			initial := start(cold)
			awaitACK(initial, badID)
			if price, _, seen := cold.Lookup(instrument); seen || price != nil || cold.Mark(instrument) != nil {
				t.Fatal("retained unusable quote established a live Mark")
			}
			initial.stop()
			live := fresh()
			active := start(live)
			awaitACK(active, badID)
			if _, _, seen := live.Lookup(instrument); seen {
				t.Fatal("fresh broadcast materialized an unusable retained quote")
			}
			awaitACK(active, publish(dec(80, 0), dec(82, 0), base))
			clock.Store(base.Add(5 * time.Second).UnixNano())
			awaitACK(active, publish(tc.bid, tc.ask, base.Add(time.Hour)))
			price, at, seen := live.Lookup(instrument)
			bid, ask, touchAt, touchKnown := live.Touch(instrument)
			if !seen || price == nil || price.Cmp(big.NewRat(81, 1)) != 0 || !at.Equal(base) || !touchKnown ||
				bid.Cmp(big.NewRat(80, 1)) != 0 || ask.Cmp(big.NewRat(82, 1)) != 0 || !touchAt.Equal(base) {
				t.Fatal("warm invalid delivery changed or refreshed public Mark/Touch")
			}
			clock.Store(base.Add(61 * time.Second).UnixNano())
			if live.Mark(instrument) != nil {
				t.Fatal("invalid delivery extended the prior mark's real lifetime")
			}
			repairAt := base.Add(62 * time.Second)
			clock.Store(repairAt.UnixNano())
			repair := publish(dec(100, 0), dec(102, 0), repairAt)
			awaitACK(active, repair)
			if price := live.Mark(instrument); price == nil || price.Cmp(big.NewRat(101, 1)) != 0 {
				t.Fatal("valid quote before the ignored future time did not repair")
			}
			active.stop()
			restarted := fresh()
			replay := start(restarted)
			awaitACK(replay, repair)
			if price := restarted.Mark(instrument); price == nil || price.Cmp(big.NewRat(101, 1)) != 0 {
				t.Fatal("fresh canonical retained rebuild lost the valid repair")
			}
		})
	}
}
