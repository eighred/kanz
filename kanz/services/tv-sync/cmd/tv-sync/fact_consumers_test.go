package main

import (
	"context"
	"errors"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
	"github.com/eighred/kanz/services/tv-sync/internal/server"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTVRefusesOverlappingPriceFilters(t *testing.T) {
	for _, s := range []string{"order.*", "order.>", "order.order.filled", "*.quote", ">", ".quote"} {
		if validateFactPriceSeparation([]string{s}) == nil {
			t.Fatalf("accepted ambiguous filter %q", s)
		}
	}
	for _, s := range []string{"market.*.trade", "market.*.quote", "custom.*"} {
		if err := validateFactPriceSeparation([]string{s}); err != nil {
			t.Fatal(err)
		}
	}
}
func TestGuardedFoldSerializesOutcomeAndRevokesOnPanic(t *testing.T) {
	r := server.NewReadiness(context.Background())
	r.Set(true)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 2)
	var count atomic.Int32
	failure := errors.New("first failed")
	h := guardedFold(r, "a", func(context.Context, *envelopepb.Envelope, []byte) error {
		if count.Add(1) == 1 {
			close(entered)
			<-release
			return failure
		}
		return nil
	})
	env := &envelopepb.Envelope{TenantId: "a", EventId: "one"}
	go func() { done <- h(context.Background(), env, nil) }()
	<-entered
	go func() { done <- h(context.Background(), env, nil) }()
	select {
	case err := <-done:
		t.Fatalf("later handler overtook earlier outcome: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	// Completion sends may race after the fold lock releases. Require exactly
	// the injected failure and one successful retry, in either receive order.
	failures := 0
	for range 2 {
		if err := <-done; errors.Is(err, failure) {
			failures++
		} else if err != nil {
			t.Fatalf("unexpected fold outcome: %v", err)
		}
	}
	if failures != 1 {
		t.Fatalf("failed folds = %d, want 1", failures)
	}
	if !r.Ready() {
		t.Fatal("earlier failure overwrote later successful retry")
	}
	panics := guardedFold(r, "a", func(context.Context, *envelopepb.Envelope, []byte) error { panic("fixture") })
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		_ = panics(context.Background(), env, nil)
	}()
	if r.Ready() {
		t.Fatal("panic left serving ready")
	}
}

type routeRecorder struct{ calls []string }

func (r *routeRecorder) SubscribeReady(_ context.Context, subject, _ string, _ bus.EventHandler, ready func()) error {
	r.calls = append(r.calls, subject)
	ready()
	return nil
}
func TestTVConsumerRouterSeparatesFactsAndPrices(t *testing.T) {
	facts, prices := &routeRecorder{}, &routeRecorder{}
	router := factConsumerRouter{facts: facts, prices: prices}
	subjects := append(projection.Subjects(), "market.*.trade", "market.*.quote")
	for _, subject := range subjects {
		if err := router.SubscribeReady(context.Background(), subject, "fixture", nil, func() {}); err != nil {
			t.Fatal(err)
		}
	}
	if len(facts.calls) != len(projection.Subjects()) || len(prices.calls) != 2 {
		t.Fatalf("fact/price clients mixed: %v %v", facts.calls, prices.calls)
	}
}
