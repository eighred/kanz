package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// WHAT A HANDLER THAT SAYS NOTHING GETS.
//
// This is the classification default, asserted at the PARKING boundary rather
// than inferred from the drain's behaviour. Getting it backwards does not break
// anything visibly: every message still parks, the drain still runs, and the
// only symptom is that ordinary store failures quietly need an extra operator
// flag to recover from — discovered during an incident, which is the worst
// possible time.
func TestPlainHandlerErrorParksAsTransient(t *testing.T) {
	h := bus.ExportedDLQHeaders(nil, "order.order.submit", 1,
		errors.New("store load: connection refused"), time.Now().UTC())

	if got := h[bus.HeaderDLQClass]; got != bus.ClassTransient {
		t.Errorf("%s = %q, want %q. A handler that says nothing must get the RECOVERABLE "+
			"classification: a Postgres blip is the case this whole path exists for, and "+
			"classifying it terminal puts a live capital-path order behind --include-terminal",
			bus.HeaderDLQClass, got, bus.ClassTransient)
	}
}

func TestTerminalMarkedErrorParksAsTerminal(t *testing.T) {
	inner := errors.New("payload is not a valid SubmitOrder")
	marked := bus.Terminal(inner)

	h := bus.ExportedDLQHeaders(nil, "order.order.submit", 1, marked, time.Now().UTC())
	if got := h[bus.HeaderDLQClass]; got != bus.ClassTerminal {
		t.Errorf("%s = %q, want %q — a handler that explicitly said the BYTES are wrong was "+
			"recorded as retryable, so the drain will send them back to fail identically",
			bus.HeaderDLQClass, got, bus.ClassTerminal)
	}
	// The operator-facing reason must not be buried behind a classification
	// prefix; the class travels as a field.
	if got := h[bus.HeaderDLQError]; got != inner.Error() {
		t.Errorf("%s = %q, want the unchanged cause %q", bus.HeaderDLQError, got, inner.Error())
	}
	if !errors.Is(marked, bus.ErrTerminal) {
		t.Error("errors.Is(marked, ErrTerminal) is false — the sentinel is how a handler signals this")
	}
	if !errors.Is(marked, inner) {
		t.Error("Terminal() lost the wrapped error's identity; a caller can no longer errors.Is " +
			"against its own sentinels")
	}
	if bus.Terminal(nil) != nil {
		t.Error("Terminal(nil) must stay nil, or `return bus.Terminal(f())` turns a success into a failure")
	}
	if bus.IsTerminal(nil) {
		t.Error("IsTerminal(nil) must be false")
	}
}

// A MALFORMED PAYLOAD MUST NOT BE AUTO-REDRIVEN.
//
// These two failures are decided by the bytes: unframeable data and an envelope
// that fails validation will fail identically every time they are dispatched.
// The Consumer marks them Terminal so the drain skips them by default —
// otherwise they inherit the transient default, get redriven on the next drain,
// fail, park, and cycle until the loop bound stops them, burning a real
// dispatch on a capital-path subject each time.
//
// Asserted at the Consumer, not at classOf, because the marking lives at the
// CALL SITE: dropping the Terminal() wrapper in consumer.go leaves classOf
// perfectly correct and every other DLQ test green.
func TestPreDispatchFailuresParkAsTerminal(t *testing.T) {
	t.Run("unframeable body", func(t *testing.T) {
		sub := &oneShotSub{msg: bus.Message{Body: []byte("garbage")}}
		dlq := &captureClient{}
		c, _ := bus.NewConsumer(sub, bus.WithDLQ(dlq))
		if err := c.Subscribe(context.Background(), "x.y.z", "g",
			func(context.Context, *envelopepb.Envelope, []byte) error {
				t.Fatal("handler ran on an unframeable body")
				return nil
			}); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		assertParkedTerminal(t, dlq)
	})

	t.Run("envelope fails validation", func(t *testing.T) {
		inbound := validEnvelope()
		inbound.EventId = "" // makes it invalid
		sub := &oneShotSub{msg: bus.Message{Body: frame(t, inbound, nil)}}
		dlq := &captureClient{}
		c, _ := bus.NewConsumer(sub, bus.WithDLQ(dlq))
		if err := c.Subscribe(context.Background(), "x.y.z", "g",
			func(context.Context, *envelopepb.Envelope, []byte) error {
				t.Fatal("handler ran on an invalid envelope")
				return nil
			}); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		assertParkedTerminal(t, dlq)
	})
}

func assertParkedTerminal(t *testing.T, dlq *captureClient) {
	t.Helper()
	if len(dlq.sent) != 1 {
		t.Fatalf("dlq.sent=%d, want 1", len(dlq.sent))
	}
	got := dlq.sent[0].Headers[bus.HeaderDLQClass]
	if got != bus.ClassTerminal {
		t.Errorf("%s = %q, want %q. These bytes cannot ever be processed, so classifying them "+
			"retryable makes the drain send them back by default — they fail, park, and cycle "+
			"against a capital-path subject until the loop bound stops them",
			bus.HeaderDLQClass, got, bus.ClassTerminal)
	}
	// And the drain must actually act on it.
	parked := bus.Message{
		Subject: "dlq.x.y.z",
		Headers: dlq.sent[0].Headers,
	}
	if _, err := bus.PlanRedrive(parked, bus.RedriveOptions{MinAge: 0}, time.Now().UTC()); err == nil {
		t.Error("the drain planned a redrive of an unprocessable message — the classification " +
			"is recorded but nothing consults it")
	}
}

// The parked-at stamp is what the drain's age gate reads. If it stops being
// written, every redrive refuses (age unverifiable) and the drain is dead
// again — the failure is safe but total, and silent until someone tries.
func TestParkedMessagesCarryAParseableTimestamp(t *testing.T) {
	now := time.Now().UTC()
	h := bus.ExportedDLQHeaders(nil, "order.order.submit", 1, errors.New("boom"), now)

	got, err := time.Parse(time.RFC3339Nano, h[bus.HeaderDLQParkedAt])
	if err != nil {
		t.Fatalf("%s = %q is not RFC3339Nano: %v — the drain cannot establish the message's age "+
			"and refuses every redrive", bus.HeaderDLQParkedAt, h[bus.HeaderDLQParkedAt], err)
	}
	if !got.Equal(now) {
		t.Errorf("%s round-tripped to %s, want %s", bus.HeaderDLQParkedAt, got, now)
	}
}

// dlqHeaders copies inbound wire headers forward. That copy is load-bearing for
// two separate things — broker dedup (Nats-Msg-Id) and the redrive loop bound
// (Kanz-DLQ-Redrives) — and neither is visible at this function's call site.
func TestParkingPreservesInboundHeaders(t *testing.T) {
	in := map[string]string{
		"Nats-Msg-Id":           "idem-1",
		bus.HeaderDLQRedrives:   "2",
		"Kanz-Custom-Something": "keep-me",
	}
	h := bus.ExportedDLQHeaders(in, "order.order.submit", 1, errors.New("boom"), time.Now().UTC())

	for k, want := range in {
		if got := h[k]; got != want {
			t.Errorf("inbound header %s was dropped when parking (got %q, want %q)", k, got, want)
		}
	}
	if h[bus.HeaderDLQRedrives] != "2" {
		t.Error("the redrive count did not survive parking — the loop bound resets on every " +
			"cycle and a permanently-failing message can be redriven forever")
	}
}
