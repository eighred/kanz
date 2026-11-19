package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

func TestConsumerDedupSeparatesIndependentDispatches(t *testing.T) {
	sub := &oneShotSub{msg: frameKey(t, "shared-fact")}
	c, err := bus.NewConsumer(sub)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := func(_ context.Context, env *envelopepb.Envelope, _ []byte) error {
		if env.IdempotencyKey != "shared-fact" || env.EventId != "shared-fact" {
			t.Fatal("dispatch changed the canonical event identity")
		}
		calls++
		return nil
	}
	for range 2 {
		for _, group := range []string{"position-projection", "tca"} {
			if err := c.Subscribe(t.Context(), "execution.fill", group, h); err != nil {
				t.Fatal(err)
			}
		}
	}
	if calls != 2 {
		t.Fatalf("independently required consumers dispatched %d times, want 2", calls)
	}
	if err := c.Subscribe(t.Context(), "execution.fill.corrected", "tca", h); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal("a distinct subscription subject lost its required dispatch")
	}
}

func TestConsumerDedupScopesCommandKeysByTenant(t *testing.T) {
	sub := &oneShotSub{}
	c, err := bus.NewConsumer(sub)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for i, tenant := range []string{"acme", "globex", "acme"} {
		env := validEnvelope()
		env.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND
		env.EventId = []string{"attempt-1", "attempt-2", "retry-1"}[i]
		env.IdempotencyKey = "client-request"
		env.TenantId = tenant
		sub.msg = bus.Message{Body: frame(t, env, nil)}
		if err := c.Subscribe(t.Context(), "order.submit", "oms", func(_ context.Context, got *envelopepb.Envelope, _ []byte) error {
			if got.IdempotencyKey != "client-request" || got.TenantId != tenant {
				t.Fatal("command identity or tenant was rewritten")
			}
			calls++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("tenant-local command identities dispatched %d times, want 2", calls)
	}
}

func TestConsumerDedupSharesOnlyTheSameReplicaDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	shared := bus.NewRedisDedup(newFakeRedis(), time.Minute)
	reg := prometheus.NewRegistry()
	metrics := bus.NewBusMetrics(reg)
	newConsumer := func() *bus.Consumer {
		c, err := bus.NewConsumer(&oneShotSub{msg: frameKey(t, "concurrent-fact")}, bus.WithDeduper(shared), bus.WithBusMetrics(metrics))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first, second := newConsumer(), newConsumer()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- first.Subscribe(ctx, "execution.fill", "position", func(ctx context.Context, _ *envelopepb.Envelope, _ []byte) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first replica did not claim its dispatch")
	}
	calls := 0
	h := func(context.Context, *envelopepb.Envelope, []byte) error { calls++; return nil }
	if err := second.Subscribe(ctx, "execution.fill", "position", h); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Error("another replica entered the in-flight logical dispatch")
	}
	if err := second.Subscribe(ctx, "execution.fill", "tca", h); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Error("an independent group was suppressed by another group's in-flight claim")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := second.Subscribe(ctx, "execution.fill", "position", h); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("completed same-group dispatch was not shared across replicas")
	}
	const counter = "kanz_bus_dedup_suppressed_total"
	if got, _ := counterByLabels(t, reg, counter, map[string]string{"subject": "execution.fill", "group": "position"}); got != 2 {
		t.Errorf("in-flight and completed duplicate counter = %v, want 2", got)
	}
	if got, _ := counterByLabels(t, reg, counter, map[string]string{"group": "tca"}); got != 0 {
		t.Errorf("independent group duplicate counter = %v, want 0", got)
	}
}

type dispatchDLQ struct{ err error }

func (d dispatchDLQ) Publish(context.Context, bus.Message) error { return d.err }

func TestConsumerDedupLifecycleKeepsOtherDispatchClaims(t *testing.T) {
	for _, mode := range []string{"failure", "cancel", "retained", "parked", "dlq-failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			options := []bus.ConsumerOption{}
			subject := "audit.authority.decision"
			if mode == "cancel" {
				options = append(options, bus.WithRetry(bus.RetryConfig{MaxAttempts: 2, InitialBackoff: time.Second}))
			}
			if mode == "retained" {
				options = append(options, bus.WithRetainedRetries(subject))
			}
			if mode == "parked" || mode == "dlq-failure" {
				var err error
				if mode == "dlq-failure" {
					err = errors.New("disposable DLQ unavailable")
				}
				options = append(options, bus.WithDLQ(dispatchDLQ{err: err}))
			}
			consumer, err := bus.NewConsumer(&oneShotSub{msg: frameKey(t, "lifecycle-fact")}, options...)
			if err != nil {
				t.Fatal(err)
			}
			otherCalls := 0
			other := func(context.Context, *envelopepb.Envelope, []byte) error { otherCalls++; return nil }
			if err := consumer.Subscribe(ctx, subject, "other-required-consumer", other); err != nil {
				t.Fatal(err)
			}
			calls := 0
			failing := func(context.Context, *envelopepb.Envelope, []byte) error {
				calls++
				if mode == "cancel" {
					cancel()
				}
				err := errors.New("disposable refusal")
				if mode == "parked" || mode == "dlq-failure" {
					return bus.Terminal(err)
				}
				return err
			}
			first := consumer.Subscribe(ctx, subject, "failing-consumer", failing)
			if (first == nil) != (mode == "parked") {
				t.Fatalf("unexpected initial %s outcome: %v", mode, first)
			}
			// A fresh delivery context models redelivery after cancellation.
			if err := consumer.Subscribe(t.Context(), subject, "other-required-consumer", other); err != nil {
				t.Fatal(err)
			}
			if otherCalls != 1 {
				t.Fatal("failure handling released another consumer's completed claim")
			}
			if mode != "parked" {
				if err := consumer.Subscribe(t.Context(), subject, "failing-consumer", other); err != nil {
					t.Fatal(err)
				}
				if otherCalls != 2 {
					t.Fatal("failed dispatch retained a claim and suppressed its retry")
				}
			} else if err := consumer.Subscribe(t.Context(), subject, "failing-consumer", failing); err != nil || calls != 1 {
				t.Fatal("terminal parking did not retain only its own dispatch claim")
			}
		})
	}
}

func TestConsumerDedupNamespaceIsUnambiguous(t *testing.T) {
	type identity struct{ subject, group, tenant, key string }
	for _, pair := range [][2]identity{
		{{"ab", "c", "tenant", "key"}, {"a", "bc", "tenant", "key"}},
		{{"a|b", "c", "tenant", "key"}, {"a", "b|c", "tenant", "key"}},
		{{"a\x00b", "c", "tenant", "key"}, {"a", "b\x00c", "tenant", "key"}},
		{{"subject", "group", "ab", "c"}, {"subject", "group", "a", "bc"}},
	} {
		sub := &oneShotSub{}
		c, err := bus.NewConsumer(sub)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		for _, id := range pair {
			env := validEnvelope()
			env.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND
			env.TenantId, env.IdempotencyKey = id.tenant, id.key
			sub.msg.Body = frame(t, env, nil)
			if err := c.Subscribe(t.Context(), id.subject, id.group, func(context.Context, *envelopepb.Envelope, []byte) error {
				calls++
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if calls != 2 {
			t.Fatal("distinct dispatch identities collided at a field boundary")
		}
	}
}
