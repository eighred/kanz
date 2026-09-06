package main

// Custody reconciliation now compares two grains — netted balances, and the
// EXECUTIONS behind them matched by external reference — and the second only
// happens for a statement that declares it supplies trade lines. Nothing in this
// build publishes a statement at any grain, so every run in this estate compares
// totals only, and that is invisible from outside: the same outcome, the same
// staleness gauge, the same empty break list as a trade-level run that matched
// every fill. These tests pin the three things that make it readable — a series
// for EVERY grain including the zeros, a WARN naming what is unproduced and what
// would arm it, and the guarantee that a grain added to the engine and forgotten
// here still gets a series rather than none at all. #1049.

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/recon"
)

// grainSeries reads kanz_accounting_custody_statement_grain_produced off a
// registry as label→value. A missing metric returns a nil map, which every caller
// below distinguishes from an empty one: "absent" is the failure mode under test.
func grainSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != custodyGrainMetric {
			continue
		}
		out := map[string]float64{}
		for _, m := range f.GetMetric() {
			var label string
			for _, l := range m.GetLabel() {
				if l.GetName() == "grain" {
					label = l.GetValue()
				}
			}
			out[label] = m.GetGauge().GetValue()
		}
		return out
	}
	return nil
}

// EVERY GRAIN GETS A SERIES, INCLUDING THE ZEROS. An absent series answers an
// alert with "no data", which is the same ambiguity moved out of the control and
// into the monitoring system.
func TestCustodyGrainPostureSeedsEveryDeclaredGrain(t *testing.T) {
	reg := prometheus.NewRegistry()
	unproduced := stateCustodyGrainPosture(reg, slog.New(slog.DiscardHandler))

	series := grainSeries(t, reg)
	if series == nil {
		t.Fatal("kanz_accounting_custody_statement_grain_produced was never registered — every grain " +
			"is ABSENT rather than zero, and a rule over an absent series never fires")
	}
	if len(series) != len(recon.Grains()) {
		t.Fatalf("%d series for %d declared grains (%v) — a grain with no series cannot be read off a "+
			"dashboard", len(series), len(recon.Grains()), series)
	}
	// THE ONE THAT MATTERS. transactions=0 is the sentence "no execution in this
	// deployment is matched against a custodian trade line anywhere".
	if got, ok := series[recon.GrainTransactions.String()]; !ok || got != 0 {
		t.Fatalf("%s series = %v (present=%v), want 0: nothing in this build publishes a custodian "+
			"statement at all, so no execution is ever matched and 'a fill never reached the book' "+
			"stays inferable from a netted position difference rather than observed",
			recon.GrainTransactions, got, ok)
	}
	if len(unproduced) != len(recon.Grains()) {
		t.Fatalf("unproduced = %v, want every grain — accounting.custody.statement has a consumer, a "+
			"NATS grant, a stream and a table, and no producer anywhere in the module", unproduced)
	}
}

// THE POSTURE IS SAID OUT LOUD, not only exported. An operator who never opens a
// dashboard must still be able to find out from the pod's own log that the
// control compares totals.
func TestCustodyGrainPostureSaysWhatIsUnproducedAndWhatWouldArmIt(t *testing.T) {
	var lines strings.Builder
	logger := slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelInfo}))
	stateCustodyGrainPosture(prometheus.NewRegistry(), logger)

	log := lines.String()
	for _, want := range []string{
		"NETTED BALANCES",
		"#105/#106",        // the feed adapter that would arm it
		custodyGrainMetric, // the series an operator opens next
		"blind to its own composition",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the startup posture never says %q.\n\nA netted-only comparison and a trade-level "+
				"one that matched everything produce the same clean run, and a Go comment is not "+
				"reachable from a pod's logs.\n\n%s", want, log)
		}
	}
	if strings.Contains(log, "level=ERROR") {
		t.Errorf("a grain was reported as having NO POSTURE DECLARED, which means custodyGrainPostures "+
			"is missing one recon.Grains() declares:\n\n%s", log)
	}
}

// A GRAIN ADDED TO THE ENGINE AND FORGOTTEN HERE STILL GETS A SERIES. The seeding
// iterates recon.Grains() rather than the posture map, so an undescribed grain is
// zero-and-loud rather than absent — the state this whole file exists to abolish.
func TestCustodyGrainPostureSeedsAGrainNoPostureDescribes(t *testing.T) {
	reg := prometheus.NewRegistry()
	var lines strings.Builder
	logger := slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// The state a newly declared grain starts in: known to the engine, described
	// by nobody.
	partial := custodyGrainPostures()
	delete(partial, recon.GrainTransactions)
	unproduced := seedCustodyGrainPosture(reg, logger, partial)

	series := grainSeries(t, reg)
	if _, ok := series[recon.GrainTransactions.String()]; !ok {
		t.Fatalf("the undescribed grain has NO SERIES (%v). Absent and zero are the distinction this "+
			"gauge exists to make, and a grain nobody remembered would land on the wrong side of it",
			series)
	}
	if !strings.Contains(lines.String(), "NO PRODUCER POSTURE DECLARED") {
		t.Errorf("an undescribed grain was seeded silently — it is reported as unproduced because "+
			"nothing says otherwise, and that has to be said:\n\n%s", lines.String())
	}
	found := false
	for _, name := range unproduced {
		if name == recon.GrainTransactions.String() {
			found = true
		}
	}
	if !found {
		t.Errorf("unproduced = %v, want it to include the undescribed grain", unproduced)
	}
}
