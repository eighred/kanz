package main

// The estate configures NO FX at all — no manifest sets ACCOUNTING_FX_PAIRS or
// ACCOUNTING_INSTRUMENT_CURRENCY — and buildLiveFX is silent about it while
// failing the start on a typo. These tests pin the three things that make the
// absence visible: a series for EVERY FX input including the zeroes, a WARN
// naming what is unconfigured and what would arm it, and the series being
// present on a pod that configures nothing, which is the deployment the posture
// exists to describe. #1041.

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/config"
)

// fxSeries reads kanz_accounting_fx_wired off a registry as label→value. A
// missing metric returns a nil map, which every caller below distinguishes from
// an empty one: "absent" is the failure mode under test.
func fxSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_accounting_fx_wired" {
			continue
		}
		out := map[string]float64{}
		for _, m := range f.GetMetric() {
			var label string
			for _, l := range m.GetLabel() {
				if l.GetName() == "input" {
					label = l.GetValue()
				}
			}
			out[label] = m.GetGauge().GetValue()
		}
		return out
	}
	return nil
}

// THE ESTATE'S ACTUAL DEPLOYMENT: no FX variable set anywhere.
//
// This is the state every accounting pod in this repository runs in, and before
// #1041 nothing in the process said so — the NAV endpoint simply answered from a
// path that dropped foreign cash and summed foreign positions un-converted.
func TestAnFXLessDeploymentReportsEveryInputUnconfigured(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := &storeLogCapture{}

	unconfigured := stateFXPosture(reg, slog.New(h), config.Config{BaseCurrency: "USD"})

	series := fxSeries(t, reg)
	if series == nil {
		t.Fatal("kanz_accounting_fx_wired is ABSENT from the registry — the posture is unreadable " +
			"in exactly the deployment it describes, and an alert written over the series would " +
			"never fire because there is no series to evaluate")
	}
	for _, name := range []string{"rates", "instrument_currency"} {
		got, ok := series[name]
		if !ok {
			t.Errorf("no kanz_accounting_fx_wired{input=%q} series — an omitted input is "+
				"indistinguishable from a configured one", name)
			continue
		}
		if got != 0 {
			t.Errorf("%s = %v with nothing configured, want 0", name, got)
		}
	}
	if strings.Join(unconfigured, ",") != "instrument_currency,rates" {
		t.Errorf("unconfigured = %v, want [instrument_currency rates]", unconfigured)
	}
}

// An operator has to be able to read WHAT is missing, WHAT IT COSTS and WHAT
// WOULD ARM IT out of the log, at a level that is not filtered away.
func TestUnconfiguredFXWarnsWithTheConsequenceAndTheRemedy(t *testing.T) {
	h := &storeLogCapture{}

	stateFXPosture(prometheus.NewRegistry(), slog.New(h), config.Config{BaseCurrency: "USD"})

	if !h.hasWarnContaining("NOTHING CONFIGURES this FX input") {
		t.Fatalf("no per-input WARN; got records: %+v", h.records)
	}
	joined := allAttrText(h)
	for _, want := range []string{
		"ACCOUNTING_FX_PAIRS",
		"ACCOUNTING_INSTRUMENT_CURRENCY",
		"the valuation refuses by name rather than understating",
		"kanz_accounting_fx_wired{input=\"rates\"}=0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the FX posture never says %q; an operator cannot act on it", want)
		}
	}
}

// AND THE OTHER DIRECTION. A configured pod must report 1 and must NOT warn, or
// the WARN becomes noise every deployment logs and nobody reads.
func TestAConfiguredFXDeploymentReportsWiredAndDoesNotWarn(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := &storeLogCapture{}

	unconfigured := stateFXPosture(reg, slog.New(h), config.Config{
		BaseCurrency:       "USD",
		FXPairs:            "EURUSD:EUR",
		InstrumentCurrency: "SAP:EUR",
	})

	if len(unconfigured) != 0 {
		t.Fatalf("unconfigured = %v on a pod that sets both variables", unconfigured)
	}
	series := fxSeries(t, reg)
	for _, name := range []string{"rates", "instrument_currency"} {
		if got := series[name]; got != 1 {
			t.Errorf("%s = %v on a configured deployment, want 1", name, got)
		}
	}
	if h.hasWarnContaining("NOTHING CONFIGURES this FX input") {
		t.Errorf("a fully configured pod warned about FX; got records: %+v", h.records)
	}
}

// THE POSTURE IS DERIVED FROM THE SAME PARSE buildLiveFX RUNS, not from a second
// reading of the environment. A spec that parses to no pairs — the shape a
// hand-written check would report as "configured, it is non-empty" — must report
// 0, because it arms nothing.
func TestAnFXSpecThatParsesToNothingReportsUnconfigured(t *testing.T) {
	reg := prometheus.NewRegistry()

	unconfigured := stateFXPosture(reg, slog.New(&storeLogCapture{}), config.Config{
		BaseCurrency: "USD",
		FXPairs:      "   ",
	})
	if len(unconfigured) == 0 {
		t.Fatal("a blank ACCOUNTING_FX_PAIRS reported as configured — the posture is reading the " +
			"variable's presence rather than what it parses to, so a pod with FX effectively off " +
			"reports rates=1 and nothing warns")
	}
	if got := fxSeries(t, reg)["rates"]; got != 0 {
		t.Errorf("rates = %v for a spec that parses to no pairs, want 0", got)
	}
}
