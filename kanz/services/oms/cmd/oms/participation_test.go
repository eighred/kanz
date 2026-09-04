package main

import (
	"log/slog"
	"testing"

	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/oms/internal/order"
)

// THE COMPOSITION ROOT'S HALF OF THE PARTICIPATION SIGNALS (#1007).
//
// composition-root wiring escapes every unit test in the packages it wires, and
// this service has twice shipped a crashing root with a green suite. What is
// asserted here is the pair of properties the alerts depend on and which neither
// the order package nor the alert file can state for itself.

// EVERY QUALITY SERIES EXISTS AT ZERO BEFORE THE FIRST DECISION.
//
// OMSParticipationUnmeasurable is "unobservable is rising AND measured-or-partial
// is not", and its second half compares against series that, on an unseeded
// CounterVec, do not exist until something increments them. On the estate the
// rule was written for — one measuring nothing — that is an EMPTY VECTOR, the
// `and` never matches, and the alert is silent in precisely the state it detects.
//
// WHAT THIS TEST CANNOT SEE, MEASURED RATHER THAN ASSUMED: it reads
// order.ParticipationQualities as its own truth set, so a quality DELETED from
// that slice while still being emitted leaves this green — the mutation was run
// on 2026-09-04 and survived here. The list's completeness is held one layer out,
// by test/arch/participation_quality_is_stated_test.go, which derives the
// vocabulary from the protobuf descriptor instead. What this test holds is the
// other half and the half that guard cannot reach: that the counter is actually
// on the registry a scrape reads, and that every seeded series is a zero rather
// than a claim.
func TestEveryParticipationQualityIsSeededAtZero(t *testing.T) {
	obs := testProvider()
	participationCounters(obs)

	families, err := obs.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "kanz_oms_participation_measurements_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			seen[labelValue(m, "quality")] = m.GetCounter().GetValue()
		}
	}
	if len(seen) == 0 {
		t.Fatal("kanz_oms_participation_measurements_total exports no series at all — the counter " +
			"is not registered, or the seeding loop was removed. An alert reads this metric")
	}
	if len(order.ParticipationQualities) == 0 {
		t.Fatal("order.ParticipationQualities is empty — the seed list is the truth set for this " +
			"test, and an empty one asserts nothing")
	}
	for _, q := range order.ParticipationQualities {
		v, ok := seen[q]
		if !ok {
			t.Errorf("quality %q exports NO series before its first increment. A rule comparing "+
				"against it gets an empty vector and cannot fire — which is the whole reason the "+
				"counter is seeded", q)
			continue
		}
		if v != 0 {
			t.Errorf("quality %q is seeded at %v, want 0 — seeding must create the series without "+
				"claiming the outcome occurred", q, v)
		}
	}
	if len(seen) != len(order.ParticipationQualities) {
		t.Errorf("counter exports %d series but ParticipationQualities lists %d — a series nothing "+
			"can increment sits at zero forever and reads as \"measured, never happened\"",
			len(seen), len(order.ParticipationQualities))
	}
}

// THE BREACH COUNTER IS REGISTERED, AND IS NOT SEEDED.
//
// Both halves matter and they pull in opposite directions.
//
// REGISTERED, because a collector that is constructed and handed to the service
// but never added to the registry exports nothing at all, and OMSParticipationCapExceeded
// would then be structurally unable to fire however many caps were breached —
// #283's defect, on the one series that says a control did not hold.
//
// NOT SEEDED, because its labels are a venue and an instrument. Those are only
// knowable from a decision that actually breached, so there is no honest zero to
// write: seeding would put a venue/instrument pair on a dashboard this OMS may
// never have traded. The rule over it is `increase(...) > 0`, which needs no
// seed — an event counter that has never fired has nothing to say.
func TestTheParticipationBreachCounterIsRegisteredAndUnseeded(t *testing.T) {
	obs := testProvider()
	_, exceeded := participationCounters(obs)

	const name = "kanz_oms_participation_cap_exceeded_total"
	if got := seriesCount(t, obs, name); got != 0 {
		t.Errorf("%s exports %d series before any breach — a seeded venue/instrument pair is a "+
			"book this OMS may never have traded, presented as a measured zero", name, got)
	}

	exceeded.WithLabelValues("XSIM", "BTC-USD").Inc()
	if got := seriesCount(t, obs, name); got != 1 {
		t.Fatalf("%s exports %d series after a breach was counted, want 1. The collector is not "+
			"on the registry the exporter scrapes, so every breach this OMS measures is invisible "+
			"and the alert cannot fire however many caps are broken", name, got)
	}
}

// seriesCount is how many series one metric family currently exports on the
// provider's own registry — the thing a scrape would see, which is the only
// question either assertion above is asking.
func seriesCount(t *testing.T, obs *observability.Provider, name string) int {
	t.Helper()
	families, err := obs.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return len(f.GetMetric())
		}
	}
	return 0
}

// THE FIVE COLLECTORS THIS FEATURE ADDS COEXIST ON ONE REGISTRY.
//
// prometheus.MustRegister PANICS on a duplicate name, and both of these run
// inside runConsumers before the process serves anything — so a collision is not
// a bad metric, it is an OMS that will not start. Composition-root wiring escapes
// every unit test in the packages it wires and this service has twice shipped a
// crashing root with a green suite, which is the whole reason this assertion is
// here rather than assumed from five distinct-looking names.
func TestTheParticipationCollectorsRegisterTogether(t *testing.T) {
	obs := testProvider()
	bindRealisedTape(obs, slog.New(slog.DiscardHandler))
	participationCounters(obs)

	for _, name := range []string{
		"kanz_oms_realised_volume_series",
		"kanz_oms_realised_volume_candles_folded",
		"kanz_oms_realised_volume_candles_refused",
		"kanz_oms_participation_measurements_total",
	} {
		if seriesCount(t, obs, name) == 0 {
			t.Errorf("%s exports no series after registration — it is not on the registry the "+
				"exporter scrapes, and every rule written over it reads as zero", name)
		}
	}
}
