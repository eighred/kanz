// THE #219 DATA-LOSS REPRODUCTION, INVERTED INTO A REGRESSION TEST.
//
// Before the fix, KafkaClient.Subscribe answered a handler error with `continue`
// on the claim that the message would be "redelivered on next fetch". It is not:
// FetchMessage has already advanced the reader's own position, so the skip is
// in-process and the NEXT successful CommitMessages writes a group offset past
// the skipped message. Reproduced against apache/kafka:3.9.0 — A committed at
// offset 0, B skipped, C committed at offset 2 ⇒ group offset 3, and
// re-subscribing on the same group never saw B again.
//
// Nothing short of a real broker can show this. The skip is a property of Kafka
// group-offset semantics, not of this package's control flow, and every fake in
// this repository commits what it is told to commit.
//
//	kanz/test/backing/up.sh          # brings up kanz-ci-kafka on :9092
//	TEST_KAFKA_BROKERS=localhost:9092 go test -run Halt ./pkg/bus/...
package bus_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/kafkatest"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

// deadDLQ is a dead-letter destination that is DOWN. Injected rather than
// achieved by pointing WithDLQ at a nonexistent topic, for two reasons: the
// unavailability is the test's PRECONDITION, not the thing under test, and a
// missing-topic publish only fails if the broker under test happens to have
// auto-create disabled — a rig setting, not something this test should depend
// on. What is under test is what KafkaClient.Subscribe does with the error.
type deadDLQ struct {
	mu    sync.Mutex
	calls int
}

func (d *deadDLQ) Publish(context.Context, bus.Message) error {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return errors.New("dead-letter destination unavailable")
}

func (d *deadDLQ) Calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// markerRecorder collects the partition keys the handler was dispatched with,
// in order, and fails the delivery whose key is failOn.
type markerRecorder struct {
	mu     sync.Mutex
	seen   []string
	failOn string
}

func (r *markerRecorder) handle(_ context.Context, env *envelopepb.Envelope, _ []byte) error {
	r.mu.Lock()
	r.seen = append(r.seen, env.GetPartitionKey())
	r.mu.Unlock()
	if env.GetPartitionKey() == r.failOn {
		return errors.New("handler refuses " + r.failOn)
	}
	return nil
}

func (r *markerRecorder) Seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (r *markerRecorder) Has(marker string) bool {
	for _, s := range r.Seen() {
		if s == marker {
			return true
		}
	}
	return false
}

func TestIntegration_KafkaHandlerFailureWithDeadDLQKeepsTheMessageReachable(t *testing.T) {
	raw := os.Getenv("TEST_KAFKA_BROKERS")
	if raw == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	brokers := strings.Split(raw, ",")

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	topic := "test.bus.halt." + suffix
	// ONE group id across both phases. The whole defect is about the durable
	// GROUP offset, so a second group id would make phase 2 read from the
	// beginning and the test would pass on a broken consumer.
	group := "test-halt-group-" + suffix

	// ONE PARTITION. With more, B and C could land on different partitions and
	// "C was not handled" would prove ordering, not a held offset.
	createProbeTopic(t, brokers, topic, 1)

	// Metrics wired the way lake-sink wires them, so the halt COUNTER is proved on
	// the same run as the halt itself. Asserting it separately against a fake
	// would only prove the Inc() call exists, not that the branch reaching it is
	// the branch a real broker takes.
	reg := prometheus.NewRegistry()
	busMetrics := bus.NewBusMetrics(reg)

	client, err := bus.DialKafka(bus.KafkaConfig{Brokers: brokers, ClientID: "halt-probe", Metrics: busMetrics})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	prod, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "test/halt-probe", ProducerVersion: "v1", Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	publish := func(marker string) {
		t.Helper()
		et := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
		if err := prod.Publish(context.Background(), bus.Event{
			Subject:          topic,
			EventType:        "kanztest.haltprobe",
			EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion:    1,
			Domain:           "market",
			EventTime:        et,
			PartitionKey:     marker,
			PayloadSchemaRef: "kanztest.haltprobe.v1:1",
			Payload:          timestamppb.New(et),
		}); err != nil {
			t.Fatalf("publish %s: %v", marker, err)
		}
	}
	for _, marker := range []string{"A", "B", "C"} {
		publish(marker)
	}

	// ---- PHASE 1: B fails, and the dead-letter path is down. ----------------

	dlq1 := &deadDLQ{}
	consumer1, err := bus.NewConsumer(client, bus.WithDLQ(dlq1))
	if err != nil {
		t.Fatalf("NewConsumer phase1: %v", err)
	}
	rec1 := &markerRecorder{failOn: "B"}

	phase1Ctx, stop1 := context.WithCancel(context.Background())
	defer stop1()
	subErr := make(chan error, 1)
	go func() { subErr <- consumer1.Subscribe(phase1Ctx, topic, group, rec1.handle) }()

	// PHASE 1 ENDS ON WHICHEVER COMES FIRST — Subscribe returning (the fix), or C
	// being handled (the defect: the loop read past a message it never committed).
	// It deliberately does NOT t.Fatal on the second: the whole point of this test
	// is the state phase 2 then finds, and stopping here would report a stalled
	// subscription rather than the lost message it causes.
	var (
		phase1Err      error
		phase1Returned bool
	)
	deadline := time.Now().Add(45 * time.Second)
	for !phase1Returned && !rec1.Has("C") && time.Now().Before(deadline) {
		select {
		case phase1Err = <-subErr:
			phase1Returned = true
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !phase1Returned {
		stop1()
		<-subErr // let the subscription unwind before phase 2 joins the group
	}

	// NON-VACUITY, and the precondition this test is actually about: the DLQ path
	// was attempted and refused. Without this, a Subscribe that ended for any
	// other reason (a dial error, a rebalance) would read as proof.
	if dlq1.Calls() == 0 {
		t.Fatalf("the dead-letter publisher was never called, so phase1 did not exercise the "+
			"DLQ-unavailable path at all; phase1 saw %v and Subscribe returned %v",
			rec1.Seen(), phase1Err)
	}

	// NON-VACUITY, other half: the reader was healthy and delivering. If A never
	// arrived, everything below is a broken consumer rather than a held offset.
	if !rec1.Has("A") {
		t.Fatalf("phase1 never handled A (saw %v) — the consumer was not delivering, so nothing "+
			"below is evidence about offsets", rec1.Seen())
	}
	if !rec1.Has("B") {
		t.Fatalf("phase1 never handled B (saw %v) — the failure under test never happened", rec1.Seen())
	}

	// THE HALT, observed from inside phase 1.
	if !phase1Returned {
		t.Errorf("phase1 Subscribe did not return after B failed with the dead-letter path down "+
			"(saw %v) — a delivery that could be neither handled nor parked must end the "+
			"subscription, not be consumed past", rec1.Seen())
	} else if phase1Err == nil {
		t.Error("phase1 Subscribe returned nil — the delivery was neither handled nor dead-lettered, " +
			"so the only correct outcome is a non-nil error and an uncommitted offset")
	}
	// Reaching C means the loop read past a message it had not committed, which is
	// precisely the state in which committing C buries B.
	if rec1.Has("C") {
		t.Errorf("phase1 handled C after failing B (saw %v) — the loop read past an uncommitted "+
			"message, and committing C writes a group offset past B", rec1.Seen())
	}

	// THE HALT MUST BE VISIBLE. Every other bus series reads normal here:
	// kanz_bus_consume_total counted the failed dispatch and then stopped moving,
	// and lag is computed off the committed offset, which correctly did not
	// advance. Without this counter a stopped sink and an idle one look identical.
	if got := counterValue(t, reg, "kanz_bus_consume_halted_total", map[string]string{
		"subject": topic, "group": group,
	}); got != 1 {
		t.Errorf("kanz_bus_consume_halted_total{subject=%q,group=%q} = %v, want 1 — the halt "+
			"happened but nothing an operator can alert on recorded it", topic, group, got)
	}

	// ---- PHASE 2: same group, healthy handler. B must still be there. -------

	dlq2 := &deadDLQ{}
	consumer2, err := bus.NewConsumer(client, bus.WithDLQ(dlq2))
	if err != nil {
		t.Fatalf("NewConsumer phase2: %v", err)
	}
	rec2 := &markerRecorder{} // failOn "" — nothing fails now

	phase2Ctx, stop2 := context.WithCancel(context.Background())
	defer stop2()
	go func() { _ = consumer2.Subscribe(phase2Ctx, topic, group, rec2.handle) }()

	// THE LIVE CONTROL. D is produced after phase 2 has had time to join the
	// group, and it MUST arrive. Without it, "B is missing" is indistinguishable
	// from a consumer that never joined — which is exactly how a broken rig would
	// fake a passing assertion in the other direction.
	//
	// The join wait is bounded and NOT asserted on: when the offset was buried
	// there is nothing pending to deliver, so waiting for a first delivery would
	// hang the whole window on precisely the run this test needs to reach.
	waitFor(t, 20*time.Second, func() bool { return len(rec2.Seen()) > 0 })
	publish("D")

	waitFor(t, 45*time.Second, func() bool { return rec2.Has("D") })
	if !rec2.Has("D") {
		t.Fatalf("the live control D never arrived (saw %v) — the phase2 consumer is not healthy, "+
			"so its view of B proves nothing", rec2.Seen())
	}

	// THE ASSERTION. Pre-fix this fails: B was skipped in phase 1 and buried by
	// the commit of C, so the same group resumes past it forever.
	if !rec2.Has("B") {
		t.Errorf("B was never redelivered to the same group (phase2 saw %v) while the live control "+
			"D did arrive — the failed message was committed past and is permanently unreachable",
			rec2.Seen())
	}
	// C was published before B failed and was never handled in phase 1; if B is
	// reachable then C must be too, and its absence would mean the held offset
	// was restored at the wrong place.
	if !rec2.Has("C") {
		t.Errorf("C was never delivered to the same group (phase2 saw %v) — the group resumed past "+
			"messages that were never committed", rec2.Seen())
	}
}

// createProbeTopic provisions a scratch topic out of band and deletes it on
// cleanup. Production topics come from kanz/infra/kafka/topics-job.yaml and
// auto-create is disabled (KafkaConfig.AllowAutoTopicCreation=false).
// createProbeTopic provisions a topic and does not return until it is writable.
//
// THE WAIT IS THE POINT (#311). This used to issue CreateTopics and return, but
// the broker acknowledges the request and settles metadata asynchronously, so the
// publish below raced it and failed UNKNOWN_TOPIC_OR_PARTITION — a red main with
// no code defect, and an error that reads like the topic was never created.
// Auto-creation is off on this broker deliberately, so waiting is the only
// option. internal/kafkatest carries the condition and the measurements behind
// it.
func createProbeTopic(t *testing.T, brokers []string, topic string, partitions int) {
	t.Helper()
	if err := kafkatest.CreateTopic(context.Background(), brokers, topic, partitions); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() {
		c, err := kafka.Dial("tcp", brokers[0])
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.DeleteTopics(topic)
	})
}

// counterValue reads one labelled counter sample out of reg. Returns -1 when
// the series is absent, which is distinct from a present-but-zero counter — the
// two mean different things and a caller asserting == 1 must not be able to
// confuse "never registered" with "registered and never incremented".
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.Metric {
			match := true
			for _, lp := range met.GetLabel() {
				if want, ok := labels[lp.GetName()]; ok && want != lp.GetValue() {
					match = false
					break
				}
			}
			if match {
				return met.GetCounter().GetValue()
			}
		}
	}
	return -1
}

// waitFor polls cond until it holds or the deadline passes. It does NOT fail —
// the caller asserts afterwards, so the failure message can name what was
// actually seen rather than a generic timeout.
func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(limit); !cond() && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
}
