package main

// Sweep-liveness tests (#983).
//
// The property under test is not "a number is right" — it is that a control
// which has STOPPED is distinguishable from one that is working. Every case
// below names the state that was previously indistinguishable from health.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func sweepReg(t *testing.T, interval time.Duration) (*prometheus.Registry, *sweepMetrics) {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	mx := newSweepMetrics(interval)
	mx.register(reg, interval)
	return reg, mx
}

// THE TIMESTAMP IS ZERO UNTIL A SWEEP COMPLETES, and the series EXISTS at zero.
//
// Both halves matter and they fail differently. An absent series makes
// CompliancePassiveBreachSweepStalled evaluate over an empty vector, so it is
// silent in the state it was written for. A series initialised to time.Now()
// would make a process whose loop never started look healthy for three
// intervals after every restart — and a broker-less deployment look healthy for
// three intervals, forever, on a loop that will never run.
func TestTheLastSuccessTimestampStartsAtZeroAndExists(t *testing.T) {
	reg, _ := sweepReg(t, time.Minute)

	got := gather(t, reg, "kanz_compliance_reevaluate_last_success_timestamp_seconds")
	v, ok := got[""]
	if !ok {
		t.Fatal("the last-success gauge is not registered at startup. CompliancePassiveBreachSweepStalled " +
			"compares time() against it, and a rule over an absent series evaluates to nothing — so " +
			"a compliance whose sweep never started would page nobody (#983).")
	}
	if v != 0 {
		t.Fatalf("last-success seeded at %v, want 0. A non-zero start makes a process whose sweep has "+
			"NEVER run look current, which is exactly the broker-less deployment this rule exists "+
			"to catch.", v)
	}
}

// THE CONFIGURED CADENCE IS EXPORTED, because the alert is written in intervals.
//
// Without it the rule has to hard-code a staleness bound, which is correct only
// for the 60s default and silently stops being correct the moment somebody sets
// COMPLIANCE_REEVALUATE_INTERVAL — a rule that decays without anybody editing it.
func TestTheConfiguredIntervalIsExported(t *testing.T) {
	reg, _ := sweepReg(t, 10*time.Minute)

	got := gather(t, reg, "kanz_compliance_reevaluate_interval_seconds")
	if got[""] != 600 {
		t.Fatalf("interval gauge = %v, want 600. The staleness rule multiplies this by three; if it "+
			"does not reflect the deployment's own cadence, a fund that widened its sweep would be "+
			"paged on every cycle.", got[""])
	}
}

// A COMPLETED SWEEP ADVANCES THE TIMESTAMP.
func TestASuccessfulSweepAdvancesTheTimestamp(t *testing.T) {
	reg, mx := sweepReg(t, time.Minute)
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	mx.now = func() time.Time { return at }

	mx.succeeded()
	if got := gather(t, reg, "kanz_compliance_reevaluate_last_success_timestamp_seconds")[""]; got != float64(at.Unix()) {
		t.Fatalf("timestamp = %v, want %v", got, at.Unix())
	}
}

// A FAILED SWEEP DOES NOT ADVANCE IT, and that is the whole discrimination.
//
// A sweep that errored has not established that every book was looked at.
// Treating it as liveness would let a loop failing on its first book for a week
// report itself as current: the failure counter would rise beside a timestamp
// saying everything is fine, and the staleness rule — the one that says the
// control is not doing its job — would never fire.
func TestAFailedSweepDoesNotAdvanceTheTimestamp(t *testing.T) {
	reg, mx := sweepReg(t, time.Minute)
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	mx.now = func() time.Time { return at }

	mx.failed()

	if got := gather(t, reg, "kanz_compliance_reevaluate_last_success_timestamp_seconds")[""]; got != 0 {
		t.Fatalf("a FAILED sweep advanced the last-success timestamp to %v. The sweep did not finish, "+
			"so the books it did not reach are unchecked — recording it as liveness lets a loop "+
			"that fails every cycle report itself as healthy forever (#983).", got)
	}
	if got := gather(t, reg, "kanz_compliance_reevaluate_failures_total")[""]; got != 1 {
		t.Fatalf("failures = %v, want 1", got)
	}
}

// THE TWO SERIES REPORT DIFFERENT FAULTS. A working-then-failing sweep must show
// a rising failure count AND a frozen timestamp, because that pairing is how an
// operator tells "running and erroring" from "not running".
func TestAFailingSweepFreezesTheTimestampItAlreadyHad(t *testing.T) {
	reg, mx := sweepReg(t, time.Minute)
	first := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	mx.now = func() time.Time { return first }
	mx.succeeded()

	mx.now = func() time.Time { return first.Add(time.Hour) }
	mx.failed()
	mx.failed()

	if got := gather(t, reg, "kanz_compliance_reevaluate_last_success_timestamp_seconds")[""]; got != float64(first.Unix()) {
		t.Fatalf("timestamp moved to %v after two failures, want it frozen at %v", got, first.Unix())
	}
	if got := gather(t, reg, "kanz_compliance_reevaluate_failures_total")[""]; got != 2 {
		t.Fatalf("failures = %v, want 2", got)
	}
}

// A NIL CLOCK DEFAULTS RATHER THAN PANICS. The loop this rides runs for the life
// of the process; a nil dereference in it would stop the control it measures.
func TestANilClockDoesNotPanic(t *testing.T) {
	reg, mx := sweepReg(t, time.Minute)
	mx.now = nil

	mx.succeeded()
	if got := gather(t, reg, "kanz_compliance_reevaluate_last_success_timestamp_seconds")[""]; got == 0 {
		t.Fatal("a nil clock recorded no timestamp")
	}
}

// THE LOOP TELLS THREE OUTCOMES APART, and each one is a different alert.
//
// This exercises reevaluateBooks itself rather than the metric set, because the
// PLACEMENT is what regresses: an instrumentation call in the wrong branch is
// invisible until somebody reads the loop, and it is exactly the mistake that
// makes a failing sweep report itself as healthy.

// fakeSweeper returns a scripted result and counts the calls.
type fakeSweeper struct {
	err    error
	calls  int
	cancel context.CancelFunc
}

func (f *fakeSweeper) ReevaluateAll(ctx context.Context) error {
	f.calls++
	if f.cancel != nil {
		f.cancel()
	}
	if f.err != nil {
		return f.err
	}
	return ctx.Err()
}

// A COMPLETED SWEEP ADVANCES LIVENESS AND COUNTS NO FAILURE.
func TestTheLoopRecordsACompletedSweep(t *testing.T) {
	reg, mx := sweepReg(t, 5*time.Millisecond)
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	mx.now = func() time.Time { return at }

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	sw := &fakeSweeper{}
	reevaluateBooks(ctx, sw, 5*time.Millisecond, discardLogger(), mx)

	if sw.calls == 0 {
		t.Fatal("the loop never swept")
	}
	if got := gather(t, reg, "kanz_compliance_reevaluate_last_success_timestamp_seconds")[""]; got != float64(at.Unix()) {
		t.Fatalf("the loop ran %d sweeps and recorded no success (timestamp %v). Every completed "+
			"sweep must advance it, or CompliancePassiveBreachSweepStalled fires on a healthy "+
			"estate and gets silenced.", sw.calls, got)
	}
	if got := gather(t, reg, "kanz_compliance_reevaluate_failures_total")[""]; got != 0 {
		t.Fatalf("a clean sweep incremented the failure counter %v times", got)
	}
}

// AN ERRORED SWEEP COUNTS A FAILURE AND DOES NOT ADVANCE LIVENESS.
//
// This is the branch the previous version of this test could not reach, because
// a monitor holding no books cannot fail. Without it, deleting mx.failed() from
// the loop was invisible — the sweep would run, error, and the only signal would
// be the slower staleness rule, with nothing naming the cause.
func TestTheLoopRecordsAnErroredSweep(t *testing.T) {
	reg, mx := sweepReg(t, 5*time.Millisecond)
	mx.now = func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	sw := &fakeSweeper{err: errors.New("mandate registry unresolved")}
	reevaluateBooks(ctx, sw, 5*time.Millisecond, discardLogger(), mx)

	if got := gather(t, reg, "kanz_compliance_reevaluate_failures_total")[""]; got == 0 {
		t.Fatalf("the loop ran %d errored sweeps and counted none. CompliancePassiveBreachSweepFailing "+
			"is what names the cause — without it an operator sees only the slower staleness rule "+
			"and is sent to restart a pod whose loop is running fine (#983).", sw.calls)
	}
	if got := gather(t, reg, "kanz_compliance_reevaluate_last_success_timestamp_seconds")[""]; got != 0 {
		t.Fatalf("an ERRORED sweep advanced the last-success timestamp to %v. The sweep did not "+
			"finish, so the books it never reached are unchecked, and a loop failing every cycle "+
			"would report itself as current forever.", got)
	}
}

// A SWEEP CANCELLED MID-FLIGHT IS NEITHER.
//
// On shutdown ReevaluateAll returns ctx.Err(). Counting that as a failure puts
// every clean shutdown into an alert; recording it as a success lets a
// terminating pod stamp its last partial pass as a completed one. The fake
// cancels from INSIDE the call, which is the only way to reach the branch — an
// already-cancelled context makes the loop exit on <-ctx.Done() without ever
// ticking, which is what the earlier version of this test was actually
// measuring.
func TestTheLoopIgnoresASweepCancelledMidFlight(t *testing.T) {
	reg, mx := sweepReg(t, 5*time.Millisecond)
	mx.now = func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := &fakeSweeper{cancel: cancel}
	reevaluateBooks(ctx, sw, 5*time.Millisecond, discardLogger(), mx)

	if sw.calls != 1 {
		t.Fatalf("the fake swept %d times, want exactly 1 — the loop must return once the context is done", sw.calls)
	}
	if got := gather(t, reg, "kanz_compliance_reevaluate_failures_total")[""]; got != 0 {
		t.Fatalf("a cancelled sweep counted %v failure(s). Every clean shutdown would raise "+
			"CompliancePassiveBreachSweepFailing, and an alert that fires on normal operation "+
			"gets silenced.", got)
	}
	if got := gather(t, reg, "kanz_compliance_reevaluate_last_success_timestamp_seconds")[""]; got != 0 {
		t.Fatalf("a cancelled sweep recorded a success (%v) — a terminating pod would stamp its last "+
			"partial pass as a completed one", got)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(sinkWriter{}, nil))
}

type sinkWriter struct{}

func (sinkWriter) Write(p []byte) (int, error) { return len(p), nil }
