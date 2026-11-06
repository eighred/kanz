package bus

import (
	"testing"
)

func TestRetainedRetryValidatesEffectiveDurableProfile(t *testing.T) {
	retained := RetainedWorkTuning()
	if retained.AckWait != workTuning.AckWait || retained.MaxAckPending != workTuning.MaxAckPending || retained.MaxDeliver != -1 {
		t.Fatal("retained profile changed work budgets")
	}
	client := &NATSClient{cfg: NATSConfig{ConsumerTuning: &retained}}
	if _, err := NewConsumer(client, WithRetainedRetries("order.order.filled")); err != nil {
		t.Fatal(err)
	}
	if client.DurableConsumerTuning("order.order.filled") != retained {
		t.Fatal("validation and subscribe profiles differ")
	}
	finite := workTuning
	client.cfg.ConsumerTuning = &finite
	if _, err := NewConsumer(client, WithRetainedRetries("audit.authority.decision")); err == nil {
		t.Fatal("global unbounded class hid finite client override")
	}
	normal := &NATSClient{}
	for _, subject := range []string{"order.order.filled", "market.test.trade"} {
		if _, err := NewConsumer(normal, WithRetainedRetries(subject)); err == nil {
			t.Fatal("global finite profile changed")
		}
	}
	if _, err := NewConsumer(normal, WithRetainedRetries("audit.authority.decision")); err != nil {
		t.Fatal("existing audit profile regressed", err)
	}
}
