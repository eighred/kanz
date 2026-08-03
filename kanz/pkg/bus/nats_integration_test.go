package bus_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eighred/kanz/pkg/bus"
)

func TestNATSPublishSubscribe(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}

	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	streamName := "TEST_BUS_" + suffix
	subject := "test.bus." + suffix

	// Out-of-band stream provisioning. The bus client itself doesn't manage
	// streams — in production they're provisioned via kanz/infra/nats/.
	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	if _, err := setupJS.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subject},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		setupConn.Close()
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() {
		_ = setupJS.DeleteStream(ctx, streamName)
		setupConn.Close()
	})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "bus-test"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	received := make(chan bus.Message, 1)
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = client.Subscribe(subCtx, subject, "test-consumer-"+suffix, func(_ context.Context, m bus.Message) error {
			received <- m
			return nil
		})
	}()

	time.Sleep(500 * time.Millisecond) // let the consumer bind

	if err := client.Publish(ctx, bus.Message{
		Subject: subject,
		Key:     []byte("partition-1"),
		Body:    []byte("hello"),
		Headers: map[string]string{"X-Test": "true"},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case m := <-received:
		if string(m.Body) != "hello" {
			t.Errorf("body=%q want hello", m.Body)
		}
		if string(m.Key) != "partition-1" {
			t.Errorf("key=%q want partition-1", m.Key)
		}
		if m.Headers["X-Test"] != "true" {
			t.Errorf("X-Test=%q want true", m.Headers["X-Test"])
		}
		// natsKeyHeader must not leak into Headers — it's surfaced as Key.
		if _, leaked := m.Headers["Kanz-Partition-Key"]; leaked {
			t.Error("Kanz-Partition-Key leaked into Headers")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for message")
	}
}

// TestOneGroupManySubjects pins the defect that made the OMS deaf.
//
// Every service on this platform subscribes to SEVERAL subjects under ONE consumer
// group. The client created the durable as `Durable: group` with
// `FilterSubject: subject`, via CreateOrUpdate — so the second Subscribe SILENTLY
// OVERWROTE the first one's filter, the third overwrote the second, and only the
// LAST subject was ever delivered. Nothing errored. The handlers were registered.
// The consumer existed. The events simply never arrived.
//
// For the OMS the surviving filter was `order.order.amend` — the last subject it
// subscribes to — so on a real spine IT NEVER PROCESSED A SUBMITTED ORDER. The
// whole execution platform was deaf, and every unit test passed, because a fake
// bus has no concept of a durable.
func TestOneGroupManySubjects(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	streamName := "TEST_MULTI_" + suffix
	first := "multi." + suffix + ".first"
	second := "multi." + suffix + ".second"

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	t.Cleanup(setupConn.Close)
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	if _, err := setupJS.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"multi." + suffix + ".>"},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() { _ = setupJS.DeleteStream(context.Background(), streamName) })

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "multi-it"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// ONE group, TWO subjects — the shape of every service on this platform.
	const group = "svc"
	got := make(chan string, 4)
	for _, subj := range []string{first, second} {
		s := subj
		go func() {
			_ = client.Subscribe(ctx, s, group, func(_ context.Context, m bus.Message) error {
				got <- m.Subject
				return nil
			})
		}()
	}
	time.Sleep(time.Second) // let both durables be created and start consuming

	for _, subj := range []string{first, second} {
		if err := client.Publish(ctx, bus.Message{Subject: subj, Body: []byte("x")}); err != nil {
			t.Fatalf("publish %s: %v", subj, err)
		}
	}

	seen := map[string]bool{}
	deadline := time.After(10 * time.Second)
	for len(seen) < 2 {
		select {
		case s := <-got:
			seen[s] = true
		case <-deadline:
			t.Fatalf("only %d of 2 subjects were delivered (%v).\n"+
				"A second subscription under the same consumer group OVERWROTE the first one's filter: "+
				"the handler is registered, the consumer exists, nothing errors, and the events never arrive. "+
				"This is what left the OMS consuming only order.order.amend — no submitted order was ever processed.", len(seen), seen)
		}
	}
}

// TestBroadcastReachesEveryPodAndSurvivesRestart pins EXEC-M12.
//
// The halt gate is constructed CLOSED and is opened only by a ModeChanged FACT. It
// consumed that FACT through Subscribe — a consumer GROUP — which is a work queue.
// Two consequences, both found by deploying the platform to a real cluster:
//
//   - A RESTARTED pod resumed its durable at the last ack and NEVER SAW the
//     operator's resume. It came back halted, answered 423 to every signal, and
//     reported /readyz 200 the whole time. Every rolling update was a SILENT TRADING
//     OUTAGE that only ended when a human noticed and re-armed it by hand.
//   - With more than one pod, the group handed the resume to exactly ONE of them.
//
// A control signal delivered to one pod out of N is not a control signal.
func TestBroadcastReachesEveryPodAndSurvivesRestart(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	streamName := "TEST_BCAST_" + suffix
	subject := "bcast." + suffix + ".mode"

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(setupConn.Close)
	js, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"bcast." + suffix + ".>"},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), streamName) })

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "bcast-it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// THE OPERATOR RESUMES — before any pod is listening. This is the ordering that
	// broke us: the FACT is already on the stream when the pod starts.
	if err := client.Publish(ctx, bus.Message{Subject: subject, Body: []byte("NORMAL")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// TWO pods start AFTERWARDS. Both must learn the current mode.
	got := make(chan string, 4)
	for i := 0; i < 2; i++ {
		pod := fmt.Sprintf("pod-%d", i)
		go func() {
			_ = client.SubscribeBroadcast(ctx, subject, func(_ context.Context, m bus.Message) error {
				got <- pod + ":" + string(m.Body)
				return nil
			})
		}()
	}

	seen := map[string]bool{}
	deadline := time.After(15 * time.Second)
	for len(seen) < 2 {
		select {
		case s := <-got:
			seen[s] = true
		case <-deadline:
			t.Fatalf("only %d of 2 pods learned the mode that was ALREADY on the stream (%v).\n"+
				"A pod that starts after the operator's resume comes back HALTED and stays there — "+
				"reporting ready while refusing every signal — until a human re-arms it by hand.", len(seen), seen)
		}
	}
	if !seen["pod-0:NORMAL"] || !seen["pod-1:NORMAL"] {
		t.Fatalf("both pods must see the resume, got %v", seen)
	}
}

// TestSubscribeDrainsBufferedMessagesOnShutdown pins the bug found against real
// brokers (2 of 4 runs, reproduced before this fix): Subscribe used to end a
// subscription with cc.Stop(), which jetstream.ConsumeContext documents as
// DISCARDING whatever is already in the client's local delivery buffer. JetStream
// marks a message delivered — starting its AckWait timer (60s for these subjects,
// pkg/bus/tuning.go) — at FETCH
// time, not callback time, so a message sitting in that buffer when the process
// stopped was neither acked nor nacked: the server held it for the full AckWait
// while its cleanly-handled siblings moved on. Worse, a sibling NAK'd by a failed
// in-flight handler redelivers on nakDelay's 2s backoff, so it can land AHEAD of the AckWait
// straggler on restart — inverting per-key order for exactly the kind of
// order-dependent fold the archiver and tv-sync's cost-basis calculation do.
//
// To make this deterministic instead of relying on real-broker timing luck, every
// message is published BEFORE the consumer starts (so the whole batch is fetched
// into the local buffer in one pull), and the handler does slow, ctx-aware work —
// modeling a real downstream call like the archiver's Kafka publish. The
// subscribe context is canceled almost immediately, guaranteeing several messages
// are still sitting in the buffer, undelivered to the handler, at cancel time.
//
// The assertion: a FRESH subscriber restarted on the SAME durable must see every
// message that the first run didn't successfully ack, well within a window far
// far shorter than the AckWait — proving nothing was left stranded for the server
// timeout to rediscover.
func TestSubscribeDrainsBufferedMessagesOnShutdown(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}

	const n = 8
	const handlerDelay = 300 * time.Millisecond
	const drainGrace = 500 * time.Millisecond

	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	streamName := "TEST_DRAIN_" + suffix
	subject := "drain." + suffix
	group := "drain-consumer-" + suffix

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	t.Cleanup(setupConn.Close)
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	if _, err := setupJS.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subject},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() { _ = setupJS.DeleteStream(context.Background(), streamName) })

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "drain-it", DrainGrace: drainGrace})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// All N messages land on the stream BEFORE any consumer exists, so the first
	// pull request fetches the whole batch into the client's local buffer at once.
	for i := 0; i < n; i++ {
		if err := client.Publish(ctx, bus.Message{
			Subject: subject,
			Key:     []byte("k"),
			Body:    []byte(fmt.Sprintf("m%d", i)),
		}); err != nil {
			t.Fatalf("publish m%d: %v", i, err)
		}
	}

	var mu sync.Mutex
	firstRunAcked := map[string]bool{}

	subCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- client.Subscribe(subCtx, subject, group, func(hctx context.Context, m bus.Message) error {
			// Real, ctx-aware work — like the archiver's Kafka publish: it either
			// finishes or is cut short by the caller's deadline, it does not
			// ignore ctx and block regardless.
			select {
			case <-time.After(handlerDelay):
				mu.Lock()
				firstRunAcked[string(m.Body)] = true
				mu.Unlock()
				return nil
			case <-hctx.Done():
				return hctx.Err()
			}
		})
	}()

	// Cancel almost immediately: well before even the first 300ms handler call
	// can complete, guaranteeing several of the N messages are still sitting in
	// the local buffer, never having reached the handler, at cancel time.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Subscribe returned error: %v", err)
		}
	case <-time.After(drainGrace + 10*time.Second):
		t.Fatal("Subscribe did not return within the drain grace period + safety margin — shutdown hung")
	}

	mu.Lock()
	ackedByFirstRun := len(firstRunAcked)
	mu.Unlock()
	if ackedByFirstRun == n {
		t.Fatal("every message was acked by the first run — the cancellation raced too late to catch " +
			"any message buffered-but-undelivered; tighten the timing so this test actually exercises the drain path")
	}

	// RESTART: a fresh client, fresh Subscribe call, same durable (same group +
	// subject). Whatever the first run didn't ack must show up here — and quickly,
	// not after the AckWait.
	client2, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "drain-it-restart"})
	if err != nil {
		t.Fatalf("dial (restart): %v", err)
	}
	t.Cleanup(func() { _ = client2.Close() })

	got := make(chan string, n)
	subCtx2, cancel2 := context.WithTimeout(ctx, 15*time.Second)
	defer cancel2()
	go func() {
		_ = client2.Subscribe(subCtx2, subject, group, func(_ context.Context, m bus.Message) error {
			got <- string(m.Body)
			return nil
		})
	}()

	seen := map[string]bool{}
	for k := range firstRunAcked {
		seen[k] = true
	}
	// A window far short of the AckWait (60s on these subjects): if anything was stranded by an
	// abrupt Stop() rather than drained, it will NOT show up in this window.
	deadline := time.After(8 * time.Second)
loop:
	for len(seen) < n {
		select {
		case s := <-got:
			seen[s] = true
		case <-deadline:
			break loop
		}
	}

	if len(seen) != n {
		missing := 0
		for i := 0; i < n; i++ {
			if !seen[fmt.Sprintf("m%d", i)] {
				missing++
			}
		}
		t.Fatalf("only %d of %d messages were observed within 8s of restart (%d missing, presumably stranded "+
			"server-side awaiting the 60s AckWait): %v", len(seen), n, missing, seen)
	}
}
