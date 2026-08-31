package main

// #865 — "IS THIS OMS FILLING AGAINST AN EXCHANGE, OR AGAINST NOTHING?"
//
// Until these gauges the answer lived in one WARN line at startup. Four venue
// counters existed and all four sit at ZERO for an OMS with no venues at all, so
// kanz_oms_unverified_venue_account_total == 0 meant "every adapter proved its
// account" on a live deployment and "there are no adapters" on a simulator, with
// nothing between them. That is the collapse CLAUDE.md names: "nothing
// configured" and "checked, and fine" must never look the same.
//
// The write-path load harness (test/load/orderflow) refuses to submit a single
// order unless it can read these two numbers off the process it is about to
// drive, so a wrong answer here is not a cosmetic metric bug — it is a load test
// that believes it is talking to a simulator.
//
// These drive the REAL configuredVenues, on both arms, against a real registry.

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/services/oms/internal/config"
)

// gatherPosture runs configuredVenues against a fresh registry and returns what
// the two gauges hold afterwards, plus how many series each family carries.
//
// THE SERIES COUNT IS NOT DECORATION. A gauge that is registered and never Set
// exports its family with no series, and every consumer — Prometheus, Grafana,
// KEDA, and this estate's own harness — reads that as zero. "Set to 0" and "never
// written" must be distinguishable here or the harness's `live == 0` check is
// satisfied by an OMS that never answered the question.
func gatherPosture(t *testing.T, cfg config.Config) (live, sim float64, liveSeries, simSeries int) {
	t.Helper()
	reg := prometheus.NewRegistry()
	posture := newVenuePostureGauges(reg)
	_, _, closeConns, err := configuredVenues(context.Background(), cfg, nil, nil,
		testVenueCounters(), posture, quietLogger())
	if err != nil {
		t.Fatalf("configuredVenues: %v", err)
	}
	t.Cleanup(closeConns)
	return testutil.ToFloat64(posture.liveVenueAdapters),
		testutil.ToFloat64(posture.simulatedVenues),
		testutil.CollectAndCount(posture.liveVenueAdapters),
		testutil.CollectAndCount(posture.simulatedVenues)
}

// AN OMS WITH NO ADAPTERS SAYS SO IN NUMBERS. It is the posture every local rig,
// every test stack and — the case that matters — every production deployment
// whose OMS_VENUE_ENDPOINTS went missing runs in.
func TestSimulatorPostureIsReportedAsANumber(t *testing.T) {
	live, sim, liveSeries, simSeries := gatherPosture(t, config.Config{SimVenueMIC: "XNAS,XLON"})

	if sim != 2 {
		t.Errorf("kanz_oms_simulated_venues = %v, want 2 — two SimVenue MICs were configured, and "+
			"this number is the only thing an alert or the load harness can read to learn that "+
			"orders here are filled against nothing", sim)
	}
	if live != 0 {
		t.Errorf("kanz_oms_live_venue_adapters = %v, want 0 — no adapter was configured", live)
	}
	// The zero above must be a WRITTEN zero.
	if liveSeries != 1 {
		t.Errorf("kanz_oms_live_venue_adapters carries %d series, want 1 — an unwritten gauge exports "+
			"an empty family, which every consumer reads as zero, so 'no live venues' would be "+
			"indistinguishable from 'this build never answered'", liveSeries)
	}
	if simSeries != 1 {
		t.Errorf("kanz_oms_simulated_venues carries %d series, want 1", simSeries)
	}
}

// AN OMS WITH A REAL ADAPTER SAYS THAT INSTEAD, and the simulated count is driven
// to a written zero rather than left at whatever the sim arm would have put there.
func TestLiveAdapterPostureIsReportedAsANumber(t *testing.T) {
	addr := serveAdapter(t, &venuepb.DescribeResponse{
		Mic: "XOKX", Account: "okx-sub-1", AccountVerified: true, ExchangeAccountId: "99999",
	})

	live, sim, liveSeries, simSeries := gatherPosture(t, config.Config{
		VenueEndpoints: "XOKX/okx-sub-1=" + addr,
		// Set, and deliberately IGNORED on this arm: a deployment that names both
		// must report the adapter, not the simulator it never reached.
		SimVenueMIC: "XSIM",
	})

	if live != 1 {
		t.Errorf("kanz_oms_live_venue_adapters = %v, want 1 — one adapter dialled and verified", live)
	}
	if sim != 0 {
		t.Errorf("kanz_oms_simulated_venues = %v, want 0 — the sim arm is not reached when an "+
			"adapter is configured, and a load harness that read a non-zero here would submit "+
			"orders believing they reach nothing", sim)
	}
	if liveSeries != 1 || simSeries != 1 {
		t.Errorf("series: live=%d sim=%d, want 1 and 1", liveSeries, simSeries)
	}
}
