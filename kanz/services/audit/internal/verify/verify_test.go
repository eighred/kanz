package verify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/services/audit/internal/audit"
)

// THE TAMPER CHECK RUNS ON A SCHEDULE, AND SAYS WHICH OF THREE THINGS HAPPENED
// (#665).
//
// The audit hash chain had a tamper check and nothing ran it. audit-deploy.yaml
// granted kanz-monitoring an authority on the strength of a schedule that did not
// exist, and the manifest's own words describe the state that left: "tamper
// detection depending on somebody remembering to look".
//
// # Three outcomes, not two
//
// The distinction this file exists to hold is that a verification has THREE
// results and collapsing any pair of them is the defect:
//
//	INTACT   the chain verified
//	BROKEN   the chain did not verify - a tamper, and the loudest thing here
//	ERROR    the store could not be read, so NOTHING WAS VERIFIED
//
// An ERROR reported as INTACT is a tamper check that passes while the database
// is down. An ERROR reported as BROKEN pages somebody to investigate a forgery
// that has not happened. Both are worse than saying "I could not tell", which is
// what ResultError means.

func seed(t *testing.T, n int) *audit.Memory {
	t.Helper()
	st := audit.NewMemory()
	for i := 0; i < n; i++ {
		if _, err := st.Append(context.Background(), &audit.Record{
			EventID: string(rune('a'+i)) + "-evt",
			Summary: "seeded",
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	return st
}

func TestAnIntactChainVerifies(t *testing.T) {
	v := New(seed(t, 3))
	got := v.Once(context.Background())

	if got.Outcome != OutcomeIntact {
		t.Fatalf("outcome = %q, want %q (detail %q, err %v)", got.Outcome, OutcomeIntact, got.Detail, got.Err)
	}
	if got.Records != 3 {
		t.Fatalf("records = %d, want 3", got.Records)
	}
}

// A TAMPER IS THE LOUDEST OUTCOME AND MUST NOT BE AN ERROR. A broken chain is a
// successful verification with a bad answer, not a failed verification — and the
// difference decides which alert fires and who gets paged.
func TestATamperedChainIsBrokenNotAnError(t *testing.T) {
	st := seed(t, 3)
	var all []*audit.Record
	if err := st.Scan(context.Background(), func(r *audit.Record) error {
		all = append(all, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	all[0].Summary = "FORGED"

	got := New(st).Once(context.Background())

	if got.Outcome != OutcomeBroken {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeBroken)
	}
	if got.Err != nil {
		t.Fatalf("a tampered chain is a VERDICT, not a failure to reach one: %v", got.Err)
	}
	if got.Detail == "" {
		t.Fatal("a broken chain reported no detail — the alert has nothing to say WHERE")
	}
}

// AN UNREADABLE STORE IS NEITHER INTACT NOR BROKEN. Reporting it as intact is a
// tamper check that passes while the database is down; reporting it as broken
// pages somebody to investigate a forgery that did not happen.
func TestAnUnreadableStoreIsItsOwnOutcome(t *testing.T) {
	boom := errors.New("connection refused")
	got := New(&failingStore{err: boom}).Once(context.Background())

	if got.Outcome != OutcomeError {
		t.Fatalf("outcome = %q, want %q — an unread chain must not report as verified",
			got.Outcome, OutcomeError)
	}
	if !errors.Is(got.Err, boom) {
		t.Fatalf("err = %v, want the store's error", got.Err)
	}
}

// RUN VERIFIES IMMEDIATELY, NOT ONLY AFTER THE FIRST INTERVAL.
//
// A ticker-only loop is the classic shape of this bug: a pod that restarts more
// often than the interval NEVER verifies, and the estate looks scheduled while
// nothing is ever checked. That is the failure this whole issue is about, so it
// would be a poor thing to reintroduce inside the fix.
func TestRunVerifiesImmediatelyAtStartup(t *testing.T) {
	// A CHANNEL, NOT A SLICE. The observer runs on Run's goroutine and the
	// assertions on the test's; sharing a slice between them is a data race that
	// -race would find in CI and this box cannot (no cgo), so it is not written.
	first := make(chan Result, 4)
	v := New(seed(t, 2),
		WithInterval(time.Hour), // far longer than this test will run
		WithObserver(func(r Result) {
			select {
			case first <- r:
			default:
			}
		}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- v.Run(ctx) }()

	var got Result
	select {
	case got = <-first:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Run performed no verification before its first tick — a pod restarting more " +
			"often than the interval would never verify at all")
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	if got.Outcome != OutcomeIntact {
		t.Fatalf("first result = %q, want %q", got.Outcome, OutcomeIntact)
	}
}

// AND IT KEEPS GOING. One verification at boot would leave a long-lived pod
// attesting to a chain it checked once, days ago.
func TestRunVerifiesRepeatedly(t *testing.T) {
	seen := make(chan struct{}, 16)
	v := New(seed(t, 2),
		WithInterval(time.Millisecond),
		WithObserver(func(Result) {
			select {
			case seen <- struct{}{}:
			default:
			}
		}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = v.Run(ctx) }()

	for i := 0; i < 3; i++ {
		select {
		case <-seen:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d verification(s) in 5s — the loop is not repeating", i)
		}
	}
}

// A CANCELLED CONTEXT STOPS THE LOOP, so shutdown is not held by the interval.
func TestRunStopsOnContextCancel(t *testing.T) {
	v := New(seed(t, 1), WithInterval(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- v.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// failingStore refuses every scan.
type failingStore struct {
	audit.Store
	err error
}

func (f *failingStore) Head(context.Context) (audit.Head, error) { return audit.Head{}, f.err }
func (f *failingStore) Scan(context.Context, func(*audit.Record) error) error {
	return f.err
}
