package main

// THE REFUSALS, WHICH ARE THE PART OF THIS HARNESS THAT MUST NOT BE WRONG.
//
// Everything else here produces a number somebody can sanity-check. These decide
// whether a write-path load test runs at all, and the failure they exist to
// prevent — a load generator pointed at an OMS holding live exchange credentials
// — is not recoverable by noticing afterwards. So each arm is tested from the
// direction that costs money: the input that must REFUSE.

import (
	"bufio"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/bus"
)

func scrapeOf(t *testing.T, text string) scrape {
	t.Helper()
	s, err := parseScrape(bufio.NewScanner(strings.NewReader(text)))
	if err != nil {
		t.Fatalf("parseScrape: %v", err)
	}
	return s
}

// A SIMULATOR IS THE ONLY POSTURE THAT RUNS.
func TestSimulatedPostureIsPermitted(t *testing.T) {
	s := scrapeOf(t, "kanz_oms_live_venue_adapters 0\nkanz_oms_simulated_venues 2\n")
	if err := refuseUnlessSimulated(s); err != nil {
		t.Fatalf("a simulator-only OMS was refused: %v", err)
	}
}

// A LIVE ADAPTER REFUSES. This is the whole point of the file.
func TestALiveVenueAdapterRefusesTheRun(t *testing.T) {
	s := scrapeOf(t, "kanz_oms_live_venue_adapters 1\nkanz_oms_simulated_venues 0\n")
	err := refuseUnlessSimulated(s)
	if err == nil {
		t.Fatal("an OMS holding a LIVE venue adapter was permitted. Every submission this harness " +
			"makes would be an order at a real exchange")
	}
	if !strings.Contains(err.Error(), "LIVE venue adapter") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}
}

// A LIVE ADAPTER ALONGSIDE A SIMULATOR STILL REFUSES. The dangerous reading is
// "there is a simulator, so this is a simulator" — one live adapter is one live
// exchange, whatever else is registered beside it.
func TestALiveAdapterBesideASimulatorStillRefuses(t *testing.T) {
	s := scrapeOf(t, "kanz_oms_live_venue_adapters 1\nkanz_oms_simulated_venues 3\n")
	if err := refuseUnlessSimulated(s); err == nil {
		t.Fatal("an OMS with one live adapter and three simulators was permitted")
	}
}

// AN ABSENT FAMILY IS UNKNOWN, AND UNKNOWN FAILS CLOSED. This is the shape an
// OMS predating the posture gauges has, which is precisely when guessing costs
// the most — and the shape a metrics URL pointed at the wrong process has.
func TestAnAbsentPostureMetricRefusesRatherThanReadingAsZero(t *testing.T) {
	for _, tt := range []struct{ name, text string }{
		{"neither gauge", "kanz_oms_claim_timeouts_total 0\n"},
		{"only the live gauge", "kanz_oms_live_venue_adapters 0\n"},
		{"only the simulated gauge", "kanz_oms_simulated_venues 1\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := refuseUnlessSimulated(scrapeOf(t, tt.text))
			if err == nil {
				t.Fatal("an OMS that does not report its venue posture was permitted. An absent " +
					"family reads as zero to every consumer, so 'no live adapters' would be " +
					"indistinguishable from 'this build never answered'")
			}
			if !strings.Contains(err.Error(), "UNKNOWN") {
				t.Errorf("the refusal does not say the posture is unknown: %v", err)
			}
		})
	}
}

// AN OMS WITH NO VENUES AT ALL REFUSES TOO, and for a different reason: nothing
// would trade, and nothing would be measured either. The router refuses a MIC it
// has no venue for, so the run would report the throughput of the refusal path —
// #859's defect wearing a write-path costume.
func TestAnOMSWithNoVenuesAtAllRefuses(t *testing.T) {
	s := scrapeOf(t, "kanz_oms_live_venue_adapters 0\nkanz_oms_simulated_venues 0\n")
	err := refuseUnlessSimulated(s)
	if err == nil {
		t.Fatal("an OMS with no venues at all was permitted; every order would be refused and the " +
			"run would measure the refusal path")
	}
	if !strings.Contains(err.Error(), "NO venues") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}
}

// THE BUDGET IS THE ESTATE'S NUMBER, NOT THIS FILE'S.
//
// workMaxAckPending is the one value copied out of pkg/bus (the work class's
// MaxAckPending is unexported). If the delivery contract is retuned there and
// this constant is not, the harness goes on quoting a budget the platform no
// longer runs — silently, because the number would still look plausible.
func TestTheAdmissionBudgetMatchesTheDeliveryContract(t *testing.T) {
	// The budget must be a strict fraction of the AckWait it is derived from: a
	// budget at or above AckWait bounds nothing.
	if b := admissionBudget(); b <= 0 || b >= bus.WorkAckWait {
		t.Fatalf("admissionBudget() = %s, which is not strictly inside bus.WorkAckWait (%s)",
			b, bus.WorkAckWait)
	}
	// And it must be the rule the tuning states: MaxAckPending × handling < AckWait.
	if got, want := admissionBudget()*workMaxAckPending, bus.WorkAckWait; got != want {
		t.Errorf("%d × the admission budget is %s, not bus.WorkAckWait (%s). The divisor here has "+
			"drifted from pkg/bus's work-class MaxAckPending, so this harness is asserting against "+
			"a delivery contract the estate does not run", workMaxAckPending, got, want)
	}
}
