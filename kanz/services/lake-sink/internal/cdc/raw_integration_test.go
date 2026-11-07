package cdc_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/eighred/kanz/internal/kafkatest"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lake-sink/internal/cdc"
	"github.com/eighred/kanz/services/lake-sink/internal/decode"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type rawFlushFailure struct{ sink.Sink }

func (s rawFlushFailure) Flush() error { return errors.New("controlled fsync failure") }

type rawParkingFailure struct{}

func (rawParkingFailure) Publish(context.Context, bus.Message) error {
	return errors.New("controlled parking failure")
}

func TestRawKafkaCustodyAndOffsets(t *testing.T) {
	brokers := strings.Split(os.Getenv("TEST_KAFKA_BROKERS"), ",")
	if brokers[0] == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	for _, mode := range []string{"process-crash", "flush-failure", "flush-failure-dlq-failure", "flush-failure-dlq-success"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			topic := fmt.Sprintf("raw-custody-%d", time.Now().UnixNano())
			group := topic + "-group"
			for _, name := range []string{topic, "dlq." + topic} {
				if err := kafkatest.CreateTopic(ctx, brokers, name, 1); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cleanupCancel()
					c, err := kafka.DialContext(cleanupCtx, "tcp", brokers[0])
					if err == nil {
						defer func() { _ = c.Close() }()
						_ = c.DeleteTopics(name)
					}
				})
			}
			client, err := bus.DialKafka(bus.KafkaConfig{Brokers: brokers, ClientID: "raw-custody"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			now := timestamppb.Now()
			id := "raw-event"
			envelope := &envelopepb.Envelope{EventId: id, IdempotencyKey: id, CorrelationId: id, EventType: "event", Domain: "d", TenantId: "a", Source: "fixture", ProducerVersion: "v", SchemaVersion: 1, EnvelopeVersion: 1, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, EventTime: now, IngestionTime: now, PublishTime: now, PayloadSchemaRef: "r:1"}
			original, err := proto.Marshal(&envelopepb.EventFrame{Envelope: envelope})
			if err != nil {
				t.Fatal(err)
			}
			original = append(original, 0x0a, 0x00, 0xa0, 0x06, 0x81, 0x00)
			if err := client.Publish(ctx, bus.Message{Subject: topic, Body: original, Kafka: &bus.KafkaDelivery{Topic: "must-not-publish", Partition: 99, Offset: 99}}); err != nil {
				t.Fatal(err)
			}
			reader := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, Topic: topic, Partition: 0, MinBytes: 1, MaxBytes: 1 << 20})
			independent, err := reader.ReadMessage(ctx)
			_ = reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(independent.Value, original) {
				t.Fatal("publisher changed bytes or used receive metadata")
			}
			root := t.TempDir()
			file, err := sink.NewFileSink(root, "raw")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() }) // assertions deliberately precede Close
			var target sink.Sink = file
			if mode != "process-crash" {
				target = rawFlushFailure{file}
			}
			es := cdc.NewEventSink(decode.NewDecoder(nil), target, time.Now, nil, nil)
			options := []bus.ConsumerOption{}
			if mode == "flush-failure-dlq-failure" {
				options = append(options, bus.WithDLQ(rawParkingFailure{}))
			}
			if mode == "flush-failure-dlq-success" {
				options = append(options, bus.WithDLQ(client))
			}
			consumer, err := bus.NewConsumer(client, options...)
			if err != nil {
				t.Fatal(err)
			}
			subctx, stop := context.WithCancel(ctx)
			defer stop()
			done := make(chan error, 1)
			finished := make(chan struct{})
			t.Cleanup(func() {
				stop()
				select {
				case <-finished:
				case <-time.After(10 * time.Second):
					t.Error("subscription failed to join")
				}
			})
			var child *exec.Cmd
			if mode == "process-crash" {
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				child = exec.CommandContext(subctx, executable, "-test.run=^TestRawKafkaCrashChild$", "-lake-raw-crash-root="+root, "-lake-raw-crash-topic="+topic, "-lake-raw-crash-group="+group)
				child.Stdout = io.Discard
				child.Stderr = io.Discard
				if err := child.Start(); err != nil {
					t.Fatal("crash helper start failed")
				}
				go func() { defer close(finished); done <- child.Wait() }()
			} else {
				go func() {
					defer close(finished)
					done <- consumer.SubscribeDelivery(subctx, topic, group, es.HandleDelivery)
				}()
			}
			committed := mode == "process-crash" || mode == "flush-failure-dlq-success"
			if committed {
				admin := kafka.Client{Addr: kafka.TCP(brokers...)}
				for {
					fetched, err := admin.OffsetFetch(ctx, &kafka.OffsetFetchRequest{GroupID: group, Topics: map[string][]int{topic: {0}}})
					if err == nil && len(fetched.Topics[topic]) == 1 && fetched.Topics[topic][0].CommittedOffset == independent.Offset+1 {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("offset never committed")
					case <-time.After(25 * time.Millisecond):
					}
				}
				if child != nil {
					// Abrupt process termination: no FileSink.Flush/Close or deferred cleanup.
					if err := child.Process.Kill(); err != nil {
						t.Fatal("crash helper kill failed")
					}
					if err := <-done; err == nil {
						t.Fatal("crash helper exited cleanly")
					}
				} else {
					stop()
					<-done
				}
			} else {
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("failed landing acknowledged")
					}
				case <-ctx.Done():
					t.Fatal("failure did not stop subscription")
				}
			}
			if mode == "process-crash" {
				files, err := filepath.Glob(filepath.Join(root, "*", "*", "dt=*", "*.ndjson"))
				if err != nil || len(files) != 1 {
					t.Fatalf("files=%v err=%v", files, err)
				}
				f, err := os.Open(files[0])
				if err != nil {
					t.Fatal(err)
				}
				scan := bufio.NewScanner(f)
				if !scan.Scan() {
					_ = f.Close()
					t.Fatal("raw row absent before Close")
				}
				var row sink.Row
				if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
					_ = f.Close()
					t.Fatal(err)
				}
				_ = f.Close()
				raw := row.RawCapture
				sum := sha256.Sum256(original)
				if raw == nil || !bytes.Equal(raw.Frame, original) || raw.SHA256 != hex.EncodeToString(sum[:]) || raw.Kafka == nil || raw.Kafka.Topic != independent.Topic || raw.Kafka.Partition != independent.Partition || raw.Kafka.Offset != independent.Offset || raw.SourceIdentity != nil {
					t.Fatal("disk custody differs from actual Kafka record")
				}
			}
			if mode == "flush-failure-dlq-success" {
				r := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, Topic: "dlq." + topic, Partition: 0, MinBytes: 1, MaxBytes: 1 << 20})
				parked, err := r.ReadMessage(ctx)
				_ = r.Close()
				if err != nil || !bytes.Equal(parked.Value, original) {
					t.Fatalf("parking lost original bytes: %v", err)
				}
			}
			var nextMarker []byte
			if committed {
				nextEnvelope := proto.Clone(envelope).(*envelopepb.Envelope)
				nextEnvelope.EventId = "next-marker"
				nextEnvelope.IdempotencyKey = "next-marker"
				nextMarker, err = proto.Marshal(&envelopepb.EventFrame{Envelope: nextEnvelope})
				if err != nil {
					t.Fatal(err)
				}
				if err := client.Publish(ctx, bus.Message{Subject: topic, Body: nextMarker}); err != nil {
					t.Fatal("next marker publish failed")
				}
			}
			restart := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, Topic: topic, GroupID: group, MinBytes: 1, MaxBytes: 1 << 20})
			defer func() { _ = restart.Close() }()
			readctx, readcancel := context.WithTimeout(ctx, 5*time.Second)
			defer readcancel()
			replay, err := restart.FetchMessage(readctx)
			if committed {
				if err != nil || replay.Offset != independent.Offset+1 || !bytes.Equal(replay.Value, nextMarker) {
					t.Fatalf("restarted group did not read exact next marker: %v", err)
				}
			} else if err != nil || replay.Offset != independent.Offset || !bytes.Equal(replay.Value, original) {
				t.Fatalf("uncommitted original not redelivered: %v", err)
			}
		})
	}
}

var rawCrashRoot = flag.String("lake-raw-crash-root", "", "public disposable raw landing directory")
var rawCrashTopic = flag.String("lake-raw-crash-topic", "", "public disposable Kafka topic")
var rawCrashGroup = flag.String("lake-raw-crash-group", "", "public disposable Kafka group")

func TestRawKafkaCrashChild(t *testing.T) {
	if *rawCrashRoot == "" {
		return
	}
	if *rawCrashTopic == "" || *rawCrashGroup == "" {
		t.Fatal("incomplete crash fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := bus.DialKafka(bus.KafkaConfig{Brokers: strings.Split(os.Getenv("TEST_KAFKA_BROKERS"), ","), ClientID: "raw-crash-child"})
	if err != nil {
		t.Fatal("child transport failed")
	}
	defer func() { _ = client.Close() }()
	file, err := sink.NewFileSink(*rawCrashRoot, "raw-child")
	if err != nil {
		t.Fatal("child landing failed")
	}
	// Intentionally no Close: the parent kills this process after observing the
	// broker offset commit, then independently reads disk and restarts the group.
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	es := cdc.NewEventSink(decode.NewDecoder(nil), file, time.Now, nil, nil)
	if err := consumer.SubscribeDelivery(ctx, *rawCrashTopic, *rawCrashGroup, es.HandleDelivery); err != nil {
		t.Fatal("child subscription ended")
	}
	t.Fatal("child must be killed before its deadline")
}
