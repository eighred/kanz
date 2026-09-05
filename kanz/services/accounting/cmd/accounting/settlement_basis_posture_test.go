package main

// Every fill this book folds is booked as settled at the instant it executed,
// which is correct while every venue adapter is crypto spot and is invisible from
// outside: a book that never tracks settlement and one that tracks it with
// nothing outstanding produce identical numbers. These tests pin the three things
// that make it visible — a series for EVERY settlement basis including the zero,
// a WARN naming what is unproduced and what would arm it, and the guarantee that
// a basis added to the ledger and forgotten here still gets a series rather than
// none at all. #1043.

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// settlementSeries reads kanz_accounting_settlement_basis_produced off a registry
// as label→value. A missing metric returns a nil map, which every caller below
// distinguishes from an empty one: "absent" is the failure mode under test.
func settlementSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_accounting_settlement_basis_produced" {
			continue
		}
		out := map[string]float64{}
		for _, m := range f.GetMetric() {
			var label string
			for _, l := range m.GetLabel() {
				if l.GetName() == "basis" {
					label = l.GetValue()
				}
			}
			out[label] = m.GetGauge().GetValue()
		}
		return out
	}
	return nil
}

// EVERY BASIS GETS A SERIES, INCLUDING THE ZERO. An absent series answers an
// alert with "no data", which is the same ambiguity moved out of the book and
// into the monitoring system.
func TestSettlementBasisPostureSeedsEveryDeclaredBasis(t *testing.T) {
	reg := prometheus.NewRegistry()
	unproduced := stateSettlementBasisPosture(reg, slog.New(slog.DiscardHandler))

	series := settlementSeries(t, reg)
	if series == nil {
		t.Fatal("kanz_accounting_settlement_basis_produced was never registered — every basis is " +
			"ABSENT rather than zero")
	}
	if len(series) != len(ledger.SettlementBases) {
		t.Fatalf("%d series for %d declared bases (%v) — a basis with no series cannot be alerted "+
			"on", len(series), len(ledger.SettlementBases), series)
	}
	if got, ok := series["pending"]; !ok || got != 0 {
		t.Fatalf("pending series = %v (present=%v), want 0: nothing in this estate can place an "+
			"instrument that settles later than it executes, and the gauge is what says so", got, ok)
	}
	if got := series["settled"]; got != 1 {
		t.Fatalf("settled series = %v, want 1 — ledger.fillSettlement asserts it for every fill", got)
	}
	if got := series["unknown"]; got != 1 {
		t.Fatalf("unknown series = %v, want 1: the cash consumer, corpact and accruals all produce "+
			"entries that assert no basis, and pretending otherwise would hide the gap that makes "+
			"Book.SettlementBasisComplete false", got)
	}
	if len(unproduced) != 1 || unproduced[0] != "pending" {
		t.Fatalf("unproduced = %v, want exactly [pending]", unproduced)
	}
}

// logCapture collects one handler's records as text so a test can assert on what
// an operator would actually read.
type logCapture struct{ sb *strings.Builder }

func (c logCapture) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(c.sb, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// THE ZERO MUST COME WITH A SENTENCE. A gauge at 0 with no log line makes an
// operator guess whether settlement is untracked or merely quiet, which is the
// ambiguity the posture exists to remove.
func TestSettlementBasisPostureWarnsWithTheReasonAndTheArmingCondition(t *testing.T) {
	logs := logCapture{sb: &strings.Builder{}}
	stateSettlementBasisPosture(prometheus.NewRegistry(), logs.logger())
	out := logs.sb.String()

	for _, want := range []string{
		"basis=pending",
		"MarginModes",  // the capability the justification is derived from
		"would_arm_it", // what would change the answer
		"level=WARN",   // not Info: this must survive an operator's filter
		"kanz_accounting_settlement_basis_produced", // the durable half, named in the line
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the startup posture never mentions %q.\n\nlog:\n%s", want, out)
		}
	}
}

// A BASIS ADDED TO THE LEDGER AND FORGOTTEN HERE STILL GETS A SERIES. This is the
// trap the entry-source posture already fell into once: a metric that appeared
// only for the values somebody remembered makes a forgotten one absent rather
// than zero — indistinguishable from a service that never had it.
func TestAnUndescribedSettlementBasisIsStillSeededAndSaidOutLoud(t *testing.T) {
	reg := prometheus.NewRegistry()
	logs := logCapture{sb: &strings.Builder{}}

	// The posture map with ledger.SettlementPending removed — the state a newly
	// declared basis starts in.
	postures := settlementBasisPostures()
	delete(postures, ledger.SettlementPending)

	unproduced := seedSettlementBasisPosture(reg, logs.logger(), postures)

	series := settlementSeries(t, reg)
	if len(series) != len(ledger.SettlementBases) {
		t.Fatalf("%d series for %d declared bases (%v) — the undescribed basis was omitted rather "+
			"than seeded at 0", len(series), len(ledger.SettlementBases), series)
	}
	if got, ok := series["pending"]; !ok || got != 0 {
		t.Fatalf("the undescribed basis reads %v (present=%v), want a 0 series", got, ok)
	}
	if len(unproduced) != 1 || unproduced[0] != "pending" {
		t.Fatalf("unproduced = %v, want [pending]", unproduced)
	}
	if out := logs.sb.String(); !strings.Contains(out, "NO PRODUCER POSTURE DECLARED") {
		t.Errorf("an undescribed basis was seeded silently — nothing tells whoever added it that "+
			"its posture is missing.\n\nlog:\n%s", out)
	}
}
