package cashview_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/cashview"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type cashTimeConsumerID struct{ stream, name string }

type cashTimeBroadcastClient struct {
	*bus.NATSClient
	created chan cashTimeConsumerID
}

func (c *cashTimeBroadcastClient) SubscribeBroadcastObserved(ctx context.Context, filter string, h bus.Handler, ready func(), created bus.ConsumerCreated) error {
	return c.NATSClient.SubscribeBroadcastObserved(ctx, filter, h, ready, func(stream, name string) {
		if created != nil {
			created(stream, name)
		}
		c.created <- cashTimeConsumerID{stream, name}
	})
}

func TestCashPayloadTimeCanonicalBroadcastACKRetainedRestartAndRepair(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires TEST_NATS_URL with canonical ACCOUNTING stream")
	}
	for _, tc := range malformedCashTimes(cashTimeBase) {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			portfolio := "CASH-TIME-" + uuid.NewString()
			control, err := nats.Connect(url)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(control.Close)
			js, err := jetstream.New(control)
			if err != nil {
				t.Fatal(err)
			}
			name, err := js.StreamNameBySubject(ctx, cashview.Subject)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := js.Stream(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			info, err := stream.Info(ctx)
			if err != nil || name != "ACCOUNTING" || info.Config.Retention != jetstream.LimitsPolicy || info.Config.MaxAge != 168*time.Hour || info.Config.MaxMsgsPerSubject == 1 {
				t.Fatalf("requires canonical ACCOUNTING retention, no fixture fallback: %+v %v", info, err)
			}
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: portfolio})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting/cash-time-test", ProducerVersion: "test", Tenant: "T"})
			if err != nil {
				t.Fatal(err)
			}
			var clock atomic.Int64
			clock.Store(cashTimeBase.UnixNano())
			type observedView struct {
				view    *cashview.View
				updates atomic.Int64
			}
			fresh := func() *observedView {
				var observed observedView
				observed.view = cashview.New(cashview.WithClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() }),
					cashview.WithOnUpdate(func(pf string) {
						if pf == portfolio {
							observed.updates.Add(1)
						}
					}))
				return &observed
			}
			publish := func(msg *accountingpb.PortfolioCashBalance) string {
				t.Helper()
				id, err := uuid.NewV7()
				if err != nil {
					t.Fatal(err)
				}
				if err := producer.Publish(ctx, bus.Event{EventID: id.String(), Subject: cashview.Subject, EventType: cashview.Subject,
					EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "accounting", SchemaVersion: 1,
					EventTime: time.Unix(0, clock.Load()).UTC(), PartitionKey: portfolio,
					PayloadSchemaRef: "accounting.v1.PortfolioCashBalance:1", Payload: msg}); err != nil {
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
			start := func(view *cashview.View) subscription {
				t.Helper()
				transport := &cashTimeBroadcastClient{NATSClient: client, created: make(chan cashTimeConsumerID, 1)}
				consumer, err := bus.NewConsumer(transport)
				if err != nil {
					t.Fatal(err)
				}
				subCtx, stop := context.WithCancel(ctx)
				seen, done := make(chan delivery, 32), make(chan error, 1)
				go func() {
					done <- consumer.SubscribeBroadcast(subCtx, cashview.Subject, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
						err := view.Handle(ctx, env, payload)
						if env.GetPartitionKey() == portfolio {
							select {
							case seen <- delivery{env.GetEventId(), err}:
							case <-subCtx.Done():
							}
						}
						return err
					})
				}()
				var id cashTimeConsumerID
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
							t.Error("cash broadcast did not join")
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
				t.Cleanup(finish) // cancel/join and owned deletion precede bus/control closure
				select {
				case id = <-transport.created:
				case <-ctx.Done():
					t.Fatal("production broadcast did not create its consumer")
				}
				if id.stream != "ACCOUNTING" || id.name == "" {
					t.Fatal("broadcast did not report its owned canonical consumer")
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
							t.Fatalf("cash discard changed nil/ACK disposition: %v", got.err)
						}
					case <-ctx.Done():
						t.Fatal("canonical cash frame did not reach the actual consumer")
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
						t.Fatal("canonical cash frame was not ACKed")
					}
				}
			}
			bad := cashTimeReport(portfolio, &commonpb.Decimal{Coefficient: 750}, tc.at)
			badID := publish(bad)
			cold := fresh()
			initial := start(cold.view)
			awaitACK(initial, badID)
			if _, known := cold.view.Lookup(portfolio); known || cold.updates.Load() != 0 {
				t.Fatal("retained malformed frame established cash or fired an update")
			}
			gate, recorder := cashTimeGate(t, cold.view, portfolio)
			assertCashTimeDecision(t, gate, recorder, portfolio, "unknown")
			initial.stop()
			live := fresh()
			active := start(live.view)
			awaitACK(active, badID)
			if _, _, _, ok := live.view.Spendable(portfolio); ok || live.updates.Load() != 0 {
				t.Fatal("fresh retained rebuild turned malformed cash into spendable cash")
			}
			gate, recorder = cashTimeGate(t, live.view, portfolio)
			assertCashTimeDecision(t, gate, recorder, portfolio, "unknown")
			awaitACK(active, publish(cashTimeReport(portfolio, &commonpb.Decimal{}, timestamppb.New(cashTimeBase))))
			assertCashTimeDecision(t, gate, recorder, portfolio, "empty")
			clock.Store(cashTimeBase.Add(4 * time.Minute).UnixNano())
			for _, future := range malformedCashTimes(cashTimeBase.Add(4 * time.Minute)) {
				if future.name == tc.name {
					bad.AsOf = future.at
				}
			}
			awaitACK(active, publish(bad))
			prior, known := live.view.Lookup(portfolio)
			if !known || prior.Total.GetCoefficient() != 0 || !prior.AsOf.Equal(cashTimeBase) || live.updates.Load() != 1 {
				t.Fatal("warm malformed delivery changed/refreshed prior cash or fired an update")
			}
			assertCashTimeDecision(t, gate, recorder, portfolio, "empty")
			clock.Store(cashTimeBase.Add(cashview.DefaultMaxAge + time.Nanosecond).UnixNano())
			if _, _, _, ok := live.view.Spendable(portfolio); ok {
				t.Fatal("malformed delivery extended the prior cash level's lifetime")
			}
			assertCashTimeDecision(t, gate, recorder, portfolio, "unknown")
			repairAt := time.Unix(0, clock.Load()).UTC()
			repair := publish(cashTimeReport(portfolio, &commonpb.Decimal{Coefficient: 750}, timestamppb.New(repairAt)))
			awaitACK(active, repair)
			if live.updates.Load() != 2 {
				t.Fatal("valid repair did not restore the actual update callback")
			}
			assertCashTimeDecision(t, gate, recorder, portfolio, "funded")
			active.stop()
			restarted := fresh()
			replay := start(restarted.view)
			awaitACK(replay, repair)
			if final, known := restarted.view.Lookup(portfolio); !known || final.Total.GetCoefficient() != 750 || !final.AsOf.Equal(repairAt) || restarted.updates.Load() != 1 {
				t.Fatal("fresh canonical retained replay lost the valid repair")
			}
			gate, recorder = cashTimeGate(t, restarted.view, portfolio)
			assertCashTimeDecision(t, gate, recorder, portfolio, "funded")
		})
	}
}
