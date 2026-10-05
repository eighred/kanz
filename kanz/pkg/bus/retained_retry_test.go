package bus_test

import (
	"context"
	"errors"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

func TestRetainedAuthorityRetriesReleaseClaimsAndKeepTerminalDLQ(t *testing.T) {
	const subject = "audit.authority.decision"
	for _, terminal := range []bool{false, true} {
		sub := &oneShotSub{msg: bus.Message{Subject: subject, Body: frame(t, validEnvelope(), []byte("decision"))}}
		dlq := &captureClient{}
		consumer, err := bus.NewConsumer(sub, bus.WithDLQ(dlq), bus.WithRetainedRetries(subject))
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		h := func(context.Context, *envelopepb.Envelope, []byte) error {
			calls++
			if terminal {
				return bus.Terminal(errors.New("invalid evidence"))
			}
			return errors.New("database unavailable")
		}
		first := consumer.Subscribe(t.Context(), subject, "audit", h)
		second := consumer.Subscribe(t.Context(), subject, "audit", h)
		if terminal {
			if first != nil || second != nil || calls != 1 || len(dlq.sent) != 1 {
				t.Fatal("terminal evidence was not parked exactly once")
			}
		} else if first == nil || second == nil || calls != 2 || len(dlq.sent) != 0 {
			t.Fatal("transient evidence was parked or lost behind a dedup claim")
		}
	}
	if _, err := bus.NewConsumer(&oneShotSub{}, bus.WithRetainedRetries("order.>")); err == nil {
		t.Fatal("finite delivery profile accepted for retained retries")
	}
}
