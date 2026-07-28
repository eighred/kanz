package archive_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/archiver/internal/archive"
)

const testTenant = "acme"

// harness is one isolated NATS stream + Kafka topic pair for a single test.
type harness struct {
	brokers []string
	natsURL string
	domain  string // synthetic, e.g. "archtest1730000000"
	subject string // "<domain>.order.submitted"
	topic   string // "acme.<domain>.order"
	nats    *bus.NATSClient
	kafka   *bus.KafkaClient
	group   string
}

func setup(t *testing.T, partitions int) *harness {
	t.Helper()
	rawBrokers := os.Getenv("TEST_KAFKA_BROKERS")
	natsURL := os.Getenv("TEST_NATS_URL")
	if rawBrokers == "" || natsURL == "" {
		t.Skip("TEST_KAFKA_BROKERS / TEST_NATS_URL not set")
	}
	brokers := strings.Split(rawBrokers, ",")

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	h := &harness{
		brokers: brokers,
		natsURL: natsURL,
		domain:  "archtest" + suffix,
		group:   "archiver-" + suffix,
	}
	h.subject = h.domain + ".order.submitted"
	h.topic = testTenant + "." + h.domain + ".order"

	ctx := context.Background()

	// --- NATS stream (the live spine under test) ---
	conn, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "ARCHTEST" + suffix,
		Subjects:  []string{h.domain + ".>"},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		conn.Close()
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() {
		_ = js.DeleteStream(context.Background(), "ARCHTEST"+suffix)
		conn.Close()
	})

	// --- Kafka topic. Created out-of-band: auto-create is DISABLED, in production
	// and in CI. Several partitions, or the ordering assertion is vacuous.
	kconn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatalf("kafka dial: %v", err)
	}
	if err := kconn.CreateTopics(kafka.TopicConfig{
		Topic:             h.topic,
		NumPartitions:     partitions,
		ReplicationFactor: 1,
	}); err != nil {
		kconn.Close()
		t.Fatalf("create topic: %v", err)
	}
	kconn.Close()
	t.Cleanup(func() {
		c, err := kafka.Dial("tcp", brokers[0])
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.DeleteTopics(h.topic)
	})

	h.nats, err = bus.DialNATS(ctx, bus.NATSConfig{URL: natsURL, Name: "archiver-test"})
	if err != nil {
		t.Fatalf("DialNATS: %v", err)
	}
	t.Cleanup(func() { _ = h.nats.Close() })

	h.kafka, err = bus.DialKafka(bus.KafkaConfig{Brokers: brokers, ClientID: "archiver-test"})
	if err != nil {
		t.Fatalf("DialKafka: %v", err)
	}
	t.Cleanup(func() { _ = h.kafka.Close() })

	return h
}

// publish puts one envelope on the NATS spine.
//
// THE BODY IS AN EventFrame, NOT A BARE Envelope. bus.Unframe unmarshals into
// envelopepb.EventFrame{Envelope, Payload}, and this is a trap: EventFrame's field 1
// and Envelope's field 1 are BOTH wire-type 2, so a bare marshaled Envelope does not
// fail to decode — it silently yields a frame holding a garbage envelope. Marshal the
// frame, exactly as bus.Producer does on the real publish path.
func (h *harness) publish(t *testing.T, eventID, partitionKey string) {
	t.Helper()
	e := &envelopepb.Envelope{
		EventType:    h.subject,
		TenantId:     testTenant,
		EventId:      eventID,
		PartitionKey: partitionKey,
		EventClass:   envelopepb.EventClass_EVENT_CLASS_FACT,
	}
	body, err := proto.Marshal(&envelopepb.EventFrame{Envelope: e})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := h.nats.Publish(context.Background(), bus.Message{Subject: h.subject, Body: body}); err != nil {
		t.Fatalf("publish %s: %v", eventID, err)
	}
}

// drain reads up to want messages off the Kafka topic, returning the event_ids per
// partition key, in arrival order.
func (h *harness) drain(t *testing.T, want int, timeout time.Duration) map[string][]string {
	t.Helper()
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: h.brokers,
		Topic:   h.topic,
		GroupID: "drain-" + h.group,
	})
	defer r.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out := map[string][]string{}
	for n := 0; n < want; n++ {
		m, err := r.ReadMessage(ctx)
		if err != nil {
			break // timeout: caller asserts on what did (or did not) arrive
		}
		// The archived body is the EventFrame, byte-for-byte as it rode the spine.
		var frame envelopepb.EventFrame
		if err := proto.Unmarshal(m.Value, &frame); err != nil {
			t.Fatalf("the archived body is not a valid EventFrame — it was rewritten in flight: %v", err)
		}
		if frame.GetEnvelope() == nil {
			t.Fatal("the archived frame has no envelope — the body was rewritten in flight")
		}
		key := string(m.Key)
		out[key] = append(out[key], frame.GetEnvelope().GetEventId())
	}
	return out
}

func (h *harness) archiver(k archive.Publisher) *archive.Archiver {
	return archive.New(archive.Config{
		Tenant:   testTenant,
		Group:    h.group,
		Subjects: []string{h.domain + ".>"},
		Kafka:    k,
		NATS:     h.nats,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// run starts the archiver and stops it after d.
func (h *harness) run(t *testing.T, a *archive.Archiver, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}
}

// deadKafka fails every produce — a Kafka outage, without stopping the container.
// calls is read from the test goroutine after Run returns. Subscribe's Drain-based
// shutdown ordinarily waits for buffered handler invocations to finish before
// returning, but the hard-stop fallback path (a handler that ignores its drain
// deadline) would not — so the counter keeps its own lock rather than relying on
// happens-before from context cancellation.
type deadKafka struct {
	mu    sync.Mutex
	calls int
}

func (d *deadKafka) Publish(context.Context, bus.Message) error {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return errors.New("kafka is down")
}

func (d *deadKafka) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// ---------------------------------------------------------------------------
// 1. THE PAYOFF TEST. Before this service existed, NOTHING had ever written an
// event to Kafka. This is the proof that it does — and that per-key order survives
// partitioning, which is what makes the log replayable into the same book.
// ---------------------------------------------------------------------------
func TestArchiver_LandsEveryEventInOrder(t *testing.T) {
	h := setup(t, 4) // several partitions, or "in order" proves nothing

	// Two entities, interleaved. Each key's events must come back in publish order.
	h.publish(t, "a1", "portfolio-A")
	h.publish(t, "b1", "portfolio-B")
	h.publish(t, "a2", "portfolio-A")
	h.publish(t, "b2", "portfolio-B")
	h.publish(t, "a3", "portfolio-A")

	h.run(t, h.archiver(h.kafka), 10*time.Second)

	got := h.drain(t, 5, 15*time.Second)

	wantA := []string{"a1", "a2", "a3"}
	wantB := []string{"b1", "b2"}
	if !equal(got["portfolio-A"], wantA) {
		t.Errorf("portfolio-A = %v, want %v (per-key order was not preserved)", got["portfolio-A"], wantA)
	}
	if !equal(got["portfolio-B"], wantB) {
		t.Errorf("portfolio-B = %v, want %v (per-key order was not preserved)", got["portfolio-B"], wantB)
	}
}

// ---------------------------------------------------------------------------
// 2. KAFKA OUTAGE — NO LOSS, CLEAN CATCH-UP. A gap is deferred work, not loss:
// NATS holds the events while Kafka is down, and the archiver drains them when it
// comes back. This is why replicas:1 + Recreate is SAFE here and was not for
// webhook-ingest.
// ---------------------------------------------------------------------------
func TestArchiver_KafkaOutageLosesNothing(t *testing.T) {
	h := setup(t, 4)

	h.publish(t, "e1", "portfolio-A")
	h.publish(t, "e2", "portfolio-A")
	h.publish(t, "e3", "portfolio-A")

	// --- Kafka is down. Every produce fails, so every message is NACKed. ---
	dead := &deadKafka{}
	h.run(t, h.archiver(dead), 5*time.Second)

	if dead.callCount() == 0 {
		t.Fatal("the archiver never tried to produce — the test proves nothing")
	}
	if landed := h.drain(t, 3, 2*time.Second); len(landed) != 0 {
		t.Fatalf("events landed in Kafka while it was down: %v", landed)
	}

	// --- Kafka is back. The SAME durable group must redeliver everything. ---
	//
	// bus.NATSClient.Subscribe now ends a subscription with Drain(), not Stop()
	// (fixed under DATA-M1 Task 5c): a message already in the client's local
	// buffer when ctx is canceled is now handed to the handler — through a
	// bounded, freshly-deadlined context — before shutdown completes, instead of
	// being silently abandoned to redeliver only after the consumer's AckWait
	// (30s default) elapses. That eliminates the LOSS-BY-DELAY failure mode Task 5
	// found: this run no longer needs a 35s window to clear AckWait, because
	// nothing is left "delivered, awaiting ack" past the drain window.
	//
	// It does NOT, however, restore strict per-key order across the outage.
	// Measured directly (kanz/pkg/bus/nats_integration_test.go,
	// TestSubscribeDrainsBufferedMessagesOnShutdown): the Stop-vs-Drain fix
	// reliably prevents STRANDING at a single shutdown boundary — 0/40 failures
	// with Drain vs 14/20 with Stop. But THIS test's failure mode is different: a
	// sustained outage NAKs the same 3 messages thousands of times a second for
	// the full 5s the outage runs (every produce fails instantly, so every
	// delivery is NAK'd on the spot and immediately redeliverable — NAK bypasses
	// AckWait by design). Re-running this test 7 times after the Drain fix landed
	// showed strict order holding only twice; the other five produced scrambled
	// orders such as [e3 e1 e2] and [e2 e3 e1] — never a loss, always a reorder.
	// That is JetStream's own redelivery scheduling among several concurrently
	// NAK-eligible messages, not a client shutdown defect: Stop vs Drain governs
	// what happens to ONE buffer at ONE shutdown instant, not the relative order
	// in which the SERVER re-offers multiple already-repeatedly-NAK'd messages to
	// a new consumer. No further weakening or forcing was applied here — per
	// instruction, this was surfaced as a finding rather than papered over.
	//
	// The invariant this test guards is the one in its name: nothing is LOST.
	// Order across a live, uninterrupted run — the actual replay-integrity
	// property — is TestArchiver_LandsEveryEventInOrder's job, and it asserts
	// order strictly; that test is unaffected.
	h.run(t, h.archiver(h.kafka), 10*time.Second)

	got := h.drain(t, 3, 10*time.Second)
	gotSet := map[string]bool{}
	for _, id := range got["portfolio-A"] {
		gotSet[id] = true
	}
	for _, want := range []string{"e1", "e2", "e3"} {
		if !gotSet[want] {
			t.Fatalf("after the outage, Kafka holds %v, missing %q — an event was LOST across the outage",
				got["portfolio-A"], want)
		}
	}
	if n := len(got["portfolio-A"]); n != 3 {
		t.Fatalf("after the outage, Kafka holds %v (%d events) — want exactly 3, no duplicates unexplained",
			got["portfolio-A"], n)
	}
}

// ---------------------------------------------------------------------------
// 3. THE RED TWIN, KEPT EXECUTABLE. Ack-before-produce is the bug this service
// exists to prevent, and a test that cannot reproduce it is not guarding anything.
// The SAME outage, against a handler that acks whatever it failed to produce,
// LOSES the events permanently — NATS considers them delivered and never sends
// them again.
// ---------------------------------------------------------------------------
func TestArchiver_AckBeforeProduceLosesEvents_RedTwin(t *testing.T) {
	h := setup(t, 4)

	h.publish(t, "e1", "portfolio-A")
	h.publish(t, "e2", "portfolio-A")

	// The broken archiver: produce, ignore the error, ACK anyway.
	dead := &deadKafka{}
	broken := func(ctx context.Context, msg bus.Message) error {
		_ = dead.Publish(ctx, msg) // the error is swallowed — this is the bug
		return nil                 // ACK. The event is now gone from NATS forever.
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := h.nats.Subscribe(ctx, h.domain+".>", h.group, broken); err != nil {
		cancel()
		t.Fatalf("subscribe: %v", err)
	}
	<-ctx.Done()
	cancel()

	// Kafka comes back, and a CORRECT archiver runs on the same durable group.
	h.run(t, h.archiver(h.kafka), 8*time.Second)

	got := h.drain(t, 2, 5*time.Second)
	if len(got["portfolio-A"]) != 0 {
		t.Fatalf("the red twin did NOT lose the events (Kafka holds %v) — either the ack semantics "+
			"changed or this test no longer reproduces the bug it guards against", got["portfolio-A"])
	}
	t.Log("red twin confirmed: ack-before-produce lost every event across the outage")
}

// ---------------------------------------------------------------------------
// 4. FAIL CLOSED ON AN UNPROVISIONED TENANT. Kafka auto-create is disabled, so a
// tenant whose topics were never provisioned has nowhere to land. That must NACK —
// never cross-file into the shared un-prefixed topic, which would breach the Kafka
// PREFIXED-ACL isolation boundary (MT-01c).
// ---------------------------------------------------------------------------
func TestArchiver_UnprovisionedTenantNacksAndDoesNotCrossFile(t *testing.T) {
	h := setup(t, 1)

	// An archiver serving a tenant whose topic ("ghost.<domain>.order") was never created.
	a := archive.New(archive.Config{
		Tenant:   "ghost",
		Group:    h.group,
		Subjects: []string{h.domain + ".>"},
		Kafka:    h.kafka,
		NATS:     h.nats,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	e := &envelopepb.Envelope{
		EventType:    h.subject,
		TenantId:     "ghost",
		EventId:      "g1",
		PartitionKey: "portfolio-A",
		EventClass:   envelopepb.EventClass_EVENT_CLASS_FACT,
	}
	body, err := proto.Marshal(&envelopepb.EventFrame{Envelope: e}) // frame, not bare envelope
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := a.Handle(context.Background(), bus.Message{Subject: h.subject, Body: body}); err == nil {
		t.Fatal("an event for an unprovisioned tenant was ACCEPTED — Kafka auto-create must be disabled " +
			"on the test broker, or the archiver is not failing closed")
	}

	// And it must not have been cross-filed into the un-prefixed topic.
	if landed := h.drain(t, 1, 2*time.Second); len(landed) != 0 {
		t.Fatalf("the event was cross-filed: %v", landed)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
