package venuemargin

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type marginTimeDelivery struct {
	env *envelopepb.Envelope
	msg *collateralpb.VenueMarginState
	err error
}

// The same Producer and broadcast Consumer used by the live owners must accept
// the valid envelope while View refuses its malformed domain timestamp. A new
// process also replays the retained bad level without manufacturing margin state.
func TestNATSMalformedMarginObservationTimeRetainedReplay(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL for canonical venue-margin broadcast and retained replay proof")
	}
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
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenant := "margin-time-" + id
	bustest.EnsureSubjects(t, ctx, js, "MARGIN_TIME_"+id, []string{Subject})
	streamName, err := bustest.StreamFor(ctx, js, Subject)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "margin-observation-time"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "venue-margin/time-test", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	publish := func(t *testing.T, msg *collateralpb.VenueMarginState) {
		t.Helper()
		// Envelope event time remains valid; the defect is in the payload stamp.
		if err := producer.Publish(ctx, bus.Event{Subject: Subject, EventType: Subject,
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1,
			Domain: "accounting", EventTime: observed, PartitionKey: "acct-1",
			PayloadSchemaRef: "collateral.v1.VenueMarginState:1", Payload: msg}); err != nil {
			t.Fatal(err)
		}
	}
	start := func(t *testing.T, view *View) (<-chan marginTimeDelivery, func(), jetstream.Consumer) {
		t.Helper()
		consumer, err := bus.NewConsumer(client)
		if err != nil {
			t.Fatal(err)
		}
		subCtx, stop := context.WithCancel(ctx)
		ready, done := make(chan struct{}), make(chan error, 1)
		seen := make(chan marginTimeDelivery, 8)
		since := time.Now().UTC()
		go func() {
			done <- consumer.SubscribeBroadcastReady(subCtx, Subject, func(c context.Context, env *envelopepb.Envelope, payload []byte) error {
				if env.GetTenantId() != tenant {
					return nil
				}
				msg := new(collateralpb.VenueMarginState)
				err := proto.Unmarshal(payload, msg)
				if err == nil {
					err = view.Handle(c, env, payload)
				}
				select {
				case seen <- marginTimeDelivery{env: env, msg: msg, err: err}:
				case <-subCtx.Done():
				}
				return err
			}, func() { close(ready) })
		}()
		var once sync.Once
		shutdown := func() {
			once.Do(func() {
				stop()
				select {
				case err := <-done:
					if err != nil && ctx.Err() == nil {
						t.Errorf("broadcast shutdown: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("broadcast did not stop")
				}
			})
		}
		t.Cleanup(shutdown)
		select {
		case <-ready: // Broker catch-up waits for explicit ACK, including refusals.
		case err := <-done:
			t.Fatalf("broadcast stopped before readiness: %v", err)
		case <-ctx.Done():
			t.Fatal("broadcast did not finish retained catch-up")
		}
		for info := range stream.ListConsumers(ctx).Info() {
			if !info.Created.Before(since) && info.Config.FilterSubject == Subject && info.Config.Durable == "" {
				actual, err := stream.Consumer(ctx, info.Name)
				if err != nil {
					t.Fatal(err)
				}
				return seen, shutdown, actual
			}
		}
		t.Fatal("new broadcast consumer missing from broker")
		return nil, nil, nil
	}
	await := func(t *testing.T, seen <-chan marginTimeDelivery, consumer jetstream.Consumer, want *collateralpb.VenueMarginState) {
		t.Helper()
		select {
		case got := <-seen:
			if got.err != nil || !proto.Equal(got.msg, want) || bus.Validate(got.env) != nil || got.env.GetEventType() != Subject || got.env.GetTenantId() != tenant || got.env.GetIdempotencyKey() != got.env.GetEventId() {
				t.Fatalf("canonical delivery changed/refused: err=%v payload_equal=%v envelope=%+v", got.err, proto.Equal(got.msg, want), got.env)
			}
		case <-ctx.Done():
			t.Fatal("canonical delivery did not reach View.Handle")
		}
		last, err := stream.GetLastMsgForSubject(ctx, Subject)
		if err != nil {
			t.Fatal(err)
		}
		for {
			info, err := consumer.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.NumAckPending == 0 && info.NumPending == 0 && info.AckFloor.Stream >= last.Sequence {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("broker never confirmed the ACK/discard outcome")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	for name, stamp := range malformedMarginTimes() {
		t.Run(name, func(t *testing.T) {
			now := frozen()
			var undated, uncovered atomic.Int32
			view := New(WithClock(func() time.Time { return now }),
				WithOnUndated(func(string, string) { undated.Add(1) }),
				WithOnUncovered(func(string, string, uint32) { uncovered.Add(1) }))
			bad := state(t)
			bad.ObservedAt = stamp
			bad.MarginRatio = decimal(t, "999")
			bad.Coverage.ExcludedCount = 1
			publish(t, bad)
			seen, stop, consumer := start(t, view)
			await(t, seen, consumer, bad)
			assertMarginLevel(t, view, nil)
			good := state(t)
			publish(t, good)
			await(t, seen, consumer, good)
			assertMarginLevel(t, view, good)
			publish(t, bad)
			await(t, seen, consumer, bad)
			assertMarginLevel(t, view, good)
			now = observed.Add(DefaultMaxAge + time.Nanosecond)
			assertMarginLevel(t, view, nil)
			if undated.Load() != 2 || uncovered.Load() != 0 {
				t.Fatalf("callbacks: rejected=%d accepted_uncovered=%d", undated.Load(), uncovered.Load())
			}
			stop()
			restarted := New(WithClock(func() time.Time { return now }))
			replayed, stopReplay, replayConsumer := start(t, restarted)
			await(t, replayed, replayConsumer, bad)
			assertMarginLevel(t, restarted, nil)
			good.ObservedAt = timestamppb.New(now)
			publish(t, good)
			await(t, replayed, replayConsumer, good)
			assertMarginLevel(t, restarted, good)
			stopReplay()
		})
	}
}
