package server

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/pg"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/fxfeed"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fxTupleWire struct {
	id   string
	body []byte
	err  error
}

// A single transport fault after the real Consumer succeeds forces broker
// redelivery. It is not the FX handler's policy: unusable tuples return nil/ACK.
// The unchanged default consumer dedup may skip the second domain dispatch.
type fxTupleTransport struct {
	*bus.NATSClient
	instrument, failEvent string
	failed                atomic.Bool
	domainErrors          atomic.Int64
	wire                  chan fxTupleWire
}

func (c *fxTupleTransport) Subscribe(ctx context.Context, topic, group string, h bus.Handler) error {
	return c.NATSClient.Subscribe(ctx, topic, group, func(ctx context.Context, msg bus.Message) error {
		err := h(ctx, msg)
		env, _, decodeErr := bus.Unframe(msg.Body)
		if decodeErr == nil && env.GetPartitionKey() == c.instrument {
			select {
			case c.wire <- fxTupleWire{env.GetEventId(), append([]byte(nil), msg.Body...), err}:
			case <-ctx.Done():
			}
			if env.GetEventId() == c.failEvent && !c.failed.Swap(true) {
				return errors.New("fixture: one transport failure after domain success")
			}
		}
		return err
	})
}

func TestPostgresJetStreamFXQuoteTuplesThroughNAV(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires real canonical JetStream and NOSUPERUSER NOBYPASSRLS PostgreSQL")
	}
	pool := newServerPool(t)
	var exempt bool
	if err := pool.QueryRow(t.Context(), `SELECT COALESCE(bool_or(rolsuper OR rolbypassrls),false) FROM pg_roles WHERE pg_has_role(current_user,oid,'USAGE')`).Scan(&exempt); err != nil || exempt {
		t.Fatalf("requires restricted role including inherited roles: exempt=%v err=%v", exempt, err)
	}
	store := ledger.NewPostgres(pool)
	seedFXTupleLeg(t, store, "PF1", "cash", "EUR", 100)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	id := "fx-tuple-" + uuid.NewString()
	instrument := "EURUSD-" + id
	const topic = "market.fx.quote"
	filter := config.DefaultFXSubjects[0]
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	streamName, err := bustest.StreamFor(ctx, js, topic)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting/fx-tuple-fixture", ProducerVersion: "test", Tenant: "__system__"})
	if err != nil {
		t.Fatal(err)
	}
	// The stream and shared market subject are authoritative history. Only this
	// fixture's unique durable consumers are removed, after all subscribers join.
	owned := map[string]bool{}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for name := range owned {
			if err := stream.DeleteConsumer(cleanup, name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
				t.Error(err)
			}
		}
	})
	type subscription struct {
		consumer jetstream.Consumer
		wire     <-chan fxTupleWire
		domain   *atomic.Int64
		stop     func()
	}
	start := func(live *fxfeed.LiveFX, suffix, failEvent string) subscription {
		t.Helper()
		group := id + "-" + suffix
		transport := &fxTupleTransport{NATSClient: client, instrument: instrument, failEvent: failEvent, wire: make(chan fxTupleWire, 64)}
		consumer, err := bus.NewConsumer(transport, bus.WithDLQ(client))
		if err != nil {
			t.Fatal(err)
		}
		subCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- consumer.Subscribe(subCtx, filter, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
				err := live.Handler(ctx, env, payload)
				if err != nil && env.GetPartitionKey() == instrument {
					transport.domainErrors.Add(1)
				}
				return err
			})
		}()
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
					t.Error("FX tuple generic subscriber did not join")
				}
			})
		}
		t.Cleanup(finish)
		for {
			listing := stream.ListConsumers(ctx)
			var name string
			for info := range listing.Info() {
				if strings.HasPrefix(info.Name, group+"-") && info.Config.FilterSubject == filter {
					if name != "" {
						t.Fatal("ambiguous owned FX consumer")
					}
					name = info.Name
				}
			}
			if err := listing.Err(); err != nil {
				t.Fatal(err)
			}
			if name != "" {
				owned[name] = true
				actual, err := stream.Consumer(ctx, name)
				if err != nil {
					t.Fatal(err)
				}
				return subscription{actual, transport.wire, &transport.domainErrors, finish}
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("production generic subscriber did not create its durable")
			}
		}
	}
	await := func(sub subscription, eventID string) fxTupleWire {
		t.Helper()
		for {
			select {
			case wire := <-sub.wire:
				if wire.id != eventID {
					continue
				}
				if wire.err != nil || sub.domain.Load() != 0 {
					t.Fatalf("unusable FX tuple must return nil/ACK: transport=%v domain_errors=%d", wire.err, sub.domain.Load())
				}
				return wire
			case <-ctx.Done():
				t.Fatal("expected canonical FX tuple was not consumed")
			}
		}
	}
	acked := func(sub subscription) {
		t.Helper()
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
				t.Fatal("tuple was not acknowledged by the real broker")
			}
		}
	}
	publish := func(event *marketpb.MarketDataEvent) string {
		t.Helper()
		eventID, err := uuid.NewV7()
		if err != nil {
			t.Fatal(err)
		}
		if err := producer.Publish(ctx, bus.Event{EventID: eventID.String(), Subject: topic, EventType: topic,
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "market",
			EventTime: event.GetEventTime().AsTime(), PartitionKey: instrument, PayloadSchemaRef: "market.v1.MarketDataEvent:1", Payload: event}); err != nil {
			t.Fatal(err)
		}
		return eventID.String()
	}
	makeLive := func() *fxfeed.LiveFX { return fxfeed.New("USD", map[string]string{instrument: "EUR"}) }
	bad := navUnusableFXTuples()
	firstID := publish(navTupleQuote(instrument, navFXTime.Add(time.Hour), bad[0].bid, bad[0].ask))
	live := makeLive()
	active := start(live, "original", firstID)
	web := fxTupleServer(store, live, "__system__")
	t.Run("cold-invalid-redelivery-ACK", func(t *testing.T) {
		first, repeated := await(active, firstID), await(active, firstID)
		if !bytes.Equal(first.body, repeated.body) {
			t.Fatal("broker redelivery changed canonical bytes")
		}
		acked(active)
		assertFXTupleNAV(t, web, "__system__", "PF1", "", "EUR")
	})
	active.stop()
	t.Run("fresh-retained-invalid-ACK", func(t *testing.T) {
		fresh := makeLive()
		replay := start(fresh, "retained-invalid", "")
		wire := await(replay, firstID)
		env, _, err := bus.Unframe(wire.body)
		if err != nil || bus.Validate(env) != nil || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
			t.Fatal("retained input did not cross canonical validation")
		}
		acked(replay)
		assertFXTupleNAV(t, fxTupleServer(store, fresh, "__system__"), "__system__", "PF1", "", "EUR")
		replay.stop()
	})
	live = makeLive()
	active = start(live, "original", "") // production durable resume, not a new replay policy
	web = fxTupleServer(store, live, "__system__")
	t.Run("resumed-cold-until-new-valid", func(t *testing.T) {
		acked(active)
		assertFXTupleNAV(t, web, "__system__", "PF1", "", "EUR")
		await(active, publish(navTimeQuote(instrument, timestamppb.New(navFXTime), 120)))
		acked(active)
		assertFXTupleNAV(t, web, "__system__", "PF1", "120.00", "")
	})
	frozen := live.Converter()
	for _, tuple := range bad {
		t.Run("future-invalid-"+tuple.name, func(t *testing.T) {
			await(active, publish(navTupleQuote(instrument, navFXTime.Add(time.Hour), tuple.bid, tuple.ask)))
			acked(active)
			assertFXTupleNAV(t, web, "__system__", "PF1", "120.00", "")
		})
	}
	t.Run("valid-between-watermarks-repairs", func(t *testing.T) {
		await(active, publish(navTimeQuote(instrument, timestamppb.New(navFXTime.Add(time.Second)), 130)))
		acked(active)
		assertFXTupleNAV(t, web, "__system__", "PF1", "130.00", "")
		if rate, known := frozen.Rate("EUR"); !known || rate.Cmp(big.NewRat(6, 5)) != 0 {
			t.Fatal("repair mutated the immutable 1.20 converter")
		}
	})
	active.stop()
	t.Run("fresh-retained-recovery", func(t *testing.T) {
		fresh := makeLive()
		replay := start(fresh, "retained-repair", "")
		acked(replay)
		assertFXTupleNAV(t, fxTupleServer(store, fresh, "__system__"), "__system__", "PF1", "130.00", "")
		replay.stop()
	})
	t.Run("restricted-reopen-RLS-and-valuation-legs", func(t *testing.T) {
		reopened, err := pg.NewTenantPool(ctx, os.Getenv("TEST_POSTGRES_URL"), "__system__")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		durable := ledger.NewPostgres(reopened)
		assertFXTupleNAV(t, fxTupleServer(durable, live, "__system__"), "__system__", "PF1", "130.00", "")
		for _, leg := range []string{"security", "accrual"} {
			portfolio := "PF-FXTUPLE-" + leg
			seedFXTupleLeg(t, durable, portfolio, leg, "EUR", 100)
			assertFXTupleNAV(t, fxTupleServer(durable, live, "__system__"), "__system__", portfolio, "130.00", "")
		}
		seedFXTupleLeg(t, durable, "PF-FXTUPLE-GBP", "cash", "GBP", 100)
		assertFXTupleNAV(t, fxTupleServer(durable, live, "__system__"), "__system__", "PF-FXTUPLE-GBP", "", "GBP")
		seedFXTupleLeg(t, durable, "PF-FXTUPLE-USD", "cash", "USD", 100)
		assertFXTupleNAV(t, fxTupleServer(durable, live, "__system__"), "__system__", "PF-FXTUPLE-USD", "100.00", "")
		other, err := pg.NewTenantPool(ctx, os.Getenv("TEST_POSTGRES_URL"), "other-"+id)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		foreign := ledger.NewPostgres(other)
		rows, err := foreign.Journal(ctx, "PF1")
		if err != nil || len(rows) != 0 {
			t.Fatalf("foreign tenant discovered another EUR journal: rows=%d err=%v", len(rows), err)
		}
		seedFXTupleLeg(t, foreign, "PF1", "cash", "EUR", 300)
		assertFXTupleNAV(t, fxTupleServer(foreign, live, "other-"+id), "other-"+id, "PF1", "390.00", "")
		assertFXTupleNAV(t, fxTupleServer(durable, live, "__system__"), "__system__", "PF1", "130.00", "")
	})
}
