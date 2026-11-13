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
			// Only this parent test goroutine advances workCtx. Subscriber and
			// process goroutines retain subctx; ctx itself is never reassigned.
			workCtx := ctx
			deadline, _ := ctx.Deadline()
			diagnostic := newRawCrashDiagnostic(deadline)
			if mode == "process-crash" {
				defer func() { t.Log(diagnostic.summary()) }()
			}
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
					diagnostic.Commit = rawCrashPhaseBudget(deadline, time.Now())
					membership := describeRawCrashGroup(t, ctx, brokers, group, diagnostic)
					if membership.GroupState != "Stable" || len(membership.Members) != 1 {
						t.Fatal("crash helper lacks exclusive active group membership")
					}
					recovery, cancelRecovery, err := rawIndependentPhase(ctx, 45*time.Second)
					if err != nil {
						t.Fatal("setup expired before crash-recovery handoff")
					}
					defer cancelRecovery()
					workCtx = recovery
					diagnostic.deadline, _ = recovery.Deadline()
					diagnostic.Phase = "crash-recovery"
					// Abrupt process termination: no FileSink.Flush/Close or deferred cleanup.
					diagnostic.Kill = rawCrashPhaseBudget(diagnostic.deadline, time.Now())
					if err := child.Process.Kill(); err != nil {
						t.Fatal("crash helper kill failed")
					}
					if err := rawKilledProcessJoined(workCtx, done); err != nil {
						t.Fatal("crash helper did not confirm nonzero join within recovery phase")
					}
					diagnostic.KillConfirmed = true
					// SIGKILL cannot send LeaveGroup. The pinned kafka-go consumer
					// uses a 30s session timeout. Observe coordinator expiry inside
					// this independent 45s recovery phase, without shortening the
					// production session timeout or reviving failed setup.
					for {
						membership = describeRawCrashGroup(t, workCtx, brokers, group, diagnostic)
						if membership.GroupState == "Empty" && len(membership.Members) == 0 {
							break
						}
						select {
						case <-workCtx.Done():
							t.Fatal("killed consumer membership did not expire")
						case <-time.After(100 * time.Millisecond):
						}
					}
				} else {
					stop()
					if err := <-done; err != nil {
						t.Fatalf("subscription shutdown: %v", err)
					}
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
				if err := client.Publish(workCtx, bus.Message{Subject: topic, Body: nextMarker}); err != nil {
					t.Fatal("next marker publish failed")
				}
			}
			readctx, readcancel, err := rawRestartReadContext(workCtx, mode == "process-crash")
			if err != nil {
				t.Fatal("raw lifecycle expired before restart read handoff")
			}
			defer readcancel()
			restart := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, Topic: topic, GroupID: group, MinBytes: 1, MaxBytes: 1 << 20})
			defer func() { _ = restart.Close() }()
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

// Each successful active phase hands off once. Setup 45s + recovery 45s + read 5s
// gives a 95s configured context-budget envelope, not a hard process wall-clock
// bound: synchronous disk I/O, cleanup and scheduling are not preempted here.
// Production Kafka keeps its 30s session; setup cannot consume expiry allowance.
func rawIndependentPhase(lifecycle context.Context, budget time.Duration) (context.Context, context.CancelFunc, error) {
	if budget != 45*time.Second && budget != 5*time.Second {
		return nil, nil, errors.New("unsupported fixture phase budget")
	}
	if err := lifecycle.Err(); err != nil {
		return nil, nil, err
	}
	deadline, bounded := lifecycle.Deadline()
	handoff := time.Now()
	if !bounded || !handoff.Before(deadline) {
		return nil, nil, context.DeadlineExceeded
	}
	ctx, cancel := context.WithDeadline(context.Background(), handoff.Add(budget))
	return ctx, cancel, nil
}

func rawRestartReadContext(lifecycle context.Context, processCrash bool) (context.Context, context.CancelFunc, error) {
	if processCrash {
		return rawIndependentPhase(lifecycle, 5*time.Second)
	}
	ctx, cancel := context.WithTimeout(lifecycle, 5*time.Second)
	return ctx, cancel, nil
}

func rawKilledProcessJoined(ctx context.Context, done <-chan error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return errors.New("killed helper exited cleanly")
		}
		return nil
	}
}

func TestRawCrashRecoveryPhaseHandoff(t *testing.T) {
	setup, cancelSetup := context.WithTimeout(context.Background(), time.Second)
	defer cancelSetup()
	recovery, cancelRecovery, err := rawIndependentPhase(setup, 45*time.Second)
	if err != nil {
		t.Fatal("active setup refused")
	}
	defer cancelRecovery()
	deadline, _ := recovery.Deadline()
	if left := time.Until(deadline); left < 44*time.Second || left > 45*time.Second {
		t.Fatal("crash recovery did not receive exactly its own bounded phase")
	}
	cancelSetup()
	if recovery.Err() != nil {
		t.Fatal("recovery inherited exhausted setup")
	}
	if _, _, err := rawIndependentPhase(setup, 45*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatal("failed setup revived")
	}
	done := make(chan error, 1)
	done <- errors.New("controlled nonzero exit")
	if rawKilledProcessJoined(recovery, done) != nil {
		t.Fatal("nonzero join refused")
	}
	done <- nil
	if rawKilledProcessJoined(recovery, done) == nil {
		t.Fatal("clean exit accepted as kill")
	}
	cancelRecovery()
	if !errors.Is(rawKilledProcessJoined(recovery, done), context.Canceled) {
		t.Fatal("blocked join ignored phase deadline")
	}
	if _, _, err := rawRestartReadContext(recovery, true); !errors.Is(err, context.Canceled) {
		t.Fatal("failed recovery revived for positive read")
	}
}

func TestRawRestartReadPhaseHandoff(t *testing.T) {
	// Context-only regression: no broker or crash-recovery acceptance is inferred.
	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	read, cancelRead, err := rawRestartReadContext(parent, true)
	if err != nil {
		t.Fatal("active lifecycle refused")
	}
	defer cancelRead()
	deadline, _ := read.Deadline()
	if remaining := time.Until(deadline); remaining < 4*time.Second || remaining > 5*time.Second {
		t.Fatal("positive read did not receive its fixed independent five-second phase")
	}
	cancelParent()
	if read.Err() != nil {
		t.Fatal("successful handoff still inherits old lifecycle cancellation")
	}
	if _, _, err := rawRestartReadContext(parent, true); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled lifecycle was revived")
	}
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if _, _, err := rawRestartReadContext(expired, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired lifecycle was revived")
	}
	if _, _, err := rawRestartReadContext(context.Background(), true); err == nil {
		t.Fatal("unbounded lifecycle accepted")
	}
	ordinary, cancelOrdinary := context.WithTimeout(context.Background(), time.Second)
	defer cancelOrdinary()
	ordinaryRead, cancelOrdinaryRead, err := rawRestartReadContext(ordinary, false)
	if err != nil {
		t.Fatal("ordinary lifecycle refused")
	}
	defer cancelOrdinaryRead()
	want, _ := ordinary.Deadline()
	got, _ := ordinaryRead.Deadline()
	if !got.Equal(want) {
		t.Fatal("non-crash mode gained additional time")
	}
	cancelOrdinary()
	if !errors.Is(ordinaryRead.Err(), context.Canceled) {
		t.Fatal("non-crash read lost parent cancellation")
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

func describeRawCrashGroup(t *testing.T, ctx context.Context, brokers []string, group string, diagnostic *rawCrashDiagnostic) kafka.DescribeGroupsResponseGroup {
	t.Helper()
	diagnostic.Polls++
	diagnostic.LastPollPhase = diagnostic.Phase
	diagnostic.LastPoll = rawCrashPhaseBudget(diagnostic.deadline, time.Now())
	admin := kafka.Client{Addr: kafka.TCP(brokers...)}
	result, err := admin.DescribeGroups(ctx, &kafka.DescribeGroupsRequest{GroupIDs: []string{group}})
	if err != nil || result == nil || len(result.Groups) != 1 || result.Groups[0].GroupID != group || result.Groups[0].Error != nil {
		diagnostic.QueryFailures++
		t.Fatal("exact crash group membership unavailable")
	}
	diagnostic.LastObserved = rawCrashPhaseBudget(diagnostic.deadline, time.Now())
	diagnostic.LastObservedPhase = diagnostic.Phase
	diagnostic.State = rawCrashGroupState(result.Groups[0].GroupState)
	diagnostic.Members = len(result.Groups[0].Members)
	return result.Groups[0]
}

// Diagnostics contain only fixed categories and numeric observations. They do not
// alter the case deadline, coordinator queries, or custody/restart assertions.
type rawCrashBudget struct {
	ElapsedMS   int64 `json:"elapsed_ms"`
	RemainingMS int64 `json:"remaining_ms"`
}

type rawCrashDiagnostic struct {
	deadline          time.Time
	Phase             string         `json:"membership_phase"`
	LastPollPhase     string         `json:"last_poll_phase"`
	LastObservedPhase string         `json:"last_observed_phase"`
	Commit            rawCrashBudget `json:"commit"`
	Kill              rawCrashBudget `json:"kill_attempt"`
	KillConfirmed     bool           `json:"kill_confirmed"`
	LastPoll          rawCrashBudget `json:"last_poll"`
	LastObserved      rawCrashBudget `json:"last_observed"`
	State             string         `json:"last_group_state"`
	Members           int            `json:"last_member_count"`
	Polls             int            `json:"poll_count"`
	QueryFailures     int            `json:"query_failures"`
}

func newRawCrashDiagnostic(deadline time.Time) *rawCrashDiagnostic {
	missing := rawCrashBudget{-1, -1}
	return &rawCrashDiagnostic{deadline: deadline, Phase: "setup", Commit: missing, Kill: missing, LastPoll: missing, LastObserved: missing, State: "unobserved", Members: -1}
}

func rawCrashPhaseBudget(deadline, now time.Time) rawCrashBudget {
	remaining := deadline.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 45*time.Second {
		remaining = 45 * time.Second
	}
	elapsed := now.Sub(deadline.Add(-45 * time.Second))
	if elapsed < 0 {
		elapsed = 0
	}
	return rawCrashBudget{ElapsedMS: elapsed.Milliseconds(), RemainingMS: remaining.Milliseconds()}
}

func rawCrashGroupState(state string) string {
	switch state {
	case "Stable", "Empty", "PreparingRebalance", "CompletingRebalance", "Dead":
		return state
	default:
		return "other"
	}
}

func (d *rawCrashDiagnostic) summary() string {
	encoded, _ := json.Marshal(d) // Fixed fields contain no unsupported JSON values.
	return "raw_crash_diagnostic=" + string(encoded)
}

func TestRawCrashDiagnosticProjection(t *testing.T) {
	deadline := time.Unix(100, 0)
	for _, tc := range []struct {
		now                time.Time
		elapsed, remaining int64
	}{
		{deadline.Add(-45 * time.Second), 0, 45000},
		{deadline.Add(-12 * time.Second), 33000, 12000},
		{deadline.Add(time.Second), 46000, 0},
	} {
		got := rawCrashPhaseBudget(deadline, tc.now)
		if got.ElapsedMS != tc.elapsed || got.RemainingMS != tc.remaining {
			t.Fatal("incorrect bounded phase budget")
		}
	}
	for _, state := range []string{"Stable", "Empty", "PreparingRebalance", "CompletingRebalance", "Dead"} {
		if rawCrashGroupState(state) != state {
			t.Fatal("known state lost")
		}
	}
	d := newRawCrashDiagnostic(deadline)
	d.State = rawCrashGroupState("private-topic-or-error")
	encoded := d.summary()
	if strings.Contains(encoded, "private-topic-or-error") || strings.Contains(encoded, "1970") || len(encoded) > 512 {
		t.Fatal("diagnostic escaped its closed projection")
	}
	if !strings.Contains(encoded, `"last_group_state":"other"`) || !strings.Contains(encoded, `"last_member_count":-1`) {
		t.Fatal("unobserved membership or unknown state fabricated")
	}
}
