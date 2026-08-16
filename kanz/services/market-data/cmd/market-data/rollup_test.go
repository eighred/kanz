package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/services/market-data/internal/config"
)

// THE ROLLUP WIRING (#509). What is graded here is the composition, not the
// fold — internal/marketdata/rollup owns the arithmetic and tests it.

// A MALFORMED SERIES IS REFUSED, NOT SKIPPED.
//
// A skipped entry is indistinguishable from one nobody configured, so a typo
// would leave exactly one instrument reading 1-minute bars forever while the
// others rolled up fine — the hardest version of this to notice.
func TestAMalformedSeriesEntryIsRefusedRatherThanSkipped(t *testing.T) {
	for _, spec := range []string{"BTC-USDT", "BTC-USDT@", "@XBIN", "BTC-USDT@XBIN,ETH-USDT"} {
		if _, err := parseRollupSeries(spec); err == nil {
			t.Errorf("%q parsed — an entry with no venue names nothing, and skipping it would "+
				"leave that instrument unrolled and look identical to not configuring it", spec)
		}
	}
}

// A WELL-FORMED LIST PARSES, TOLERATING SPACES.
func TestAWellFormedSeriesListParses(t *testing.T) {
	got, err := parseRollupSeries(" BTC-USDT@XBIN , ETH-USDT@XOKX ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d series, want 2", len(got))
	}
	if got[0].InstrumentID != "BTC-USDT" || got[0].Venue != "XBIN" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Venue != "XOKX" {
		t.Errorf("second venue = %q, want XOKX — a venue is part of a bar's identity", got[1].Venue)
	}
}

// AN EMPTY SPEC YIELDS NO JOBS, so the caller can report the disabled state.
func TestAnEmptySpecYieldsNoJobsSoTheCallerCanWarn(t *testing.T) {
	jobs, err := rollupJobs(config.Config{}, store.NewMemory(), prometheus.NewRegistry(), testLogger())
	if err != nil {
		t.Fatalf("rollupJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("got %d jobs with no series configured, want 0", len(jobs))
	}
}

// EVERY CONFIGURED SERIES GETS A JOB PER COARSE RESOLUTION.
//
// Two resolutions are derived, so N series must produce 2N jobs. A missing
// resolution is the whole defect this wiring exists to fix, one series at a time.
func TestEverySeriesIsRolledUpToEveryCoarseResolution(t *testing.T) {
	cfg := config.Config{
		RollupSeries:       "BTC-USDT@XBIN,ETH-USDT@XOKX",
		RollupInterval:     config.DefaultRollupInterval,
		RollupWatermarkLag: config.DefaultRollupWatermarkLag,
	}
	jobs, err := rollupJobs(cfg, store.NewMemory(), prometheus.NewRegistry(), testLogger())
	if err != nil {
		t.Fatalf("rollupJobs: %v", err)
	}
	if len(jobs) != 4 {
		t.Fatalf("got %d jobs for 2 series, want 4 (1h and 1d each)", len(jobs))
	}
	var saw1h, saw1d int
	for _, j := range jobs {
		if j.Interval != cfg.RollupInterval {
			t.Errorf("%s interval = %v, want %v", j.Name, j.Interval, cfg.RollupInterval)
		}
		switch {
		case strings.Contains(j.Name, string(store.Resolution1h)):
			saw1h++
		case strings.Contains(j.Name, string(store.Resolution1d)):
			saw1d++
		}
	}
	if saw1h != 2 || saw1d != 2 {
		t.Errorf("1h jobs = %d, 1d jobs = %d, want 2 and 2 — Resolution1h having no producer is "+
			"the defect this wiring fixes", saw1h, saw1d)
	}
}

// A MALFORMED SPEC FAILS THE WIRING rather than yielding a partial job set.
func TestAMalformedSpecFailsTheWiring(t *testing.T) {
	cfg := config.Config{RollupSeries: "BTC-USDT@XBIN,broken"}
	if _, err := rollupJobs(cfg, store.NewMemory(), prometheus.NewRegistry(), testLogger()); err == nil {
		t.Error("a spec with one broken entry produced jobs — the good half would roll up and " +
			"the bad half would be silently absent")
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
