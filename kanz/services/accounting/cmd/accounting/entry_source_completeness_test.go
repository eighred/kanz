package main

// The gauge is what an operator reads; the posture on every cash announcement is
// what the pre-trade buying-power gate reads (#614). They come from one
// classification (classifyEntrySources) so they cannot disagree — an operator
// told by /metrics that corporate actions are unfed, while the balances that
// gate orders say they are fed, is worse off than one told nothing at all.

import (
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// WHAT RIDES ON EVERY BALANCE IS WHAT THE GAUGE SAYS. Asserted against the
// gauge rather than against a hand-written list, because a hand-written list is
// a second answer that can drift from the first — which is the whole reason
// this platform has one balance publisher.
func TestEntrySourceCompletenessAgreesWithTheGauge(t *testing.T) {
	cfg := fullyWired()
	reg := prometheus.NewRegistry()
	stateEntrySourcePosture(reg, slog.New(&storeLogCapture{}), cfg)
	series := wiredSeries(t, reg)
	if len(series) == 0 {
		t.Fatal("no kanz_accounting_entry_source_wired series — this test would compare against " +
			"nothing")
	}

	posture := entrySourceCompleteness(cfg)
	if len(posture.Produced) == 0 || len(posture.Unproduced) == 0 {
		t.Fatalf("posture = %+v — this build feeds fills and cash and feeds neither corporate "+
			"actions nor accruals, so both lists must be non-empty. An empty pair is published as "+
			"NO STATEMENT, and every consumer then reads its balance as unstated", posture)
	}
	got := map[string]float64{}
	for _, name := range posture.Produced {
		got[name] = 1
	}
	for _, name := range posture.Unproduced {
		got[name] = 0
	}
	if len(got) != len(series) {
		t.Fatalf("the posture covers %d entry types and the gauge %d — one of them is missing a "+
			"type the ledger declares", len(got), len(series))
	}
	for name, want := range series {
		if have, ok := got[name]; !ok || have != want {
			t.Errorf("entry type %q: gauge says %v, the announced posture says %v (present=%v). "+
				"An operator reading /metrics and a gate reading a balance must never be told "+
				"different things about the same feed", name, want, have, ok)
		}
	}
}

// AND corporate_action IS UNPRODUCED UNDER A FULLY CONFIGURED DEPLOYMENT. No
// environment variable arms it: nothing in this module publishes an
// accounting.v1.CorporateAction, so there is no subject a config could name
// (#588). Every balance this build announces is missing the cash effect of every
// dividend, coupon and merger, and it now says so on the wire.
func TestAFullyConfiguredDeploymentStillAnnouncesCorporateActionsAsUnproduced(t *testing.T) {
	posture := entrySourceCompleteness(fullyWired())
	for _, want := range []string{"accrual", "corporate_action"} {
		found := false
		for _, got := range posture.Unproduced {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("unproduced = %v, want it to contain %q. If a feed has been wired, this test "+
				"and the darkPackageExempt entry for services/accounting/internal/corpact go "+
				"together — do not silence one of them alone", posture.Unproduced, want)
		}
	}
	for _, notWanted := range posture.Produced {
		if notWanted == "corporate_action" || notWanted == "accrual" {
			t.Errorf("%q is announced as PRODUCED — a balance vouching for a feed that does not "+
				"exist is worse than one that says nothing", notWanted)
		}
	}
}
