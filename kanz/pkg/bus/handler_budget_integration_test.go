package bus_test

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eighred/kanz/pkg/bus"
)

// A HANDLER IS CANCELLED BEFORE THE BROKER REDELIVERS ITS MESSAGE (#836).
//
// # Why this runs against a real broker
//
// The property is a race between two clocks that belong to different processes:
// the deadline this library puts on the handler's context, and JetStream's own
// AckWait timer, which starts at FETCH time on the server. A fake subscriber has
// neither — it would assert that a context we constructed expires when we said,
// which is a tautology, and it is precisely the half that cannot go wrong.
//
// What can go wrong is the ordering: if the budget were derived from the wrong
// number, or applied after the message was already in flight, the handler would
// still be running when the server re-offers the message. That is a second
// concurrent dispatch of one event — on this platform, a second trade — and only
// a real server's timer can show it does not happen.
//
// AckWait is 2s here so the whole race fits in a test. The budget is a fraction
// of AckWait (bus.HandlerBudget), so the ratio under test is the production one
// whatever the absolute numbers are.
func TestHandlerIsCancelledBeforeTheBrokerRedelivers(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}
	const ackWait = 2 * time.Second
	budget := bus.HandlerBudget(ackWait)
	if budget <= 0 || budget >= ackWait {
		t.Fatalf("HandlerBudget(%s) = %s — it must be positive and strictly inside AckWait, or "+
			"this test is asserting an impossible ordering", ackWait, budget)
	}

	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	streamName := "TEST_BUDGET_" + suffix
	subject := "test.budget." + suffix
	group := "budget-" + suffix

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	if _, err := setupJS.CreateStream(ctx, jetstream.StreamConfig{
		Name: streamName, Subjects: []string{subject},
		Storage: jetstream.MemoryStorage, Retention: jetstream.LimitsPolicy,
	}); err != nil {
		setupConn.Close()
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() {
		_ = setupJS.DeleteStream(ctx, streamName)
		setupConn.Close()
	})

	cli, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: url, Name: "budget-cons",
		ConsumerTuning: &bus.ConsumerTuning{AckWait: ackWait, MaxDeliver: 5, MaxAckPending: 1},
	})
	if err != nil {
		t.Fatalf("dial consumer: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	var (
		mu          sync.Mutex
		deliveries  int
		live        int
		maxLive     int
		hadDeadline bool
		cancelledAt time.Duration
	)
	done := make(chan struct{}, 4)

	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = cli.Subscribe(subCtx, subject, group, func(hctx context.Context, _ bus.Message) error {
			started := time.Now()
			mu.Lock()
			deliveries++
			live++
			if live > maxLive {
				maxLive = live
			}
			_, ok := hctx.Deadline()
			hadDeadline = hadDeadline || ok
			mu.Unlock()

			// Block until the budget cuts us off — the shape of a handler waiting
			// on a lock or a venue that will not answer. The hard cap is a test
			// safety net: without the fix this returns on the cap, and the
			// assertions below fail on the elapsed time rather than hanging.
			select {
			case <-hctx.Done():
			case <-time.After(4 * ackWait):
			}

			mu.Lock()
			live--
			if cancelledAt == 0 {
				cancelledAt = time.Since(started)
			}
			mu.Unlock()
			done <- struct{}{}
			return hctx.Err()
		})
	}()

	pub, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "budget-pub"})
	if err != nil {
		t.Fatalf("dial publisher: %v", err)
	}
	t.Cleanup(func() { _ = pub.Close() })

	if err := pub.Publish(ctx, bus.Message{Subject: subject, Body: []byte("probe")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-done:
	case <-time.After(10 * ackWait):
		t.Fatal("the handler never returned — it was neither cancelled nor capped")
	}

	mu.Lock()
	gotDeadline, elapsed := hadDeadline, cancelledAt
	mu.Unlock()

	// 1. THE CONTEXT CARRIES A DEADLINE AT ALL. Without one the handler is bounded
	//    by nothing and every assertion below is about a coincidence.
	if !gotDeadline {
		t.Fatal("the handler's context carries no deadline — bus.Subscribe is handing out an " +
			"unbounded context, so a slow handler runs past AckWait and the broker redelivers " +
			"a message whose first copy is still working")
	}

	// 2. IT EXPIRED STRICTLY INSIDE AckWait. This is the whole ordering: the
	//    handler must be finished before the server decides the pod died.
	if elapsed >= ackWait {
		t.Fatalf("the handler ran %s against an AckWait of %s — it is still executing when the "+
			"broker re-offers the message, which is a second concurrent dispatch of one event",
			elapsed.Round(time.Millisecond), ackWait)
	}

	// 3. AND IT IS THE BUDGET DOING IT, not some other timeout that happens to be
	//    shorter. Generous bounds either side: this is proving which clock fired,
	//    not measuring a scheduler.
	if elapsed < budget/2 {
		t.Errorf("the handler was cut off after %s, well before its %s budget — something other "+
			"than the delivery budget is cancelling it, and the bound under test is not the one "+
			"in force", elapsed.Round(time.Millisecond), budget)
	}

	// 4. NO TWO COPIES RAN AT ONCE. The reason the budget exists.
	if maxLive > 1 {
		t.Errorf("%d handler invocations were in flight at once for one published message — the "+
			"redelivery overtook the first copy, which is the defect this bound removes", maxLive)
	}
}

// A NON-POSITIVE AckWait DOES NOT CANCEL EVERY DELIVERY ON ARRIVAL.
//
// "Nothing configured" and "checked, and fine" must never look the same — but
// they must not look like "expire immediately" either. ConsumerTuning.validate
// refuses a zero AckWait at construction, so this is the belt behind that brace:
// if a budget is ever derived from an unset number, the safe reading is "do not
// bound", not "bound to zero and DLQ the entire estate".
func TestHandlerBudgetOfAnUnsetAckWaitIsNotZero(t *testing.T) {
	for _, ackWait := range []time.Duration{0, -time.Second} {
		if got := bus.HandlerBudget(ackWait); got > 0 {
			t.Errorf("HandlerBudget(%s) = %s, want a non-positive budget that reads as "+
				"'do not bound'", ackWait, got)
		}
	}
	if got := bus.HandlerBudget(60 * time.Second); got != 45*time.Second {
		t.Errorf("HandlerBudget(60s) = %s, want 45s — the work class's budget is what #801 "+
			"arrived at independently, and the OMS's own compile-time assertion is bound to it",
			got)
	}
}
