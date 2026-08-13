package app

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
)

// A BREACH TRIGGERS THE DECISION PATH, AND EXECUTES NOTHING (#74).
//
// That sentence IS #74's "Verified when", and until now only half of it was true.
// internal/risk/unwind proved its own arithmetic and test/arch proved it cannot
// execute — but nothing called it. A skeleton with no caller is indistinguishable
// from one that does not work, and the issue sat in needs-verification because of
// exactly that gap.
//
// These tests run the trigger.

var unwindNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func breachPayload(t *testing.T, vs ...*compliancepb.Violation) []byte {
	t.Helper()
	b, err := proto.Marshal(&compliancepb.ComplianceBreach{
		PortfolioId: "fund-alpha", MandateId: "m-1", MandateVersion: 3,
		Result: &compliancepb.ComplianceResult{
			Status: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH, Violations: vs,
		},
	})
	if err != nil {
		t.Fatalf("marshal breach: %v", err)
	}
	return b
}

func concentration(observed, limit string) *compliancepb.Violation {
	return &compliancepb.Violation{
		RuleId:   "conc-1",
		Severity: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
		Evidence: map[string]string{
			"dimension": "SECTOR", "bucket": "TECH", "observed": observed, "limit": limit,
		},
	}
}

type watchFixture struct {
	w    *UnwindWatch
	reg  *prometheus.Registry
	logs *unwindLogs
}

func newWatch(t *testing.T) watchFixture {
	t.Helper()
	reg := prometheus.NewRegistry()
	logs := &unwindLogs{}
	w := NewUnwindWatch(reg, "__system__", slog.New(logs), WithUnwindClock(func() time.Time { return unwindNow }))
	return watchFixture{w: w, reg: reg, logs: logs}
}

// THE DECISION PATH RUNS AND SIZES A REDUCTION.
func TestUnwindWatch_ABreachIsDecided(t *testing.T) {
	f := newWatch(t)

	if err := f.w.Handle(context.Background(), &envelopepb.Envelope{EventId: "e1"},
		breachPayload(t, concentration("0.20", "0.10"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := counterValue(t, f.reg, "kanz_risk_unwind_proposals_total", "true"); got != 1 {
		t.Errorf("actionable proposals = %v, want 1 — the breach did not reach the decider", got)
	}
	// The proposal must reach an operator with the SCOPE attached: "shed 0.555556"
	// means nothing without knowing it is a fraction of the TECH bucket.
	line := f.logs.text()
	for _, want := range []string{"fund-alpha", "TECH", "SECTOR"} {
		if !strings.Contains(line, want) {
			t.Errorf("proposal log does not mention %q; logged:\n%s", want, line)
		}
	}
	// THE NUMBER ITSELF, because the arithmetic is the part that is easy to get
	// plausibly wrong. w=0.20 against m=0.10 gives f = (w−m)/(w·(1−m)) = 5/9 =
	// 0.555556, NOT the naive (w−m)/w = 0.5 — shedding a bucket shrinks the total
	// too, so the naive form solves for a denominator that will not exist once the
	// trade is done and leaves the book still in breach. Both look reasonable in a
	// log; only one clears the limit.
	if !strings.Contains(line, "0.555556") {
		t.Errorf("proposal does not carry the correct fraction 0.555556; logged:\n%s", line)
	}
	if strings.Contains(line, "shed 0.500000") {
		t.Error("the proposal used the NAIVE concentration fraction (0.5), which undersheds and " +
			"leaves the portfolio in breach")
	}
}

// AND IT SAYS, IN THE SAME BREATH, THAT IT DID NOT ACT. The whole scope of #74 is
// decide-do-not-execute, and an operator reading a line about shedding 55% of a
// sector must not have to wonder whether it already happened.
func TestUnwindWatch_TheLogSaysNothingWasExecuted(t *testing.T) {
	f := newWatch(t)
	_ = f.w.Handle(context.Background(), &envelopepb.Envelope{EventId: "e1"},
		breachPayload(t, concentration("0.20", "0.10")))

	if !strings.Contains(f.logs.text(), "nothing has been executed") {
		t.Errorf("the proposal log does not state that nothing was executed; logged:\n%s", f.logs.text())
	}
}

// A BREACH NOTHING CAN BE DERIVED FOR IS COUNTED, NOT DROPPED.
//
// "This portfolio is in breach and the platform cannot size a way out" is the
// more alarming outcome of the two, and it is the one that reads as a quiet
// system if it is not counted. Undecidable rules get their own counter because a
// proposal can be actionable overall while still leaving individual rules unsolved.
func TestUnwindWatch_AnUndecidableBreachIsCountedAndNamed(t *testing.T) {
	f := newWatch(t)

	// No evidence numbers ⇒ nothing derivable.
	v := &compliancepb.Violation{
		RuleId:   "mystery-1",
		Severity: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
	}
	if err := f.w.Handle(context.Background(), &envelopepb.Envelope{EventId: "e2"},
		breachPayload(t, v)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := counterValue(t, f.reg, "kanz_risk_unwind_proposals_total", "false"); got != 1 {
		t.Errorf("non-actionable proposals = %v, want 1", got)
	}
	if got := plainCounter(t, f.reg, "kanz_risk_unwind_undecidable_rules_total"); got != 1 {
		t.Errorf("undecidable rules = %v, want 1 — a rule nobody can unwind is a rule a human "+
			"must handle, and an uncounted one looks like a quiet system", got)
	}
	// It must name the RULE, not merely count it. A count tells an operator
	// something is unresolved without telling them what to look at.
	if !strings.Contains(f.logs.text(), "mystery-1") {
		t.Errorf("undecidable rule not named; logged:\n%s", f.logs.text())
	}
}

// GARBAGE IS ACKED, NOT REDELIVERED FOREVER. This handler observes; it does not
// own the breach. Nacking would replay the same bad bytes indefinitely while
// occupying the consumer real breaches arrive on, and the breach FACT itself is
// durable regardless of whether this handler could read it.
func TestUnwindWatch_UndecodableBytesAreAcked(t *testing.T) {
	f := newWatch(t)

	if err := f.w.Handle(context.Background(), &envelopepb.Envelope{EventId: "e3"},
		[]byte("not a proto")); err != nil {
		t.Fatalf("Handle(garbage) = %v, want nil (ack) — a nack replays it forever", err)
	}
	if got := plainCounter(t, f.reg, "kanz_risk_unwind_undecidable_rules_total"); got != 0 {
		t.Errorf("undecodable bytes were counted as an undecidable RULE (%v) — they are a "+
			"different failure and conflating them hides both", got)
	}
}

// counterValue reads a labelled counter.
func counterValue(t *testing.T, reg *prometheus.Registry, name, label string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetValue() == label {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func plainCounter(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			for _, m := range f.GetMetric() {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("counter %q is not registered", name)
	return 0
}

// unwindLogs captures what was logged.
type unwindLogs struct {
	b strings.Builder
}

func (h *unwindLogs) Enabled(context.Context, slog.Level) bool { return true }

func (h *unwindLogs) Handle(_ context.Context, r slog.Record) error {
	h.b.WriteString(r.Level.String())
	h.b.WriteString(" ")
	h.b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		h.b.WriteString(" ")
		h.b.WriteString(a.Key)
		h.b.WriteString("=")
		h.b.WriteString(a.Value.String())
		return true
	})
	h.b.WriteString("\n")
	return nil
}

func (h *unwindLogs) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *unwindLogs) WithGroup(string) slog.Handler      { return h }
func (h *unwindLogs) text() string                       { return h.b.String() }
