package bus_test

import (
	"context"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

type readyFixture struct {
	oneShotSub
	readyCalls int
}

func (s *readyFixture) SubscribeReady(ctx context.Context, subject, group string, h bus.Handler, ready func()) error {
	if err := s.Subscribe(ctx, subject, group, h); err != nil {
		return err
	}
	s.readyCalls++
	ready()
	return nil
}
func TestConsumerDurableReadyPreservesValidationAndDedup(t *testing.T) {
	env := validEnvelope()
	sub := &readyFixture{oneShotSub: oneShotSub{msg: bus.Message{Subject: "x", Body: frame(t, env, []byte("p"))}}}
	c, err := bus.NewConsumer(sub)
	if err != nil {
		t.Fatal(err)
	}
	handled, armed := 0, 0
	h := func(context.Context, *envelopepb.Envelope, []byte) error { handled++; return nil }
	for range 2 {
		if err := c.SubscribeReady(context.Background(), "x", "fixture", h, func() { armed++ }); err != nil {
			t.Fatal(err)
		}
	}
	if handled != 1 || armed != 2 {
		t.Fatalf("handled=%d armed=%d", handled, armed)
	}
	sub.msg.Body = []byte("invalid")
	if err := c.SubscribeReady(context.Background(), "x", "fixture", h, func() { armed++ }); err == nil {
		t.Fatal("invalid frame armed")
	}
	if armed != 2 {
		t.Fatal("invalid frame reached readiness")
	}
}
func TestConsumerDurableReadyRefusesUnsupportedTransport(t *testing.T) {
	c, err := bus.NewConsumer(&oneShotSub{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SubscribeReady(context.Background(), "x", "fixture", nil, func() { t.Fatal("armed") }); err == nil {
		t.Fatal("unsupported readiness accepted")
	}
	if err := c.SubscribeReady(context.Background(), "x", "fixture", nil, nil); err == nil {
		t.Fatal("nil readiness accepted")
	}
}
