package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

func TestParkedDeliveryFailureNeverMovesOrDeduplicatesUnresolvedEvidence(t *testing.T) {
	sub := &oneShotSub{msg: bus.Message{Subject: "dlq.market.equity.trade", Body: frame(t, validEnvelope(), nil)}}
	dlq := &captureClient{}
	c, err := bus.NewConsumer(sub, bus.WithDLQ(dlq), bus.WithDedupWindow(time.Minute, 100))
	if err != nil {
		t.Fatal(err)
	}
	failed := errors.New("evidence store unavailable")
	calls := 0
	h := func(context.Context, *envelopepb.Envelope, []byte) error {
		calls++
		if calls < 3 {
			return failed
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		err := c.Subscribe(context.Background(), "dlq.market.equity.trade", "evidence", h)
		if i < 2 && !errors.Is(err, failed) {
			t.Fatalf("failed evidence acknowledged: %v", err)
		}
		if i == 2 && err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 || len(dlq.sent) != 0 {
		t.Fatalf("calls=%d recursive publications=%d", calls, len(dlq.sent))
	}
	sub.msg.Body = []byte("invalid frame")
	if err := c.Subscribe(context.Background(), "dlq.market.equity.trade", "evidence", h); err == nil || len(dlq.sent) != 0 {
		t.Fatal("invalid parked bytes were moved or acknowledged")
	}
}
