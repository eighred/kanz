package replay_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/tools/replay"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// TestReaderRange is an integration test gated on TEST_KAFKA_BROKERS. It
// provisions a throwaway 2-partition topic, writes 10 EventFrame-wrapped
// envelopes per partition, then exercises both offset- and time-bounded
// reads plus a malformed-frame surface.
func TestReaderRange(t *testing.T) {
	raw := os.Getenv("TEST_KAFKA_BROKERS")
	if raw == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	brokers := strings.Split(raw, ",")

	topic := fmt.Sprintf("test.replay.%d", time.Now().UnixNano())
	conn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     2,
		ReplicationFactor: 1,
	}); err != nil {
		conn.Close()
		t.Fatalf("create topic: %v", err)
	}
	conn.Close()
	t.Cleanup(func() {
		c, err := kafka.Dial("tcp", brokers[0])
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.DeleteTopics(topic)
	})

	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.Hash{},
		RequiredAcks:           kafka.RequireAll,
		AllowAutoTopicCreation: false,
	}
	defer w.Close()

	// Write 10 framed events per partition (20 total) with monotonically
	// increasing event_time so a time-window read is meaningful.
	t0 := time.Now().Add(-time.Hour)
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("key-%d", i%2) // hashes to partition 0/1
		body := mustFrame(t, fmt.Sprintf("event-%d", i), t0.Add(time.Duration(i)*time.Second))
		if err := w.WriteMessages(context.Background(), kafka.Message{
			Key:   []byte(key),
			Value: body,
			Time:  t0.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// Append one malformed frame so we can assert the per-message error path.
	if err := w.WriteMessages(context.Background(), kafka.Message{
		Key:   []byte("key-0"),
		Value: []byte("not-an-event-frame"),
	}); err != nil {
		t.Fatalf("write malformed: %v", err)
	}

	t.Run("offset range covers everything", func(t *testing.T) {
		end := int64(99)
		r, err := replay.NewReader(replay.Config{
			Brokers: brokers,
			Topic:   topic,
			Range:   replay.Range{EndOffset: &end},
		})
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		defer r.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ok, bad := drain(t, ctx, r)
		if ok != 20 {
			t.Errorf("ok=%d want 20", ok)
		}
		if bad != 1 {
			t.Errorf("bad=%d want 1", bad)
		}
	})

	t.Run("time range filters", func(t *testing.T) {
		// Window covers events 2..6 inclusive: 5 successes total across partitions.
		start := t0.Add(2 * time.Second)
		end := t0.Add(7 * time.Second) // exclusive
		r, err := replay.NewReader(replay.Config{
			Brokers: brokers,
			Topic:   topic,
			Range:   replay.Range{StartTime: &start, EndTime: &end},
		})
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		defer r.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ok, _ := drain(t, ctx, r)
		if ok != 5 {
			t.Errorf("ok=%d want 5 (events 2..6 inclusive)", ok)
		}
	})

	t.Run("explicit partition restriction", func(t *testing.T) {
		end := int64(99)
		r, err := replay.NewReader(replay.Config{
			Brokers:    brokers,
			Topic:      topic,
			Range:      replay.Range{EndOffset: &end},
			Partitions: []int{0},
		})
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		defer r.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ok, bad := drain(t, ctx, r)
		// Hash-balanced "key-0" → partition 0 only: 10 successes + the 1 malformed.
		if ok+bad < 10 {
			t.Errorf("partition-0 events = %d (ok) + %d (bad), want >=10", ok, bad)
		}
	})
}

func drain(t *testing.T, ctx context.Context, r *replay.Reader) (ok, bad int) {
	t.Helper()
	for {
		ev, err := r.Next(ctx)
		if errors.Is(err, io.EOF) {
			return
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Next: %v (reader did not terminate before the context expired)", err)
			return
		}
		if err != nil && ev.Envelope == nil {
			bad++
			continue
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ev.Envelope == nil {
			t.Fatalf("nil envelope on success path")
		}
		ok++
	}
}

func mustFrame(t *testing.T, eventID string, eventTime time.Time) []byte {
	t.Helper()
	env := &envelopepb.Envelope{
		EventId:          eventID,
		EventType:        "test.replay.event",
		SchemaVersion:    1,
		EnvelopeVersion:  1,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		Domain:           "test",
		EventTime:        timestamppb.New(eventTime),
		IngestionTime:    timestamppb.New(eventTime),
		PublishTime:      timestamppb.New(eventTime),
		CorrelationId:    eventID,
		Source:           "replay-test",
		ProducerVersion:  "test",
		IdempotencyKey:   eventID,
		PayloadSchemaRef: "test.v1:1",
	}
	frame := &envelopepb.EventFrame{Envelope: env, Payload: []byte("body")}
	b, err := proto.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
