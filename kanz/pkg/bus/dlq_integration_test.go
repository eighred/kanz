// The DLQ wiring against a REAL spine.
//
// Every other DLQ test in this package injects a fake Publisher that records
// what it was handed and cannot refuse it. That is enough to prove the routing
// LOGIC and nothing about whether the message can actually land: bus.Publish is
// a JetStream publish, so a dlq.<subject> that no stream carries is a hard
// publish failure, and publishDLQ surfaces that error — which puts the event
// straight back on the broker for the redelivery loop the DLQ exists to end.
// A consumer wired with WithDLQ and no reachable DLQ destination is therefore
// WORSE than one with no DLQ at all, and no fake publisher can show it.
//
//	docker run -d --name kanz-nats -p 4222:4222 nats:2 -js
//	TEST_NATS_URL=nats://localhost:4222 go test ./pkg/bus/...
package bus_test

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/bustest"
	"github.com/kanz-eng/kanz/pkg/bus"
)

func TestIntegration_ConsumerWithDLQParksAFailedEventOnTheWire(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the DLQ wiring over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const probeSubject = "kanztest.dlqprobe"
	const dlqProbeSubject = "dlq." + probeSubject

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	// t.Cleanup, not defer — cleanups run last-registered-first, so registering
	// the close FIRST makes it run LAST, after the stream deletions below.
	t.Cleanup(setupConn.Close)
	js, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}

	// ONE SUBJECT PER CALL, deliberately. EnsureSubjects is all-or-nothing on a
	// single subject, so on a bootstrapped spine the DLQ half binds to the REAL
	// `dlq.>` stream from infra/nats/bootstrap-job.yaml while the probe half gets
	// a scratch stream. Passing both to one call would trip its half-bound
	// refusal on every real topology, which is correct behaviour there and simply
	// the wrong question here.
	bustest.EnsureSubjects(t, ctx, js, "KANZTEST_DLQPROBE", []string{probeSubject})
	bustest.EnsureSubjects(t, ctx, js, "KANZTEST_DLQPROBE_DLQ", []string{dlqProbeSubject})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "dlq-probe"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	prod, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "test/dlq-probe", ProducerVersion: "v1", Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	// THE COMPOSITION UNDER TEST — the same shape as all 13 call sites: the
	// NATSClient is handed to WithDLQ as the Publisher.
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	var dispatches atomic.Int32
	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	go func() {
		_ = consumer.Subscribe(subCtx, probeSubject, "dlq-probe-group",
			func(context.Context, *envelopepb.Envelope, []byte) error {
				dispatches.Add(1)
				return errors.New("probe handler always fails")
			})
	}()

	et := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if err := prod.Publish(ctx, bus.Event{
		Subject:          probeSubject,
		EventType:        "kanztest.dlqprobe",
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "market",
		EventTime:        et,
		PartitionKey:     "probe",
		PayloadSchemaRef: "kanztest.dlqprobe.v1:1",
		Payload:          timestamppb.New(et),
	}); err != nil {
		t.Fatalf("publish probe: %v", err)
	}

	// Read it back off the DLQ. This is the assertion that could not be made with
	// a fake publisher: the message is on the wire, in a stream, retrievable by
	// something that is not the process that failed it.
	dlqStream, err := js.StreamNameBySubject(ctx, dlqProbeSubject)
	if err != nil {
		t.Fatalf("no stream carries %q: %v", dlqProbeSubject, err)
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, dlqStream, jetstream.ConsumerConfig{
		FilterSubject: dlqProbeSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("dlq consumer: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), dlqStream, cons.CachedInfo().Name) })

	msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(15*time.Second))
	if err != nil {
		t.Fatalf("fetch from dlq: %v", err)
	}
	var got jetstream.Msg
	for m := range msgs.Messages() {
		got = m
		_ = m.Ack()
	}
	if err := msgs.Error(); err != nil {
		t.Fatalf("dlq fetch error: %v", err)
	}
	if got == nil {
		t.Fatalf("nothing arrived on %s within 15s — the handler failed %d time(s) and the event "+
			"went nowhere. That is the swallow this wiring exists to end: no DLQ record, and the "+
			"broker holding a redelivery nobody will resolve",
			dlqProbeSubject, dispatches.Load())
	}

	// NON-VACUITY: the handler really ran and really failed. Without this, a
	// message arriving for any other reason (a stale stream, a mis-set filter)
	// would read as proof of DLQ routing.
	if n := dispatches.Load(); n == 0 {
		t.Fatal("handler never ran, so whatever arrived on the DLQ subject did not get there by " +
			"being failed — this test would pass without the routing under test working at all")
	}

	// The failure metadata is the point of the DLQ: an operator needs to know
	// which subject it came from and why it stopped, from the message alone.
	if orig := got.Headers().Get("Kanz-DLQ-Original-Subject"); orig != probeSubject {
		t.Errorf("Kanz-DLQ-Original-Subject = %q, want %q", orig, probeSubject)
	}
	if reason := got.Headers().Get("Kanz-DLQ-Error"); reason == "" {
		t.Error("Kanz-DLQ-Error is empty — the parked message does not say why it failed")
	}
	if attempts := got.Headers().Get("Kanz-DLQ-Attempts"); attempts != "1" {
		t.Errorf("Kanz-DLQ-Attempts = %q, want \"1\" — MaxAttempts is deliberately 1 "+
			"estate-wide (see test/arch/bus_dlq_test.go); a different number here means "+
			"retry was wired somewhere, and re-entering a handler that acks on sight "+
			"swallows the failure before it can reach this DLQ at all", attempts)
	}

	// And the ORIGINAL delivery is settled: the point of parking it is that the
	// broker stops asking. A consumer still redelivering after a DLQ publish
	// would be doing both, which is the loop the DLQ replaces.
	if n := dispatches.Load(); n > 1 {
		t.Errorf("handler ran %d times — the event was parked in the DLQ AND redelivered; "+
			"the original delivery is not being settled", n)
	}
}
