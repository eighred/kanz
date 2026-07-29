package cdc_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lake-sink/internal/cdc"
	"github.com/eighred/kanz/services/lake-sink/internal/decode"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
)

// TestCrash_UnflushedRowsAreLostAndOffsetsAreCommitted is the DATA-M5 crash
// reproduction. (It cited a plan under docs/, deleted 2026-07-29 and available
// in git history — but the reproduction below is the durable artifact, which is
// the point: a test that demonstrates the failure outlives any document
// describing it.)
//
// It models a hard pod crash faithfully: after EventSink.Handle returns nil for
// each of 10 delivered messages — which makes pkg/bus/kafka.go's Subscribe loop
// commit the Kafka offset SYNCHRONOUSLY, per message — the test cancels the
// context and returns without ever calling Flush or Close on the sink. A
// SIGKILLed process never gets the chance to run either one; that is exactly
// what this simulates. The bufio.Writer's buffered bytes are process-local
// userspace memory and die with it.
//
// Before the DATA-M5 fix: this test FAILS — rows on disk are far short of 10
// (expect 0), because Handle returns nil the instant the row is buffered, long
// before anything reaches the fd. After the fix: Handle flushes (bufio.Flush +
// fsync) before returning nil, so all 10 rows are already durable by the time
// the (simulated) crash happens.
func TestCrash_UnflushedRowsAreLostAndOffsetsAreCommitted(t *testing.T) {
	raw := os.Getenv("TEST_KAFKA_BROKERS")
	if raw == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	brokers := strings.Split(raw, ",")

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	topic := "durability-test-" + suffix
	group := "durability-test-group-" + suffix

	// Out-of-band topic provisioning: KAFKA_AUTO_CREATE_TOPICS_ENABLE=false in
	// CI and production, same as pkg/bus/kafka_integration_test.go and
	// services/archiver/internal/archive/archiver_integration_test.go.
	conn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatalf("kafka dial: %v", err)
	}
	if err := conn.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     1,
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

	// --- Produce N=10 small envelopes onto the fresh topic. ---
	//
	// Built by hand (not via bus.Producer) and kept deliberately tiny: every
	// bus.Producer-stamped envelope carries two 36-byte UUIDv7 strings
	// (event_id, correlation_id), which alone push 10 rows' marshaled JSON
	// past bufio's 4096-byte default buffer (~4360 bytes measured) — enough to
	// trigger bufio's own internal auto-flush mid-write and land rows on disk
	// for the WRONG reason, making this test pass before the fix even exists.
	// These hand-built envelopes marshal to ~280 bytes each (~2.8KB for all
	// 10), safely under the buffer size, so the only thing that can put bytes
	// on disk is an explicit Flush.
	const n = 10
	producerClient, err := bus.DialKafka(bus.KafkaConfig{Brokers: brokers, ClientID: "durability-test-producer"})
	if err != nil {
		t.Fatalf("DialKafka (producer): %v", err)
	}
	defer func() { _ = producerClient.Close() }()

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("e%d", i)
		now := timestamppb.New(time.Now())
		env := &envelopepb.Envelope{
			EventId:          id,
			EventType:        "t",
			SchemaVersion:    1,
			EnvelopeVersion:  1,
			EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
			Domain:           "d",
			EventTime:        now,
			IngestionTime:    now,
			PublishTime:      now,
			CorrelationId:    id,
			Source:           "s",
			ProducerVersion:  "v",
			IdempotencyKey:   id, // FACT requires idempotency_key == event_id
			PayloadSchemaRef: "r:1",
			TenantId:         "a",
		}
		body, err := proto.Marshal(&envelopepb.EventFrame{Envelope: env})
		if err != nil {
			t.Fatalf("marshal frame %d: %v", i, err)
		}
		if err := producerClient.Publish(context.Background(), bus.Message{Subject: topic, Body: body}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	// --- Wire the real chain exactly as cmd/lake-sink/main.go's runSink does. ---
	// A nil registry resolver ⇒ rows land envelope-only: this test is about
	// durability, not decode.
	landingRoot := t.TempDir()
	fileSink, err := sink.NewFileSink(landingRoot, "crash-test")
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	// Deliberately NOT called before the durability assertion below — that is
	// the crash being simulated. Registered as a t.Cleanup (LIFO: runs before
	// t.TempDir's own RemoveAll cleanup) purely so the open file handle
	// doesn't wedge the test's temp-dir removal on Windows; it runs strictly
	// after every load-bearing assertion in this test.
	t.Cleanup(func() { _ = fileSink.Close() })
	eventSink := cdc.NewEventSink(decode.NewDecoder(nil), fileSink, time.Now, nil, nil)

	consumerClient, err := bus.DialKafka(bus.KafkaConfig{Brokers: brokers, ClientID: "durability-test-consumer"})
	if err != nil {
		t.Fatalf("DialKafka (consumer): %v", err)
	}
	defer func() { _ = consumerClient.Close() }()
	consumer, err := bus.NewConsumer(consumerClient)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	// --- Consume all 10. Each Handle returning nil commits the offset
	// synchronously (pkg/bus/kafka.go). Then simulate the crash: let the
	// context expire and walk away. NO Flush. NO Close.
	//
	// The deadline (not a manual cancel triggered from inside the handler) is
	// deliberate: canceling ctx from within the handler races the very commit
	// it is meant to happen after — the bus's Subscribe loop calls
	// r.CommitMessages(ctx, m) on the SAME ctx immediately after the handler
	// returns, on the same goroutine, so a synchronous cancel there can
	// interrupt that commit before it reaches the broker. A deadline expires
	// only once the loop is blocked waiting on an 11th message that never
	// arrives — strictly after the 10th commit has already completed — which
	// is what actually models "the process is gone" without corrupting the
	// very commit this test depends on having happened.
	var handled int64
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	subscribeDone := make(chan struct{})
	go func() {
		defer close(subscribeDone)
		_ = consumer.Subscribe(ctx, topic, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			err := eventSink.Handle(ctx, env, payload)
			if err == nil {
				atomic.AddInt64(&handled, 1)
			}
			return err
		})
	}()

	<-subscribeDone

	if got := atomic.LoadInt64(&handled); got != n {
		t.Fatalf("handled %d of %d messages before the simulated crash", got, n)
	}

	// --- Assert: rows readable FROM DISK, not from the bufio buffer. This is
	// the whole point — what is on disk is what survives a crash. ---
	if onDisk := countNDJSONLines(t, landingRoot); onDisk != n {
		t.Fatalf("rows on disk after the simulated crash = %d, want exactly %d — "+
			"the offset was committed for rows that were never made durable", onDisk, n)
	}

	// --- Prove the loss is PERMANENT, not lag: a second consumer on the SAME
	// group must receive ZERO messages, because the offsets were already
	// committed. Without this, one could believe the periodic flush ticker
	// would eventually have saved the data; it would not, because nothing ever
	// re-reads a committed offset. ---
	root2 := t.TempDir()
	fileSink2, err := sink.NewFileSink(root2, "crash-test-2")
	if err != nil {
		t.Fatalf("NewFileSink (2nd consumer): %v", err)
	}
	t.Cleanup(func() { _ = fileSink2.Close() })
	eventSink2 := cdc.NewEventSink(decode.NewDecoder(nil), fileSink2, time.Now, nil, nil)
	consumer2, err := bus.NewConsumer(consumerClient)
	if err != nil {
		t.Fatalf("NewConsumer (2nd): %v", err)
	}

	var handled2 int64
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := consumer2.Subscribe(ctx2, topic, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
		atomic.AddInt64(&handled2, 1)
		return eventSink2.Handle(ctx, env, payload)
	}); err != nil {
		t.Fatalf("second consumer subscribe: %v", err)
	}
	if got := atomic.LoadInt64(&handled2); got != 0 {
		t.Fatalf("a second consumer on the SAME group received %d messages — "+
			"this would mean the loss was only lag, not permanent", got)
	}
}

// countNDJSONLines walks root and counts non-empty lines across every
// part-*.ndjson file — i.e. rows actually durable on disk, as opposed to
// whatever a bufio.Writer happens to still be holding in process memory.
func countNDJSONLines(t *testing.T, root string) int {
	t.Helper()
	total := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".ndjson") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) != "" {
				total++
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatalf("walk landing root: %v", err)
	}
	return total
}
