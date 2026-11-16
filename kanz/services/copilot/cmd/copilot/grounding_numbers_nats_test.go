package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/bus"
)

type groundingConsumerID struct{ stream, name string }

// Observe the actual broker-generated name, without guessing it from timing or
// replacing the production Consumer's decoding/validation/replay/ACK path.
type groundingReplayClient struct {
	*bus.NATSClient
	created chan groundingConsumerID
}

func (c *groundingReplayClient) SubscribeReplayObserved(ctx context.Context, subject string, h bus.Handler, ready func(), created bus.ConsumerCreated) error {
	return c.NATSClient.SubscribeReplayObserved(ctx, subject, h, ready, func(stream, name string) {
		c.created <- groundingConsumerID{stream, name}
		created(stream, name)
	})
}

type groundingDelivery struct {
	envelope *envelopepb.Envelope
	payload  []byte
	record   *observationpb.AgentAnswerRecord
}

// This is a retained APPEND-LOG proof, not a new audit database projection.
// PLATFORM is the canonical 168-hour stream; no fallback stream is created.
func TestGroundingNumbersReachCanonicalObservationReplay(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL with the canonical PLATFORM stream")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	principal := "grounding-" + uuid.NewString()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	streamName, err := js.StreamNameBySubject(ctx, SubjectAgentAnswer)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if streamName != "PLATFORM" || info.Config.MaxAge != 168*time.Hour || info.Config.MaxMsgsPerSubject == 1 || info.Config.Retention != jetstream.LimitsPolicy {
		t.Fatalf("fixture lacks canonical append-only PLATFORM retention: %+v", info.Config)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: principal})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "copilot", ProducerVersion: "grounding-test"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &busAnswerRecorder{producer: producer} // same no-default-tenant composition as main
	var owned []groundingConsumerID
	// LIFO: join subscriptions, delete only our consumers, then close sockets.
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, id := range owned {
			if err := js.DeleteConsumer(cleanupCtx, id.stream, id.name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
				t.Errorf("delete consumer %s/%s: %v", id.stream, id.name, err)
			}
		}
	})
	startReplay := func(t *testing.T) (<-chan groundingDelivery, jetstream.Consumer, func()) {
		t.Helper()
		observed := &groundingReplayClient{NATSClient: client, created: make(chan groundingConsumerID, 1)}
		consumer, err := bus.NewConsumer(observed, bus.WithDLQ(client))
		if err != nil {
			t.Fatal(err)
		}
		messages := make(chan groundingDelivery, 32)
		ready := make(chan struct{})
		subCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- consumer.SubscribeReplay(subCtx, SubjectAgentAnswer, func(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
				var record observationpb.AgentAnswerRecord
				if err := proto.Unmarshal(payload, &record); err != nil {
					return err
				}
				if record.GetPrincipalSubject() != principal {
					return nil
				}
				select {
				case messages <- groundingDelivery{proto.Clone(env).(*envelopepb.Envelope), bytes.Clone(payload), &record}:
					return nil
				case <-subCtx.Done():
					return subCtx.Err()
				}
			}, func() { close(ready) })
			close(done)
		}()
		var once sync.Once
		join := func() {
			once.Do(func() {
				stop()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Errorf("stop replay: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("replay did not stop before connection cleanup")
				}
			})
		}
		t.Cleanup(join)
		var id groundingConsumerID
		select {
		case id = <-observed.created:
		case <-ctx.Done():
			t.Fatal("canonical replay consumer not created:", ctx.Err())
		}
		owned = append(owned, id)
		brokerConsumer, err := js.Consumer(ctx, id.stream, id.name)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("replay stopped before catchup: %v", err)
		case <-ctx.Done():
			t.Fatal("canonical replay did not catch up:", ctx.Err())
		}
		return messages, brokerConsumer, join
	}
	waitAck := func(t *testing.T, c jetstream.Consumer) {
		t.Helper()
		for {
			info, err := c.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.NumPending == 0 && info.NumAckPending == 0 && info.AckFloor.Consumer >= uint64(len(groundingCases())) {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("canonical observation ACK incomplete: %+v (%v)", info, ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	await := func(t *testing.T, ch <-chan groundingDelivery) groundingDelivery {
		t.Helper()
		select {
		case msg := <-ch:
			return msg
		case <-ctx.Done():
			t.Fatalf("no %s observation for principal %s: %v", SubjectAgentAnswer, principal, ctx.Err())
		}
		return groundingDelivery{}
	}
	messages, liveConsumer, stopLive := startReplay(t)
	want := map[string]groundingDelivery{}
	for _, tc := range groundingCases() {
		t.Run(tc.name, func(t *testing.T) {
			expected := askGroundingNumber(t, tc, principal, "t1", recorder)
			msg := await(t, messages)
			env := msg.envelope
			if err := bus.Validate(env); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(expected, msg.record) || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_OBSERVATION || env.GetEventType() != SubjectAgentAnswer || env.GetDomain() != "platform" || env.GetPayloadSchemaRef() != "observation.v1.AgentAnswerRecord:1" || env.GetSource() != "copilot" || env.GetTenantId() != "t1" || env.GetPartitionKey() != principal || !proto.Equal(env.GetEventTime(), expected.GetAnsweredAt()) || env.GetIdempotencyKey() != env.GetEventId() {
				t.Fatalf("canonical observation lost Agent verdict/lineage/identity: %v %v", env, msg.record)
			}
			if _, exists := want[expected.GetAnswerId()]; exists {
				t.Fatal("independent answers reused an identity")
			}
			want[expected.GetAnswerId()] = msg
		})
	}
	if len(want) != len(groundingCases()) {
		t.Fatal("a live answer case failed; restart cannot stand in for missing publication")
	}
	waitAck(t, liveConsumer)
	stopLive()
	t.Run("retained_restart", func(t *testing.T) {
		replayed, replayConsumer, _ := startReplay(t)
		seen := map[string]bool{}
		for range len(want) {
			msg := await(t, replayed)
			id := msg.record.GetAnswerId()
			previous, ok := want[id]
			if !ok || seen[id] || !bytes.Equal(previous.payload, msg.payload) || !proto.Equal(previous.envelope, msg.envelope) {
				t.Fatalf("fresh replay changed/duplicated an accepted answer %s", id)
			}
			seen[id] = true
		}
		waitAck(t, replayConsumer)
	})
}
