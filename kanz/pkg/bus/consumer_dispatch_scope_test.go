package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

func TestConsumerDispatchScopeSeparatesIndependentWork(t *testing.T) {
	for _, backend := range []string{"memory", "shared-redis"} {
		for _, changed := range []string{"subject", "group", "tenant", "command-key", "framing"} {
			t.Run(backend+"/"+changed, func(t *testing.T) {
				first, second := validEnvelope(), validEnvelope()
				first.EventClass, second.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND, envelopepb.EventClass_EVENT_CLASS_COMMAND
				first.IdempotencyKey, second.IdempotencyKey = "caller-key", "caller-key"
				subjects, groups := [2]string{"subject", "subject"}, [2]string{"group", "group"}
				switch changed {
				case "subject":
					subjects[1] = "other-subject"
				case "group":
					groups[1] = "other-group"
				case "tenant":
					second.TenantId = "other-tenant"
				case "command-key":
					second.IdempotencyKey = "other-caller-key"
				case "framing":
					// Equal concatenations must not alias distinct component tuples.
					subjects, groups = [2]string{"a:b", "a"}, [2]string{"c", "b:c"}
				}
				sub := &oneShotSub{}
				var opts []bus.ConsumerOption
				if backend == "shared-redis" {
					opts = append(opts, bus.WithDeduper(bus.NewRedisDedup(newFakeRedis(), time.Minute)))
				}
				consumer, err := bus.NewConsumer(sub, opts...)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				handler := func(context.Context, *envelopepb.Envelope, []byte) error { calls++; return nil }
				for i, env := range []*envelopepb.Envelope{first, second, second} {
					index := i
					if index == 2 {
						index = 1
					}
					sub.msg = bus.Message{Body: frame(t, env, nil)}
					if err := consumer.Subscribe(context.Background(), subjects[index], groups[index], handler); err != nil {
						t.Fatal(err)
					}
				}
				if calls != 2 {
					t.Fatalf("independent dispatches=%d, want 2 with the repeated second delivery suppressed", calls)
				}
			})
		}
	}
}

func TestConsumerDispatchScopeCoordinatesOnlyMatchingReplicas(t *testing.T) {
	shared := bus.NewRedisDedup(newFakeRedis(), time.Minute)
	message := bus.Message{Body: frame(t, validEnvelope(), nil)}
	calls := 0
	for _, group := range []string{"projector", "projector", "costs", "costs"} {
		consumer, err := bus.NewConsumer(&oneShotSub{msg: message}, bus.WithDeduper(shared))
		if err != nil {
			t.Fatal(err)
		}
		if err := consumer.Subscribe(context.Background(), "order.order.filled", group,
			func(context.Context, *envelopepb.Envelope, []byte) error { calls++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("replicated groups produced %d effects, want one for each logical group", calls)
	}
}

func TestConsumerDispatchScopeSeparatesInFlightGroups(t *testing.T) {
	shared := bus.NewRedisDedup(newFakeRedis(), time.Minute)
	message := bus.Message{Body: frame(t, validEnvelope(), nil)}
	ctx, cancel := context.WithCancel(context.Background())
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	effects := make(chan string, 3)
	first, err := bus.NewConsumer(&oneShotSub{msg: message}, bus.WithDeduper(shared))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(release)
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("in-flight fixture did not finish cleanly")
			}
		case <-time.After(5 * time.Second):
			t.Error("in-flight fixture did not join")
		}
	}()
	go func() {
		done <- first.Subscribe(ctx, "order.order.filled", "positions", func(context.Context, *envelopepb.Envelope, []byte) error {
			effects <- "positions"
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first dispatch did not take its claim")
	}
	for _, group := range []string{"positions", "costs"} {
		consumer, err := bus.NewConsumer(&oneShotSub{msg: message}, bus.WithDeduper(shared))
		if err != nil {
			t.Fatal(err)
		}
		if err := consumer.Subscribe(ctx, "order.order.filled", group, func(context.Context, *envelopepb.Envelope, []byte) error { effects <- group; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(effects) != 2 || <-effects != "positions" || <-effects != "costs" {
		t.Fatal("in-flight claim blocked an independent group or admitted its own replica")
	}
}

func TestConsumerDispatchScopeDocumentsUnsupportedRawKeyOverlap(t *testing.T) {
	message := bus.Message{Body: frame(t, validEnvelope(), nil)}
	probe := &dispatchScopeDeduper{}
	consumer, err := bus.NewConsumer(&oneShotSub{msg: message}, bus.WithDeduper(probe))
	if err != nil {
		t.Fatal(err)
	}
	handler := func(context.Context, *envelopepb.Envelope, []byte) error { return nil }
	if err := consumer.Subscribe(context.Background(), "subject", "group", handler); err != nil {
		t.Fatal(err)
	}
	old := bus.NewDedupWindow(time.Minute, 10)
	// An old COMMAND may choose this literal raw key; no hash collision is
	// needed. Therefore old/new shared-backend overlap is not a supported rollout.
	if len(probe.keys) != 2 || !old.Claim(probe.keys[0]) {
		t.Fatal("raw-key overlap counterexample was not established")
	}
	consumer, err = bus.NewConsumer(&oneShotSub{msg: message}, bus.WithDeduper(old))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := consumer.Subscribe(context.Background(), "subject", "group", func(context.Context, *envelopepb.Envelope, []byte) error { calls++; return nil }); err != nil || calls != 0 {
		t.Fatal("raw-key overlap counterexample changed; re-review rollout requirements")
	}
}

type dispatchScopeDeduper struct{ operations, keys []string }

func (d *dispatchScopeDeduper) Claim(key string) bool {
	d.operations, d.keys = append(d.operations, "claim"), append(d.keys, key)
	return true
}
func (d *dispatchScopeDeduper) Commit(key string) {
	d.operations, d.keys = append(d.operations, "commit"), append(d.keys, key)
}
func (d *dispatchScopeDeduper) Release(key string) {
	d.operations, d.keys = append(d.operations, "release"), append(d.keys, key)
}

func TestConsumerDispatchScopeFollowsEveryClaimExit(t *testing.T) {
	for _, outcome := range []string{"success", "handler-failure", "retained-failure", "parked", "park-failure", "backoff-canceled"} {
		t.Run(outcome, func(t *testing.T) {
			dedup := &dispatchScopeDeduper{}
			var dlq bus.Publisher = &captureClient{}
			opts := []bus.ConsumerOption{bus.WithDeduper(dedup)}
			subject := "order.order.submit"
			want := "release"
			switch outcome {
			case "success":
				want = "commit"
			case "retained-failure":
				subject = "audit.authority.decision"
				opts = append(opts, bus.WithRetainedRetries(subject))
			case "parked", "park-failure":
				if outcome == "parked" {
					want = "commit"
				} else {
					dlq = &errPublisher{err: errors.New("dummy park failure")}
				}
				opts = append(opts, bus.WithDLQ(dlq))
			case "backoff-canceled":
				opts = append(opts, fastRetry(2))
			}
			consumer, err := bus.NewConsumer(&oneShotSub{msg: bus.Message{Body: frame(t, validEnvelope(), nil)}}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_ = consumer.Subscribe(ctx, subject, "commands", func(context.Context, *envelopepb.Envelope, []byte) error {
				if outcome == "success" {
					return nil
				}
				if outcome == "backoff-canceled" {
					cancel()
				}
				return errors.New("dummy handler failure")
			})
			if len(dedup.operations) != 2 || dedup.operations[0] != "claim" || dedup.operations[1] != want || dedup.keys[0] == "" || dedup.keys[0] != dedup.keys[1] || dedup.keys[0] == validEnvelope().IdempotencyKey {
				t.Fatal("claim and terminal action did not use one dispatch-scoped key")
			}
		})
	}
}

func TestConsumerDispatchScopePreservesDisabledAndRedisFailOpen(t *testing.T) {
	failed := newFakeRedis()
	failed.failClaim, failed.failSet, failed.failDel = true, true, true
	for name, option := range map[string]bus.ConsumerOption{
		"disabled":      bus.WithDedupWindow(0, 0),
		"typed-nil":     bus.WithDeduper((*bus.RedisDedup)(nil)),
		"redis-failure": bus.WithDeduper(bus.NewRedisDedup(failed, time.Minute)),
		"empty-key":     bus.WithValidator(func(*envelopepb.Envelope) error { return nil }),
	} {
		t.Run(name, func(t *testing.T) {
			env := validEnvelope()
			if name == "empty-key" {
				env.IdempotencyKey = "" // custom-validator compatibility, not a live valid envelope
			}
			sub := &multiShotSub{msg: bus.Message{Body: frame(t, env, nil)}, times: 2}
			consumer, err := bus.NewConsumer(sub, option)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			if err := consumer.Subscribe(context.Background(), "subject", "group", func(context.Context, *envelopepb.Envelope, []byte) error { calls++; return nil }); err != nil || calls != 2 {
				t.Fatal("disabled or unavailable dedup suppressed work")
			}
		})
	}
}
