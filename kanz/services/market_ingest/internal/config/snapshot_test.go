package config

import (
	"testing"
	"time"
)

func TestSnapshotScheduleConfiguration(t *testing.T) {
	for _, tc := range []struct {
		mode, interval string
		wantOnChange   bool
		wantInterval   time.Duration
		wantError      bool
	}{
		{"", "", false, time.Second, false},
		{"true", "250ms", true, 250 * time.Millisecond, false},
		{"invalid", "250ms", false, 0, true},
		{"true", "250", false, 0, true},
		{"true", "0s", false, 0, true},
	} {
		t.Setenv("MARKET_INGEST_SNAPSHOT_ON_CHANGE", tc.mode)
		t.Setenv("MARKET_INGEST_SNAPSHOT_INTERVAL", tc.interval)
		cfg, err := Load()
		if (err != nil) != tc.wantError || cfg.SnapshotOnChange != tc.wantOnChange || cfg.SnapshotInterval != tc.wantInterval {
			t.Errorf("mode %q, interval %q: got onChange=%v interval=%v err=%v", tc.mode, tc.interval, cfg.SnapshotOnChange, cfg.SnapshotInterval, err)
		}
	}
}
