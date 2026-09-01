package controller_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/services/autopilot/internal/actuate"
	"github.com/eighred/kanz/services/autopilot/internal/controller"
	"github.com/eighred/kanz/services/autopilot/internal/plan"
	"github.com/eighred/kanz/services/autopilot/internal/remediate"
	"github.com/eighred/kanz/services/autopilot/internal/signal"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// call is one remediation the controller drove through a seam: what it acted on
// and the reason it passed down.
type call struct{ target, reason string }

// recordingQuarantiner / recordingRoller are the assertion surface that replaced
// remediate's LogQuarantiner.Quarantined and LogModelRoller.RolledBack when #892
// removed the ledgers behind them.
//
// The ledgers were state PRODUCTION carried so that a test could read it back —
// an unbounded map keyed by a subject off the wire, filling fastest during the
// storm autopilot exists to handle. Recording belongs to the test, so it now
// lives here, and these assert MORE than the removed bool did: which subject,
// with which reason, how many times, in what order. What LogQuarantiner and
// LogModelRoller themselves do is asserted in their own package, against the log
// line production actually keeps (remediate/remediate_test.go).
type recordingQuarantiner struct {
	mu    sync.Mutex
	calls []call
}

func (q *recordingQuarantiner) Quarantine(_ context.Context, subject, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, call{subject, reason})
	return nil
}

func (q *recordingQuarantiner) recorded() []call {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]call(nil), q.calls...)
}

type recordingRoller struct {
	mu    sync.Mutex
	calls []call
}

func (r *recordingRoller) Rollback(_ context.Context, modelID, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{modelID, reason})
	return nil
}

func (r *recordingRoller) recorded() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]call(nil), r.calls...)
}

// wantCalls asserts the exact sequence a seam saw. `want` is variadic and an
// empty want is the negative case — "this seam was not touched at all" — which
// is what a sub-threshold signal has to prove.
func wantCalls(t *testing.T, what string, got []call, want ...call) {
	t.Helper()
	if len(want) == 0 && len(got) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s calls = %v, want %v", what, got, want)
	}
}

type rig struct {
	q      *recordingQuarantiner
	roller *recordingRoller
	scaler *actuate.LogScaler
	fail   *actuate.LogFailover
	escLog *escRecorder
	ctrl   *controller.Controller
}

func newRig(autoFailover bool, q remediate.Quarantiner) rig {
	rq := &recordingQuarantiner{}
	if q == nil {
		q = rq
	}
	roller := &recordingRoller{}
	scaler := actuate.NewLogScaler(discard)
	fail := actuate.NewLogFailover(discard)
	// The escalator gets its OWN logger so a test can count the escalation
	// records it emits. That error-level line is the escalation's real sink —
	// #844 removed the in-process buffer these assertions used to read — and
	// the Controller keeps `discard`, so its own "escalating to human" warning
	// on the same path cannot be counted as one.
	escLog := &escRecorder{}
	esc := controller.NewLogEscalator(escLog.logger())
	deps := plan.Deps{Quarantiner: q, ModelRoller: roller, Scaler: scaler, Failover: fail}
	ctrl := controller.New(plan.DefaultMatcher(), plan.DefaultRegistry(deps, autoFailover), esc, discard, nil)
	return rig{q: rq, roller: roller, scaler: scaler, fail: fail, escLog: escLog, ctrl: ctrl}
}

func sig(kind signal.Kind, subject string, sev signal.Severity) signal.Signal {
	return signal.Signal{Kind: kind, Subject: subject, Severity: sev, Time: time.Now()}
}

func mustDispatch(t *testing.T, r rig, s signal.Signal) controller.Outcome {
	t.Helper()
	o, err := r.ctrl.Dispatch(context.Background(), s)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return o
}

// AUTO-01e: a data gap, model drift, and broker stall are each auto-remediated.
func TestAutoRemediation(t *testing.T) {
	t.Run("critical data gap → quarantine", func(t *testing.T) {
		r := newRig(false, nil)
		if o := mustDispatch(t, r, sig(signal.KindDataGap, "AAPL", signal.SeverityCritical)); o != controller.OutcomeRemediated {
			t.Fatalf("outcome = %s", o)
		}
		wantCalls(t, "quarantine", r.q.recorded(), call{"AAPL", "data_gap: "})
		if n := len(r.escLog.escalations(t)); n != 0 {
			t.Errorf("escalation records = %d, want 0: an auto-remediated condition must not escalate", n)
		}
	})

	t.Run("reconcile divergence → quarantine", func(t *testing.T) {
		r := newRig(false, nil)
		mustDispatch(t, r, sig(signal.KindReconcileDivergence, "risk.exposure", signal.SeverityWarning))
		wantCalls(t, "quarantine", r.q.recorded(), call{"risk.exposure", "reconcile_divergence: "})
	})

	t.Run("model drift → rollback", func(t *testing.T) {
		r := newRig(false, nil)
		mustDispatch(t, r, sig(signal.KindDrift, "model-7", signal.SeverityCritical))
		wantCalls(t, "rollback", r.roller.recorded(), call{"model-7", "drift: "})
	})

	t.Run("broker stall: staleness → scale ingest, circuit → scale inference", func(t *testing.T) {
		r := newRig(false, nil)
		mustDispatch(t, r, sig(signal.KindStaleness, "market.data", signal.SeverityCritical))
		mustDispatch(t, r, sig(signal.KindCircuitOpen, "inference", signal.SeverityCritical))
		if r.scaler.ScaleOuts("market-data") != 1 || r.scaler.ScaleOuts("inference") != 1 {
			t.Errorf("scale-outs = market-data:%d inference:%d", r.scaler.ScaleOuts("market-data"), r.scaler.ScaleOuts("inference"))
		}
	})
}

// AUTO-01e: escalation only on unrecognized / non-actionable conditions.
func TestEscalationOnlyOnUnrecognized(t *testing.T) {
	t.Run("sub-threshold WARNING gap escalates, no action", func(t *testing.T) {
		r := newRig(false, nil)
		if o := mustDispatch(t, r, sig(signal.KindDataGap, "AAPL", signal.SeverityWarning)); o != controller.OutcomeEscalated {
			t.Fatalf("outcome = %s, want escalated", o)
		}
		if got := r.q.recorded(); len(got) != 0 {
			t.Errorf("quarantine calls = %v, want none: a sub-threshold WARNING must not trigger "+
				"remediation", got)
		}
		if n := len(r.escLog.escalations(t)); n != 1 {
			t.Fatalf("escalation records = %d, want 1", n)
		}
	})

	t.Run("failed remediation step escalates", func(t *testing.T) {
		r := newRig(false, failingQuarantiner{})
		if o := mustDispatch(t, r, sig(signal.KindDataGap, "AAPL", signal.SeverityCritical)); o != controller.OutcomeEscalated {
			t.Fatalf("outcome = %s, want escalated", o)
		}
		if n := len(r.escLog.escalations(t)); n != 1 {
			t.Errorf("escalation records = %d, want 1: a failed remediation must escalate to a human", n)
		}
	})
}

func TestSLOBurnFailoverGated(t *testing.T) {
	off := newRig(false, nil)
	mustDispatch(t, off, sig(signal.KindSLOBurn, "us-east-1", signal.SeverityCritical))
	if off.scaler.ScaleOuts("risk-engine") != 1 {
		t.Error("SLO burn should scale risk-engine")
	}
	if off.fail.FailedOver("us-east-1") {
		t.Error("failover must not auto-fire with AutoFailover off (human-in-the-loop)")
	}

	on := newRig(true, nil)
	mustDispatch(t, on, sig(signal.KindSLOBurn, "us-east-1", signal.SeverityCritical))
	if !on.fail.FailedOver("us-east-1") {
		t.Error("with AutoFailover on, a critical SLO burn should fail the region over")
	}
}

// The full Handle path: classify a real DataQualityEvent envelope and remediate.
func TestHandleClassifiesAndRemediates(t *testing.T) {
	r := newRig(false, nil)
	payload, err := proto.Marshal(&observationpb.DataQualityEvent{
		Subject:  "AAPL",
		Severity: observationpb.Severity_SEVERITY_CRITICAL,
		Detail:   &observationpb.DataQualityEvent_Gap{Gap: &observationpb.GapDetail{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := &envelopepb.Envelope{
		EventId:          "e1",
		Domain:           "data",
		EventType:        "data.quality.detected",
		PayloadSchemaRef: "observation.v1.DataQualityEvent:1",
		EventTime:        timestamppb.New(time.Unix(0, 0).UTC()),
	}
	if err := r.ctrl.Handle(context.Background(), env, payload); err != nil {
		t.Fatal(err)
	}
	if got := r.q.recorded(); !reflect.DeepEqual(got, []call{{"AAPL", "data_gap: "}}) {
		t.Errorf("quarantine calls = %v, want the AAPL data gap — Handle did not classify+remediate "+
			"the data-quality event", got)
	}

	// A non-signal event is ignored (acked, no action).
	plain := &envelopepb.Envelope{EventType: "risk.portfolio.snapshot", EventTime: timestamppb.New(time.Unix(0, 0).UTC())}
	if err := r.ctrl.Handle(context.Background(), plain, nil); err != nil {
		t.Fatal(err)
	}
}

type failingQuarantiner struct{}

func (failingQuarantiner) Quarantine(context.Context, string, string) error {
	return errors.New("quarantine backend down")
}
