// THE DRAIN, END TO END, ON A REAL KAFKA (#285's last assertion).
//
// drain_test.go proves the drain's decisions against fakes: the gate refuses, the
// read is bounded, an unroutable event is re-parked. What no test covered is the
// ROUND TRIP — that a message really parked on dlq.archiver is really read back,
// really re-routed by the current table, and really lands on the destination
// topic a consumer reads. That is the whole promise of the tool, and it is the
// one part a fake cannot make: the archiver parks onto ONE topic and the drain
// must produce onto a DIFFERENT one, so every step in between is Kafka's.
//
// It is the third of the three assertions #285's design record asked for. The
// other two shipped: the gate's refusal is drain_test.go's
// TestDrainRefusesWhileTheArchiverIsConsuming, and the structural guard is
// test/arch/drain_single_writer_gate_test.go.
//
// WHY THE GATE IS A STUB HERE, AND WHY THAT IS NOT A HOLE. Driving the real
// bus.NATSClient gate would mean creating the archiver's durable, stopping it,
// and waiting for its pull requests to EXPIRE — up to ~90s, which is what
// pkg/bus/consumer_active_integration_test.go budgets for exactly that
// transition. That test already proves the gate against a live broker in both
// directions. Paying the same 90s here would re-prove it and make this test slow
// enough to be skipped, while proving nothing new about the Kafka round trip
// this file exists for. The gate is stubbed to "not live" and nothing else is.
package archive_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/kafkatest"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/archiver/internal/archive"
)

// idleGate reports the archiver stopped. See the package comment for why this is
// the one stubbed part.
type idleGate struct{}

func (idleGate) ConsumerActive(context.Context, string, string) (bool, error) {
	return false, nil
}

// parkOne writes one message onto dlq.archiver exactly as Archiver.deadLetter
// does: the raw EventFrame body VERBATIM, with pkg/bus's DLQ headers.
func parkOne(t *testing.T, h *harness, eventType, eventID string) {
	t.Helper()
	env := &envelopepb.Envelope{
		EventType:    eventType,
		TenantId:     testTenant,
		EventId:      eventID,
		PartitionKey: eventID,
		EventClass:   envelopepb.EventClass_EVENT_CLASS_FACT,
	}
	body, err := proto.Marshal(&envelopepb.EventFrame{Envelope: env})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if err := h.kafka.Publish(context.Background(), bus.Message{
		Subject: archive.DLQSubject,
		Body:    body,
		Headers: map[string]string{
			bus.HeaderDLQOriginalSubject: eventType,
			bus.HeaderDLQError:           "route: no topic for event_type (the table did not know it yet)",
			bus.HeaderDLQParkedAt:        time.Now().UTC().Format(time.RFC3339Nano),
			bus.HeaderDLQClass:           bus.ClassTerminal,
		},
	}); err != nil {
		t.Fatalf("park %s: %v", eventID, err)
	}
}

// readDestination pulls up to want messages off the destination topic and
// returns their event ids, so the assertion is on what a CONSUMER sees rather
// than on what the drain reported.
func readDestination(t *testing.T, h *harness, want int, timeout time.Duration) []string {
	t.Helper()
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: h.brokers,
		Topic:   h.topic,
		GroupID: "drain-dest-" + h.group,
	})
	defer func() { _ = r.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var ids []string
	for len(ids) < want {
		m, err := r.ReadMessage(ctx)
		if err != nil {
			break // timeout; the caller asserts on what did or did not arrive
		}
		var frame envelopepb.EventFrame
		if err := proto.Unmarshal(m.Value, &frame); err != nil {
			t.Fatalf("the re-archived body is not a valid EventFrame — it was rewritten in flight: %v", err)
		}
		if frame.GetEnvelope() == nil {
			t.Fatal("the re-archived frame has no envelope — the body was rewritten in flight")
		}
		ids = append(ids, frame.GetEnvelope().GetEventId())
	}
	return ids
}

// PARK → FIX THE TABLE → DRAIN → IT LANDS.
//
// The parked event carries an event_type the CURRENT routing table resolves,
// which is the situation the drain exists for: the table has been corrected
// since the event parked, so what could not be routed then can be routed now.
func TestDrain_ReArchivesAParkedEventOntoItsRealTopic(t *testing.T) {
	h := setup(t, 1) // skips unless TEST_KAFKA_BROKERS and TEST_NATS_URL are set

	// The DLQ topic is provisioned out of band, like every other topic here:
	// auto-create is disabled in CI and production, and CreateTopic waits for the
	// partition to be led before returning (#311).
	if err := kafkatest.CreateTopic(context.Background(), h.brokers, archive.DLQSubject, 1); err != nil {
		t.Fatalf("create %s: %v", archive.DLQSubject, err)
	}
	t.Cleanup(func() {
		c, err := kafka.Dial("tcp", h.brokers[0])
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.DeleteTopics(archive.DLQSubject)
	})

	parkOne(t, h, h.subject, "evt-drained-1")

	d := archive.NewDrain(archive.DrainConfig{
		Tenant:   testTenant,
		Subjects: []string{h.domain + ".>"},
		Group:    h.group,
		Kafka:    h.kafka,
		Reader:   h.kafka,
		Gate:     idleGate{},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Write:    true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rep, err := d.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Produced != 1 {
		t.Fatalf("report says Produced=%d, want 1 (parked=%d read=%d routed=%d reparked=%d refusals=%v)",
			rep.Produced, rep.Parked, rep.Read, rep.Routed, rep.Reparked, rep.Refusals)
	}

	// THE ASSERTION THAT MATTERS: a consumer of the destination topic sees it.
	// The report above is the drain's own account of what it did; this is the
	// broker's.
	got := readDestination(t, h, 1, 30*time.Second)
	if len(got) != 1 || got[0] != "evt-drained-1" {
		t.Fatalf("the destination topic %s yielded %v, want [evt-drained-1] — the drain reported a "+
			"produce that no consumer can read, which is the whole failure this test exists to catch",
			h.topic, got)
	}
}

// A DRY RUN TOUCHES NOTHING, against a real broker rather than a fake.
//
// The default is a dry run precisely because every message here is
// ClassTerminal, so the operator must ASK to write. If that default ever leaked
// a produce, the first sign would be duplicate events in the log of record —
// which is exactly the thing an operator runs --dry-run to avoid finding out
// the hard way.
func TestDrain_DryRunPublishesNothingToTheRealTopic(t *testing.T) {
	h := setup(t, 1)

	if err := kafkatest.CreateTopic(context.Background(), h.brokers, archive.DLQSubject, 1); err != nil {
		t.Fatalf("create %s: %v", archive.DLQSubject, err)
	}
	t.Cleanup(func() {
		c, err := kafka.Dial("tcp", h.brokers[0])
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.DeleteTopics(archive.DLQSubject)
	})

	parkOne(t, h, h.subject, "evt-dryrun-1")

	d := archive.NewDrain(archive.DrainConfig{
		Tenant:   testTenant,
		Subjects: []string{h.domain + ".>"},
		Group:    h.group,
		Kafka:    h.kafka,
		Reader:   h.kafka,
		Gate:     idleGate{},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Write deliberately absent: the zero value must be the safe one.
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rep, err := d.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.DryRun {
		t.Fatal("the zero value of Write produced a WRITING run against a real broker")
	}
	if rep.Routed != 1 {
		t.Fatalf("Routed=%d, want 1 — a dry run still reports what it WOULD do", rep.Routed)
	}
	if rep.Produced != 0 {
		t.Fatalf("Produced=%d in a dry run", rep.Produced)
	}

	if got := readDestination(t, h, 1, 10*time.Second); len(got) != 0 {
		t.Fatalf("a DRY RUN put %v on the destination topic %s", got, h.topic)
	}
}
