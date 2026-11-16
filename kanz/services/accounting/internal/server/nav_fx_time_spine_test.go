package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/fxfeed"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresJetStreamFXTimeThroughNAV(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires real restricted-role PostgreSQL and JetStream")
	}
	pool := newServerPool(t)
	var super, bypass bool
	if err := pool.QueryRow(t.Context(), `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("requires NOSUPERUSER NOBYPASSRLS: super=%v bypass=%v err=%v", super, bypass, err)
	}
	store := ledger.NewPostgres(pool)
	seedFXTimeCash(t, store)
	suffix := fmt.Sprint(time.Now().UnixNano())
	instrument := "EURUSD-FXTIME-" + suffix
	live := fxfeed.New("USD", map[string]string{instrument: "EUR"})
	web := serveFXTimeNAV(t, store, live, "__system__")
	assertFXTimeNAV(t, web, "__system__", "")
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	const subject = "market.fx.quote"
	bustest.EnsureSubjects(t, ctx, js, "FX_TIME_"+suffix, []string{subject, "dlq." + subject})
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "fx-time"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "fx-time-fixture", ProducerVersion: "test", Tenant: "__system__"})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithDedupWindow(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	ready, done := make(chan struct{}), make(chan error, 1)
	observed := make(chan error, 1)
	go func() {
		done <- consumer.SubscribeReady(ctx, "market.fx.>", "fx-time-"+suffix, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			err := live.Handler(ctx, env, payload)
			if env.GetPartitionKey() == instrument {
				select {
				case observed <- err:
				default:
				}
			}
			return err
		}, func() { close(ready) })
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("FX subscriber did not join")
			}
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		joined = true
		t.Fatalf("FX subscription exited: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, tc := range []struct {
		name  string
		at    *timestamppb.Timestamp
		rate  int64
		err   bool
		total string
	}{
		{"missing payload time", nil, 120, true, ""},
		{"invalid payload time", &timestamppb.Timestamp{Nanos: -1}, 120, true, ""},
		{"first", timestamppb.New(navFXTime), 120, false, "120.00"},
		{"older", timestamppb.New(navFXTime.Add(-time.Second)), 110, false, "120.00"},
		{"exact replay", timestamppb.New(navFXTime), 120, false, "120.00"},
		{"same-time conflict", timestamppb.New(navFXTime), 125, true, ""},
		{"older cannot restore", timestamppb.New(navFXTime.Add(-time.Second)), 110, false, ""},
		{"same-time cannot restore", timestamppb.New(navFXTime), 120, true, ""},
		{"strictly newer repair", timestamppb.New(navFXTime.Add(time.Second)), 130, false, "130.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eventTime := navFXTime
			if tc.at != nil && tc.at.CheckValid() == nil {
				eventTime = tc.at.AsTime()
			}
			// A valid envelope timestamp cannot substitute for a missing or
			// malformed authoritative timestamp inside the received payload.
			if err := producer.Publish(ctx, bus.Event{Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
				SchemaVersion: 1, Domain: "market", EventTime: eventTime, PartitionKey: instrument, PayloadSchemaRef: "market.v1.MarketDataEvent:1", Payload: navTimeQuote(instrument, tc.at, tc.rate)}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-observed:
				if (err != nil) != tc.err {
					t.Fatalf("FX subscriber error=%v want refusal=%v", err, tc.err)
				}
			case err := <-done:
				joined = true
				t.Fatalf("FX subscription exited: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			assertFXTimeNAV(t, web, "__system__", tc.total)
		})
	}
	cancel()
	select {
	case err := <-done:
		joined = true
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("FX subscriber did not stop")
	}
}
