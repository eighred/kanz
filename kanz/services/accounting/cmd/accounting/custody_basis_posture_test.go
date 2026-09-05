package main

// The comparison basis of every configured pair must be readable from outside the
// process (#1073).
//
// The line this replaced recorded an undeclared portfolio at INFO as an
// affirmatively correct configuration — "compares the whole portfolio book",
// reason "one custodian configured for this portfolio" — while comparing the whole
// book against one custodian's statement is what reported an investor subscription
// into the fund's own bank as a cash break for the full amount, every run. These
// tests pin the three things that make the truth visible: a series for BOTH bases
// of every pair including the zero, a WARN that names what the derived basis does
// not cover, and registration that does not depend on a broker being configured.

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

// sawAttrAt reports whether a record at level carries needle in its message or in
// any ATTRIBUTE value. capturingHandler.sawAt reads the message alone, and the
// basis label is an attribute — asserting only the message would leave the join
// between this log line and the gauge untested, which is the half an operator
// actually uses.
func sawAttrAt(h *capturingHandler, level slog.Level, needle string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level != level {
			continue
		}
		if strings.Contains(r.Message, needle) {
			return true
		}
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if strings.Contains(a.Value.String(), needle) {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// basisSeries reads kanz_accounting_custody_comparison_basis off a registry as
// "portfolio|custodian|basis" -> value. A missing metric returns a nil map, which
// every caller below distinguishes from an empty one: "absent" is the failure mode
// under test.
func basisSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != custodyBasisMetric {
			continue
		}
		out := map[string]float64{}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			out[labels["portfolio"]+"|"+labels["custodian"]+"|"+labels["basis"]] = m.GetGauge().GetValue()
		}
		return out
	}
	return nil
}

// postureFor runs the posture over a configuration, returning the registry, the
// captured log and the derived portfolios.
func postureFor(t *testing.T, cfg config.Config) (*prometheus.Registry, *capturingHandler, []string) {
	t.Helper()
	cc, err := buildCustodyConfig(cfg)
	if err != nil {
		t.Fatalf("buildCustodyConfig: %v", err)
	}
	reg := prometheus.NewRegistry()
	h := &capturingHandler{}
	return reg, h, stateCustodyBasisPosture(reg, slog.New(h), cc.pairs, cc.scope)
}

// BOTH BASES GET A SERIES FOR EVERY PAIR, INCLUDING THE ZERO. An absent series
// answers a dashboard with "no data", which is the same ambiguity moved out of the
// book and into the monitoring system — and "no data" is exactly what a pair whose
// basis nobody stated looks like.
func TestCustodyBasisPostureSeedsBothBasesForEveryPair(t *testing.T) {
	cfg := baseCfg()
	cfg.CustodyPairs = []string{"PF1:CUST-A", "PF2:CUST-X", "PF2:CUST-Y"}
	cfg.CustodyAccounts = "PF2:CUST-X:okx-sub-1 PF2:CUST-Y:bin-main"

	reg, _, derived := postureFor(t, cfg)
	series := basisSeries(t, reg)
	if series == nil {
		t.Fatal("kanz_accounting_custody_comparison_basis was never registered — every pair's basis is " +
			"ABSENT rather than zero, and an operator cannot tell a declared scope from a derived one")
	}
	want := map[string]float64{
		"PF1|CUST-A|derived":  1,
		"PF1|CUST-A|declared": 0,
		"PF2|CUST-X|declared": 1,
		"PF2|CUST-X|derived":  0,
		"PF2|CUST-Y|declared": 1,
		"PF2|CUST-Y|derived":  0,
	}
	for key, v := range want {
		got, ok := series[key]
		if !ok {
			t.Errorf("%s{%s} is ABSENT — a basis nobody stated reads as no data, which is the state "+
				"this gauge exists to abolish", custodyBasisMetric, key)
			continue
		}
		if got != v {
			t.Errorf("%s{%s} = %v, want %v", custodyBasisMetric, key, got, v)
		}
	}
	if len(series) != len(want) {
		t.Errorf("gathered %d series, want %d: %v", len(series), len(want), series)
	}
	if len(derived) != 1 || derived[0] != "PF1" {
		t.Errorf("derived portfolios = %v, want [PF1] — PF2 declares its accounts", derived)
	}
}

// THE DERIVED PAIR IS ANNOUNCED, AND THE ANNOUNCEMENT NAMES THE RESIDUE.
//
// "This portfolio's scope came from its journal" is only half the fact. The other
// half is that entries which settled against no exchange account are in NO
// comparison basis and are reconciled by nothing — real money in the book of
// record that no statement can confirm. Saying only the first half is how the
// replaced INFO line steered an operator away from the one thing that mattered.
func TestCustodyBasisPostureNamesWhatTheDerivedBasisDoesNotCover(t *testing.T) {
	_, h, _ := postureFor(t, baseCfg())

	if !h.sawAt(slog.LevelWarn, "RECONCILED BY NOTHING") {
		t.Errorf("the derived basis was not announced as leaving anything unreconciled.\n\n"+
			"warn: %v\ninfo: %v", h.messagesAt(slog.LevelWarn), h.messagesAt(slog.LevelInfo))
	}
	// Not an ERROR: the derivation is correct and a fund bank account is a
	// legitimate place for cash to be. What would be wrong is not saying so.
	if len(h.messagesAt(slog.LevelError)) > 0 {
		t.Errorf("a correct single-custodian configuration was recorded at ERROR: %v",
			h.messagesAt(slog.LevelError))
	}
	// The word an operator greps for has to be the word the gauge uses, or the
	// log and the dashboard are two facts.
	if !sawAttrAt(h, slog.LevelWarn, custody.BasisDerived) {
		t.Errorf("the warning does not carry the basis label the gauge is keyed on (%q), so an "+
			"operator cannot join the line to the series: %v", custody.BasisDerived, h.messagesAt(slog.LevelWarn))
	}
}

// A DECLARED PORTFOLIO IS NOT WARNED ABOUT. The warning is about what the derived
// basis cannot cover; firing it on every pair would make it noise, and a warning
// that fires always is one nobody reads — the same failure the break queue itself
// is being protected from.
func TestCustodyBasisPostureIsQuietForADeclaredPortfolio(t *testing.T) {
	cfg := baseCfg()
	cfg.CustodyPairs = []string{"PF2:CUST-X", "PF2:CUST-Y"}
	cfg.CustodyAccounts = "PF2:CUST-X:okx-sub-1 PF2:CUST-Y:bin-main"

	_, h, derived := postureFor(t, cfg)
	if len(derived) != 0 {
		t.Errorf("derived portfolios = %v, want none — both custodians declare their accounts", derived)
	}
	for _, m := range h.messagesAt(slog.LevelWarn) {
		if strings.Contains(m, "RECONCILED BY NOTHING") {
			t.Errorf("a fully declared configuration was warned about: %q", m)
		}
	}
}

// REGISTERED FOR A DEPLOYMENT WITH NO PAIRS AT ALL. A GaugeVec with no series is
// still a registered collector, so the metric exists and answers "no pairs
// configured" rather than "this build has no such metric" — and the two are
// indistinguishable to a dashboard that only ever sees an empty result.
func TestCustodyBasisPostureRegistersWithNoPairsConfigured(t *testing.T) {
	cfg := baseCfg()
	cfg.CustodyPairs = nil

	reg, _, derived := postureFor(t, cfg)
	if len(derived) != 0 {
		t.Errorf("derived portfolios = %v, want none", derived)
	}
	if series := basisSeries(t, reg); series == nil {
		// Gather() omits a GaugeVec with no children, so this asserts the
		// registration itself rather than the (correctly empty) series set.
		if err := reg.Register(prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: custodyBasisMetric, Help: "duplicate probe",
		}, []string{"portfolio", "custodian", "basis"})); err == nil {
			t.Fatal("kanz_accounting_custody_comparison_basis was never registered on a deployment " +
				"with no custody pairs — the metric is absent rather than empty, so a dashboard " +
				"cannot tell an unconfigured control from a build that never had one")
		}
	}
}
