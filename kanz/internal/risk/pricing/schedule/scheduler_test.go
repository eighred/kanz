package schedule

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A disabled job (non-positive interval or nil Refresh) is dropped, so New over
// a mixed set reports only the enabled ones and Run never touches the rest.
func TestNewDropsDisabledJobs(t *testing.T) {
	var called atomic.Bool
	s := New([]Job{
		{Name: "no-interval", Interval: 0, Refresh: func(context.Context, time.Time) error { called.Store(true); return nil }},
		{Name: "nil-refresh", Interval: time.Second, Refresh: nil},
		{Name: "ok", Interval: time.Hour, Refresh: func(context.Context, time.Time) error { return nil }},
	}, WithLogger(quietLogger()))

	if s.Jobs() != 1 {
		t.Fatalf("Jobs() = %d, want 1 (only the enabled job)", s.Jobs())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stop after the immediate run
	_ = s.Run(ctx)
	if called.Load() {
		t.Fatal("disabled job's Refresh was invoked")
	}
}

// Every enabled job fires once immediately on Run, before any interval elapses —
// a restart warms the stores without waiting a full cadence.
func TestRunFiresImmediately(t *testing.T) {
	var mu sync.Mutex
	fired := map[string]int{}
	mk := func(name string) Job {
		return Job{Name: name, Interval: time.Hour, Refresh: func(context.Context, time.Time) error {
			mu.Lock()
			fired[name]++
			mu.Unlock()
			return nil
		}}
	}
	s := New([]Job{mk("curve"), mk("vol")}, WithLogger(quietLogger()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	// Poll until both immediate runs land, then cancel.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := fired["curve"] + fired["vol"]
		mu.Unlock()
		if got >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("immediate runs did not fire: %v", fired)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if fired["curve"] != 1 || fired["vol"] != 1 {
		t.Fatalf("want one immediate run each, got %v", fired)
	}
}

// A Refresh error is swallowed: the loop keeps ticking and sibling jobs are
// unaffected (the scheduler-side of deny-on-garbage).
//
// It WAITS FOR THE EVENT, not for a duration. The old version ran the scheduler for
// 60ms of wall clock and then asserted that at least two 5ms ticks had landed — which
// is not the claim being made. It is a claim about how many ticks a loaded machine can
// fit in 60 milliseconds, and under `-race` in a CI container the answer is sometimes
// "fewer than two". The behaviour under test — a failing job keeps ticking, and its
// sibling is untouched — is reached here by waiting for it, so the test passes as fast
// as the machine allows and fails only if a loop has genuinely stopped. The 5s below is
// a deadline for a hung test, never a performance budget.
func TestRefreshErrorDoesNotStopLoop(t *testing.T) {
	var badCalls, goodCalls atomic.Int64
	const wantEach = 2

	done := make(chan struct{})
	var once sync.Once
	reached := func() {
		if badCalls.Load() >= wantEach && goodCalls.Load() >= wantEach {
			once.Do(func() { close(done) })
		}
	}

	s := New([]Job{
		{Name: "bad", Interval: time.Millisecond, Refresh: func(context.Context, time.Time) error {
			badCalls.Add(1)
			reached()
			return errors.New("uncalibratable quote set")
		}},
		{Name: "good", Interval: time.Millisecond, Refresh: func(context.Context, time.Time) error {
			goodCalls.Add(1)
			reached()
			return nil
		}},
	}, WithLogger(quietLogger()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	select {
	case <-done:
		// The failing job kept firing, proving the error never broke its loop; the good
		// job fired too, proving it was untouched by its sibling's failures.
	case <-time.After(5 * time.Second):
		t.Fatalf("a job stopped ticking: bad fired %d times, good fired %d (want >= %d each)",
			badCalls.Load(), goodCalls.Load(), wantEach)
	}
}

// asOf is drawn from the injected clock, so a refresh publishes point-in-time at
// the scheduler's notion of now (the MODEL-01b stance), not wall-clock.
func TestRefreshUsesInjectedClock(t *testing.T) {
	want := time.Date(2026, 7, 3, 22, 0, 0, 0, time.UTC)
	got := make(chan time.Time, 1)
	s := New([]Job{
		{Name: "curve", Interval: time.Hour, Refresh: func(_ context.Context, asOf time.Time) error {
			select {
			case got <- asOf:
			default:
			}
			return nil
		}},
	}, WithClock(func() time.Time { return want }), WithLogger(quietLogger()))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	defer cancel()

	select {
	case asOf := <-got:
		if !asOf.Equal(want) {
			t.Fatalf("asOf = %v, want injected %v", asOf, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never fired")
	}
}

// Run over an empty (or all-disabled) job set returns immediately without error,
// so the composition root can wire it unconditionally.
func TestRunEmptyReturns(t *testing.T) {
	s := New(nil, WithLogger(quietLogger()))
	done := make(chan struct{})
	go func() { _ = s.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run over empty job set did not return")
	}
}
