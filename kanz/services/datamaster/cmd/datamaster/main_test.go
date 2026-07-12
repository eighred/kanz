package main

import (
	"os"
	"path/filepath"
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

// With no vendor drop configured the service wires nothing. An empty master is the
// honest state — the canned set exists only under the flag.
func TestBuildFeedsIsEmptyUnlessSimIsAllowed(t *testing.T) {
	feeds, err := buildFeeds(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 0 {
		t.Fatalf("the default configuration wired %d feed(s); it must wire none", len(feeds))
	}
	feeds, err = buildFeeds(config.Config{AllowSim: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) == 0 {
		t.Fatal("DATAMASTER_ALLOW_SIM wired no feeds — the developer path is dead")
	}
	if err := guardSim(feeds, true); err != nil {
		t.Fatalf("the sim feeds it builds must pass their own guard: %v", err)
	}
}

// A configured vendor drop displaces the canned book entirely: with a real vendor
// wired, ALLOW_SIM must not quietly mix invented records into the master beside it.
func TestARealVendorDisplacesTheSimBook(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reference.csv")
	if err := os.WriteFile(path, []byte("figi,currency\nBBG000BLNNH6,USD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	feeds, err := buildFeeds(config.Config{
		AllowSim:       true, // even so
		RefFiles:       map[string]string{"BLOOMBERG": path},
		VendorPriority: map[string]int{"BLOOMBERG": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 || feeds[0].Vendor() != "BLOOMBERG" {
		t.Fatalf("want exactly the real vendor, got %d feed(s)", len(feeds))
	}
	if err := guardSim(feeds, false); err != nil {
		t.Fatalf("a real vendor feed is not a simulator: %v", err)
	}
}
