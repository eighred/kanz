package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	comp "github.com/eighred/kanz/internal/compliance"
)

// A POST-TRADE BREACH RECORD THAT NEVER REACHED THE AUDIT TRAIL MUST BE COUNTED
// (#622).
//
// The monitor writes its breach decision with `_ = m.recorder.Record(...)`.
// Best-effort is argued and the argument is right — audit.go: "an audit-sink
// outage must not become a trading outage" — and the recorder logs. What was
// missing is a counter, and the distinction matters here more than usual:
//
// audit.go stamps EventTime from rec.Result.GetEvaluatedAt(), and the producer
// REFUSES a zero EventTime. So an upstream that forgets to stamp evaluated_at
// makes EVERY record fail, not some — and a sink that fails every time looks
// exactly like a sink that is quiet. That is the #245 failure mode, and without a
// counter it is invisible again: the audit trail is empty while trading
// continues and every probe stays green.

// failingRecorder is a comp.DecisionRecorder that always refuses, standing in for
// the zero-EventTime case and for a sink outage alike.
type failingRecorder struct{ n int }

func (f *failingRecorder) Record(context.Context, comp.DecisionRecord) error {
	f.n++
	return errors.New("audit sink unavailable")
}

func TestABreachRecordThatFailedToStoreIsCounted(t *testing.T) {
	var lost int
	fb := &fakeBus{}
	rec := &failingRecorder{}
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, NewEmitter(fb), rec, nil,
		WithDroppedRecordObserver(func() { lost++ }))

	// The concentration fixture breaches on this position.
	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, t0.Add(time.Minute))); err != nil {
		t.Fatalf("a failing audit sink must not fail the fold: %v", err)
	}

	if rec.n == 0 {
		t.Fatal("premise broken: the recorder was never called, so nothing could be dropped")
	}
	if lost != 1 {
		t.Fatalf("dropped-record observer fired %d time(s), want 1 — a breach decision that never "+
			"reached the audit trail is exactly what nobody can see", lost)
	}
	// The BREACH itself still went out: the audit failure must not swallow the
	// event it was recording.
	if len(fb.events) == 0 {
		t.Fatal("the breach was not emitted — an audit-sink outage became a monitoring outage")
	}
}

// A RECORDER THAT SUCCEEDS COUNTS NOTHING.
func TestASuccessfulRecordIsNotCountedAsLost(t *testing.T) {
	var lost int
	fb := &fakeBus{}
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, NewEmitter(fb), &countingRecorder{}, nil,
		WithDroppedRecordObserver(func() { lost++ }))

	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if lost != 0 {
		t.Fatalf("a successful record was counted as lost %d time(s)", lost)
	}
}

// countingRecorder accepts everything.
type countingRecorder struct{ n int }

func (c *countingRecorder) Record(context.Context, comp.DecisionRecord) error {
	c.n++
	return nil
}

// The seam is optional — a deployment that forgets it still records and still
// folds.
func TestTheDroppedRecordObserverIsOptional(t *testing.T) {
	fb := &fakeBus{}
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, NewEmitter(fb), &failingRecorder{}, nil)
	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, t0.Add(time.Minute))); err != nil {
		t.Fatalf("no observer wired must not change the fold: %v", err)
	}
}
