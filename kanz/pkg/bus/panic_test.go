package bus_test

import (
	"context"
	"strings"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

// A handler panic must reach the DEAD-LETTER PATH, not the process exit (#218).
//
// Consumer had a full terminal-failure apparatus for handler ERRORS — bounded
// attempts, DLQ routing with Kanz-DLQ-* headers, dedup Commit/Release — and none
// at all for handler PANICS, which unwound straight past it. Because the delivery
// was never acked, the broker then redelivered the same bytes into the replacement
// pod: one malformed event became estate-wide CrashLoopBackOff, and the DLQ built
// for exactly this case was bypassed.
//
// MaxAttempts is 3 here deliberately. A panic is TERMINAL — re-entering a handler
// that just panicked re-runs the same deterministic defect on the same bytes, over
// whatever partial state the first attempt left behind — so `calls` must be 1. If
// the short-circuit ever regresses this reads 3 and says so.
func TestConsumerParksAPanicInsteadOfDyingOnIt(t *testing.T) {
	inbound := validEnvelope()
	inbound.EventId = "evt-panic"
	inbound.IdempotencyKey = "evt-panic" // FACT: idempotency_key == event_id
	sub := &oneShotSub{msg: bus.Message{Body: frame(t, inbound, nil)}}
	dlq := &captureClient{}
	c, _ := bus.NewConsumer(sub, fastRetry(3), bus.WithDLQ(dlq))

	var calls int
	err := c.Subscribe(context.Background(), "market.equity.trade", "g",
		func(context.Context, *envelopepb.Envelope, []byte) error {
			calls++
			nilMapExternal()["boom"] = "nil map write"
			return nil
		})

	// Reaching this line at all is half the assertion: an unrecovered panic would
	// have taken the test binary down with it.
	if err != nil {
		t.Errorf("Subscribe returned %v — a panicked delivery should be parked and acked, "+
			"not surfaced to the broker for redelivery", err)
	}
	if calls != 1 {
		t.Errorf("handler entered %d times, want 1 — a panic is terminal; retrying it re-runs "+
			"the same defect on the same bytes over the first attempt's partial state", calls)
	}
	if len(dlq.sent) != 1 {
		t.Fatalf("dlq.sent=%d want 1 — the panic never reached the dead-letter path that "+
			"exists for messages the handler cannot process", len(dlq.sent))
	}

	dq := dlq.sent[0]
	if dq.Subject != "dlq.market.equity.trade" {
		t.Errorf("DLQ subject=%q want dlq.market.equity.trade", dq.Subject)
	}
	if got := dq.Headers["Kanz-DLQ-Error"]; !strings.Contains(got, "handler panicked") {
		t.Errorf("Kanz-DLQ-Error=%q, want it to name the panic — an operator reading the "+
			"parked message has to learn why it is there", got)
	}
	if got := dq.Headers["Kanz-DLQ-Attempts"]; got != "1" {
		t.Errorf("Kanz-DLQ-Attempts=%q want \"1\"; a higher number means the terminal "+
			"short-circuit stopped working and panics are being retried", got)
	}
}

// oneShotBroadcastSub is oneShotSub that also satisfies BroadcastSubscriber, so the
// broadcast path is reachable without a real transport.
type oneShotBroadcastSub struct {
	msg bus.Message
}

func (s *oneShotBroadcastSub) Subscribe(ctx context.Context, _, _ string, h bus.Handler) error {
	return h(ctx, s.msg)
}

func (s *oneShotBroadcastSub) SubscribeBroadcast(ctx context.Context, _ string, h bus.Handler) error {
	return h(ctx, s.msg)
}

func (s *oneShotBroadcastSub) SubscribeBroadcastReady(ctx context.Context, _ string, h bus.Handler, ready func()) error {
	err := h(ctx, s.msg)
	if ready != nil {
		ready()
	}
	return err
}

// The broadcast path has no DLQ by design — an unreadable control message must not
// be quietly parked. A panic there must still not kill the process: it surfaces as
// an error so the message is nacked and the process stays in whatever state it was
// already in. For the halt gate that state is CLOSED, and "I could not read the
// brake signal" must resolve to neither "keep trading" nor "the pod is gone".
func TestConsumerBroadcastSurvivesAPanickingHandler(t *testing.T) {
	inbound := validEnvelope()
	inbound.EventId = "evt-panic-bcast"
	inbound.IdempotencyKey = "evt-panic-bcast"
	sub := &oneShotBroadcastSub{msg: bus.Message{Body: frame(t, inbound, nil)}}
	c, _ := bus.NewConsumer(sub)

	err := c.SubscribeBroadcast(context.Background(), "risk.halt.changed",
		func(context.Context, *envelopepb.Envelope, []byte) error {
			panic("control-plane handler exploded")
		})

	if err == nil {
		t.Fatal("SubscribeBroadcast returned nil after its handler panicked — a control " +
			"message that could not be read must surface, so the delivery is nacked and " +
			"the process holds its current state")
	}
	if !strings.Contains(err.Error(), "handler panicked") {
		t.Errorf("error = %q, want it to name the panic", err.Error())
	}
}

// See nilMap in panic_internal_test.go — same reason, different test package.
func nilMapExternal() map[string]string { return nil }
