package controller_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

type rig struct {
	q      *remediate.LogQuarantiner
	roller *remediate.LogModelRoller
	scaler *actuate.LogScaler
	fail   *actuate.LogFailover
	escLog *escRecorder
	ctrl   *controller.Controller
}

func newRig(autoFailover bool, q remediate.Quarantiner) rig {
	lq := remediate.NewLogQuarantiner(discard)
	if q == nil {
		q = lq
	}
	roller := remediate.NewLogModelRoller(discard)
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
	return rig{q: lq, roller: roller, scaler: scaler, fail: fail, escLog: escLog, ctrl: ctrl}
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
		if !r.q.Quarantined("AAPL") {
			t.Error("subject not quarantined")
		}
		if n := len(r.escLog.escalations(t)); n != 0 {
			t.Errorf("escalation records = %d, want 0: an auto-remediated condition must not escalate", n)
		}
	})

	t.Run("reconcile divergence → quarantine", func(t *testing.T) {
		r := newRig(false, nil)
		mustDispatch(t, r, sig(signal.KindReconcileDivergence, "risk.exposure", signal.SeverityWarning))
		if !r.q.Quarantined("risk.exposure") {
			t.Error("divergent subject not quarantined")
		}
	})

	t.Run("model drift → rollback", func(t *testing.T) {
		r := newRig(false, nil)
		mustDispatch(t, r, sig(signal.KindDrift, "model-7", signal.SeverityCritical))
		if !r.roller.RolledBack("model-7") {
			t.Error("model not rolled back")
		}
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
		if r.q.Quarantined("AAPL") {
			t.Error("a sub-threshold WARNING must not trigger remediation")
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
	if !r.q.Quarantined("AAPL") {
		t.Error("Handle did not classify+remediate the data-quality gap")
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
func (failingQuarantiner) Quarantined(string) bool { return false }
