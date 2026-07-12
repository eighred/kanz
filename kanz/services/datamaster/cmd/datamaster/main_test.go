package main

import (
	"testing"

	"github.com/kanz-eng/kanz/services/datamaster/internal/config"
	"github.com/kanz-eng/kanz/services/datamaster/internal/feed"
)

// The fabrication guard. A SimFeed's canned records, once resolved and written to
// golden_records, are indistinguishable from mastered vendor data — same endpoint,
// same provenance, same trust downstream. Simulated data must be an explicit,
// loud choice; it must never be what a deployment falls back to because nobody set
// a flag. This is the fourth surface where that trap was reachable.
func TestGuardSimRefusesASimulatedMaster(t *testing.T) {
	sim := []feed.VendorFeed{feed.SimFeed{Name: "SIM_A"}}

	if err := guardSim(sim, false); err == nil {
		t.Fatal("a SimFeed was accepted without DATAMASTER_ALLOW_SIM — the service would master a security book out of invented data")
	}
	if err := guardSim(sim, true); err != nil {
		t.Fatalf("an explicitly permitted SimFeed must start: %v", err)
	}
	if err := guardSim(nil, false); err != nil {
		t.Fatalf("no feeds is not a simulated feed: %v", err)
	}
}

// The default build wires no vendor at all: nothing implements feed.RefSource, so
// there is no production feed to construct. An empty master is the honest state —
// the canned set exists only under the flag.
func TestBuildFeedsIsEmptyUnlessSimIsAllowed(t *testing.T) {
	if feeds := buildFeeds(config.Config{}); len(feeds) != 0 {
		t.Fatalf("the default configuration wired %d feed(s); it must wire none", len(feeds))
	}
	feeds := buildFeeds(config.Config{AllowSim: true})
	if len(feeds) == 0 {
		t.Fatal("DATAMASTER_ALLOW_SIM wired no feeds — the developer path is dead")
	}
	if err := guardSim(feeds, true); err != nil {
		t.Fatalf("the sim feeds it builds must pass their own guard: %v", err)
	}
}
