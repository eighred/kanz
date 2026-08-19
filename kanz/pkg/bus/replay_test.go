package bus_test

import (
	"context"
	"strings"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// replaySub satisfies ReplaySubscriber: it hands over a fixed backlog and then
// arms, which is the shape SubscribeReplay has against a real broker.
type replaySub struct {
	msgs    []bus.Message
	subject string
}

func (s *replaySub) Subscribe(ctx context.Context, _, _ string, h bus.Handler) error {
	for _, m := range s.msgs {
		if err := h(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (s *replaySub) SubscribeReplay(ctx context.Context, subject string, h bus.Handler, ready func()) error {
	s.subject = subject
	for _, m := range s.msgs {
		if err := h(ctx, m); err != nil {
			return err
		}
	}
	if ready != nil {
		ready()
	}
	return nil
}

// A TRANSPORT THAT CANNOT REPLAY MUST REFUSE, LOUDLY. Falling back to a durable
// queue-group subscription would fold the log from wherever that durable happened
// to stop — a registry built from the tail of the log, presented as the log.
func TestConsumerSubscribeReplayRefusesATransportWithoutIt(t *testing.T) {
	c, err := bus.NewConsumer(&oneShotSub{})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	err = c.SubscribeReplay(context.Background(), "platform.model.registered",
		func(context.Context, *envelopepb.Envelope, []byte) error { return nil }, nil)
	if err == nil {
		t.Fatal("SubscribeReplay accepted a transport that cannot replay")
	}
	if !strings.Contains(err.Error(), "cannot replay") {
		t.Errorf("error = %q, want it to name the missing capability", err.Error())
	}
}

// The replay path decodes envelopes through the SAME helper the broadcast path
// uses, so tenant/correlation/trace propagation cannot drift between them.
func TestConsumerSubscribeReplayFoldsTheBacklogThenArms(t *testing.T) {
	first := validEnvelope()
	first.EventId = "evt-replay-1"
	first.IdempotencyKey = "evt-replay-1"
	second := validEnvelope()
	second.EventId = "evt-replay-2"
	second.IdempotencyKey = "evt-replay-2"

	sub := &replaySub{msgs: []bus.Message{
		{Body: frame(t, first, nil)},
		{Body: frame(t, second, nil)},
	}}
	c, err := bus.NewConsumer(sub)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	var seen []string
	armed := false
	if err := c.SubscribeReplay(context.Background(), "platform.model.registered",
		func(ctx context.Context, env *envelopepb.Envelope, _ []byte) error {
			seen = append(seen, env.EventId)
			if bus.TenantIDFromContext(ctx) == "" {
				t.Error("tenant was not propagated onto the replay path — the broadcast path does " +
					"it, and a second copy of that logic is how one of them loses it")
			}
			return nil
		}, func() { armed = true }); err != nil {
		t.Fatalf("SubscribeReplay: %v", err)
	}

	if len(seen) != 2 || seen[0] != "evt-replay-1" || seen[1] != "evt-replay-2" {
		t.Fatalf("folded %v — an append log delivered out of order or short applies a promotion "+
			"before the validation it depends on", seen)
	}
	if !armed {
		t.Fatal("ready never fired — without it a caller cannot tell 'folded the log and found " +
			"nothing' from 'has not started', and those are opposite findings")
	}
	if sub.subject != "platform.model.registered" {
		t.Fatalf("subscribed to %q", sub.subject)
	}
}

// DEDUP MUST NOT REACH THE REPLAY PATH. A claim exists to hand an event to exactly
// ONE consumer; a log fold must reach every replica, and every entry of it.
func TestConsumerSubscribeReplayTakesNoDedupClaim(t *testing.T) {
	env := validEnvelope()
	env.EventId = "evt-replay-dup"
	env.IdempotencyKey = "evt-replay-dup"
	body := frame(t, env, nil)

	sub := &replaySub{msgs: []bus.Message{{Body: body}, {Body: body}}}
	c, err := bus.NewConsumer(sub, bus.WithDedupWindow(time.Minute, 128))
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	delivered := 0
	if err := c.SubscribeReplay(context.Background(), "platform.model.registered",
		func(context.Context, *envelopepb.Envelope, []byte) error { delivered++; return nil }, nil); err != nil {
		t.Fatalf("SubscribeReplay: %v", err)
	}
	if delivered != 2 {
		t.Fatalf("handler entered %d times, want 2 — a dedup claim on a log fold silently drops "+
			"an entry, and a registry missing one mutation reports its result with full confidence",
			delivered)
	}
}
