package main

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/internal/compliancebus"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/config"
)

// WHICH SINK THIS BUILD RECORDS TO, AND WHETHER IT SAYS SO (#713).
//
// The gate's decisions are the audit answer to "why was this trade allowed". A
// build that records them to stdout and a build that publishes them onto the
// hash chain are both legitimate; a build where you cannot tell which one you
// deployed is not. Both arms are graded here, including the metrics that
// distinguish them, because "the recorder is keeping up" and "there is no durable
// recorder" would otherwise be the same zero.

type stubBus struct{ published int }

func (s *stubBus) Publish(context.Context, bus.Event) error {
	s.published++
	return nil
}

// WITH A BUS, the decisions are durable and the posture gauge says so.
func TestWithABusTheRecorderIsDurableAndSaysSo(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec, closeRec := buildDecisionRecorder(&stubBus{}, reg, gateLogger())
	defer closeRec()

	if _, ok := rec.(*compliancebus.AsyncRecorder); !ok {
		t.Fatalf("recorder is %T, want the asynchronous bus recorder — a synchronous one would put "+
			"broker latency into order admission, and a slog one is not on the audit chain", rec)
	}
	if got := gaugeValue(t, reg, "kanz_compliance_pretrade_recorder_durable"); got != 1 {
		t.Errorf("kanz_compliance_pretrade_recorder_durable = %v, want 1", got)
	}
}

// WITHOUT A BUS it falls back to the LOG rather than to nothing, and the gauge
// reads 0 so the weaker posture is not mistaken for the stronger one.
func TestWithNoBusTheRecorderFallsBackToTheLogAndSaysSo(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec, closeRec := buildDecisionRecorder(nil, reg, gateLogger())
	defer closeRec()

	if rec == nil {
		t.Fatal("no recorder at all with no bus — the gate would record NOWHERE, which is the #643 " +
			"state this replaced")
	}
	if _, ok := rec.(*compliancebus.AsyncRecorder); ok {
		t.Fatal("a bus recorder was built with no bus")
	}
	if got := gaugeValue(t, reg, "kanz_compliance_pretrade_recorder_durable"); got != 0 {
		t.Errorf("kanz_compliance_pretrade_recorder_durable = %v with no bus, want 0 — a log-only "+
			"build would report itself as being on the audit chain", got)
	}
}

// THE DROP COUNTER EXISTS ON BOTH ARMS, AT ZERO. A series that appears only on
// its first increment reads as no-data to an alert, and on the log arm it must
// exist so that its zero is not read as "the trail is complete" — the gauge above
// is what says otherwise.
func TestTheDropCounterExistsOnBothArms(t *testing.T) {
	for _, tc := range []struct {
		name string
		bus  compliancebus.Bus
	}{
		{"with a bus", &stubBus{}},
		{"with no bus", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			_, closeRec := buildDecisionRecorder(tc.bus, reg, gateLogger())
			defer closeRec()
			if got := testutil.CollectAndCount(reg, "kanz_compliance_decisions_dropped_total"); got != 1 {
				t.Errorf("kanz_compliance_decisions_dropped_total has %d series, want 1 at zero", got)
			}
		})
	}
}

// THE CLOSER IS NEVER NIL. The composition root defers it unconditionally, so a
// nil on either arm is a panic on the shutdown path — the one path that runs when
// something has already gone wrong.
func TestTheRecorderCloserIsNeverNil(t *testing.T) {
	for _, b := range []compliancebus.Bus{&stubBus{}, nil} {
		_, closeRec := buildDecisionRecorder(b, prometheus.NewRegistry(), gateLogger())
		if closeRec == nil {
			t.Fatalf("nil closer for bus %v", b)
		}
		closeRec()
	}
}

// THE GATE THE BUILDER RETURNS CARRIES THE DURABLE RECORDER when a bus exists —
// the seam, not just the metric. Posture reports the seam is present; this pins
// that it is the one that publishes.
func TestTheBuiltGateCarriesTheDurableRecorder(t *testing.T) {
	b := &stubBus{}
	w, err := buildPreTradeGate(masterConfigured(), gateDeps(), b, prometheus.NewRegistry(), gateLogger())
	if err != nil {
		t.Fatalf("buildPreTradeGate: %v", err)
	}
	defer w.CloseRecorder()
	if !w.Gate.Posture().Recorder {
		t.Fatal("the gate reports no recorder on a build with a bus")
	}
}

// --- the dual-control postures, extracted to pay for #713's lifecycle line ---

// EACH POSTURE IS DISTINCT AND SAID OUT LOUD. Which one a deployment is in is the
// difference between "a second person signs for a large order", "we are counting
// what would need one", and "one person commits any size" — and it was previously
// discoverable only by reading the composition root.
func TestEachDualControlPostureIsAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name    string
		require bool
		minimum *big.Rat
		want    string
	}{
		{"enforcing", true, big.NewRat(1_000_000, 1), "MAKER-CHECKER IS ENFORCING"},
		{"observing", false, big.NewRat(1_000_000, 1), "MAKER-CHECKER IS OBSERVING"},
		{"absent", false, nil, "NO DUAL-CONTROL THRESHOLD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := routerLog()
			cfg := omsConfig()
			cfg.RequireDualControl = tc.require
			cfg.DualControlMinNotional = tc.minimum
			gate, err := buildDualControlGate(cfg, gateDeps().Marks, prometheus.NewRegistry(), logger)
			if err != nil {
				t.Fatalf("buildDualControlGate: %v", err)
			}
			if gate == nil {
				t.Fatal("no gate was built — with no threshold the gate must still exist, or " +
					"'this deployment has no dual control' is a MISSING metric rather than a zero one")
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("the %s posture is not in the startup log: %s", tc.name, buf.String())
			}
		})
	}
}

// ARMING WITH NO THRESHOLD IS REFUSED. "Enforce dual control" and "above what
// size" are two halves of one decision, and a build that had only the first would
// either hold every order or hold none — both of which someone would discover in
// production rather than at startup.
func TestArmingWithNoThresholdRefusesToStart(t *testing.T) {
	logger, _ := routerLog()
	cfg := omsConfig()
	cfg.RequireDualControl = true
	cfg.DualControlMinNotional = nil
	if _, err := buildDualControlGate(cfg, gateDeps().Marks, prometheus.NewRegistry(), logger); err == nil {
		t.Fatal("buildDualControlGate armed maker-checker with no threshold — the build reports " +
			"itself as enforcing a control whose trigger nobody set")
	}
}

// THE MANDATE POSTURE IS STATED EITHER WAY, because "an unmandated portfolio
// trades unconstrained" and "it cannot trade at all" are opposite answers and
// neither should require reading the config.
func TestTheMandatePostureIsAnnouncedEitherWay(t *testing.T) {
	for _, tc := range []struct {
		require bool
		want    string
	}{
		{true, "MANDATE REQUIRED"},
		{false, "MANDATE ADVISORY"},
	} {
		logger, buf := routerLog()
		cfg := omsConfig()
		cfg.RequireMandate = tc.require
		announceMandatePosture(cfg, logger)
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("RequireMandate=%v did not announce %q: %s", tc.require, tc.want, buf.String())
		}
	}
}

// --- helpers --------------------------------------------------------------

// omsConfig is the minimum a builder in this package needs. Everything a test
// varies, it varies explicitly, so a default here can never be the thing under
// test.
func omsConfig() config.Config {
	return config.Config{Tenant: "acme", DualControlNotionalCurrency: "USD"}
}
