package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/tv-sync/internal/brokerapi"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
	"github.com/eighred/kanz/services/tv-sync/internal/server"
)

type subscriptionReadyCall struct {
	subject string
	ready   func()
}
type readinessFixture struct {
	calls chan subscriptionReadyCall
	fail  string
}

func (f *readinessFixture) SubscribeReady(ctx context.Context, subject, group string, h bus.EventHandler, ready func()) error {
	if subject == f.fail {
		return errors.New("fixture subscription failure")
	}
	select {
	case f.calls <- subscriptionReadyCall{subject, ready}:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}
func receiveReadyCall(t *testing.T, ch <-chan subscriptionReadyCall) subscriptionReadyCall {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(time.Second):
		t.Fatal("subscription not started")
		return subscriptionReadyCall{}
	}
}
func TestTVReadinessWaitsForEverySubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &readinessFixture{calls: make(chan subscriptionReadyCall, 2)}
	armed := make(chan struct{}, 2)
	done := make(chan error, 1)
	go func() {
		done <- runConsumers(ctx, f, map[string]bus.EventHandler{"fact": nil, "mark": nil}, "fixture", func(ready bool) {
			if ready {
				armed <- struct{}{}
			}
		})
	}()
	a, b := receiveReadyCall(t, f.calls), receiveReadyCall(t, f.calls)
	a.ready()
	a.ready()
	select {
	case <-armed:
		t.Fatal("partial/duplicate callback armed")
	default:
	}
	b.ready()
	select {
	case <-armed:
	case <-time.After(time.Second):
		t.Fatal("complete barrier did not arm")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("exit=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("consumers did not join")
	}
	a.ready()
	b.ready()
	select {
	case <-armed:
		t.Fatal("callback after cancellation armed")
	default:
	}
}
func TestTVReadinessRefusesSubscriptionFailureAndEmptySet(t *testing.T) {
	f := &readinessFixture{calls: make(chan subscriptionReadyCall, 2), fail: "fact"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runConsumers(ctx, f, map[string]bus.EventHandler{"fact": nil, "mark": nil}, "fixture", func(ready bool) {
		if ready {
			t.Error("failed barrier armed")
		}
	}); err == nil {
		t.Fatal("failure ignored")
	}
	if err := runConsumers(ctx, f, nil, "fixture", func(ready bool) {
		if ready {
			t.Error("empty barrier armed")
		}
	}); err == nil {
		t.Fatal("empty set accepted")
	}
}

type drainingReadinessFixture struct {
	calls       chan subscriptionReadyCall
	fail, drain chan struct{}
}

func (f *drainingReadinessFixture) SubscribeReady(ctx context.Context, subject, group string, h bus.EventHandler, ready func()) error {
	f.calls <- subscriptionReadyCall{subject, ready}
	if subject == "fact" {
		select {
		case <-f.fail:
			return errors.New("fixture subscription stopped")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	<-ctx.Done()
	<-f.drain
	return ctx.Err()
}
func TestTVReadinessClosesBeforeSiblingDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &drainingReadinessFixture{calls: make(chan subscriptionReadyCall, 2), fail: make(chan struct{}), drain: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(f.drain)
		}
	}()
	ready := server.NewReadiness(ctx)
	api := server.New(ready, brokerapi.New(projection.New(time.Now, nil)))
	done := make(chan error, 1)
	go func() {
		done <- runConsumers(ctx, f, map[string]bus.EventHandler{"fact": nil, "mark": nil}, "fixture", ready.Set)
	}()
	a, b := receiveReadyCall(t, f.calls), receiveReadyCall(t, f.calls)
	a.ready()
	b.ready()
	status := func() int {
		r := httptest.NewRequest(http.MethodGet, "/broker/accounts", nil)
		r.Header.Set(auth.HeaderPrincipalTenant, "fixture")
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		return w.Code
	}
	if status() != 200 {
		t.Fatal("complete barrier did not open Broker")
	}
	close(f.fail)
	deadline := time.Now().Add(time.Second)
	for ready.Ready() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if status() != 503 {
		t.Fatal("Broker stayed open during sibling drain")
	}
	select {
	case <-done:
		t.Fatal("fixture did not hold drain")
	default:
	}
	// Parent context is deliberately still live: cancellation alone cannot mask
	// the required immediate revocation on the child subscription failure.
	if ctx.Err() != nil {
		t.Fatal("parent context unexpectedly cancelled")
	}
	close(f.drain)
	released = true
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("subscription failure disappeared")
		}
	case <-time.After(time.Second):
		t.Fatal("consumer drain did not join")
	}
}
