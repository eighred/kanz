package config

import (
	"strings"
	"testing"
	"time"
)

// The checkpoint job is ON by default (#229). An operator who has never heard
// of ACCOUNTING_SNAPSHOT_INTERVAL must still get the bounded read path — the
// service is unbounded without it, so "unset" cannot mean "off".
func TestSnapshotDefaultsAreOn(t *testing.T) {
	t.Setenv("ACCOUNTING_SNAPSHOT_INTERVAL", "")
	t.Setenv("ACCOUNTING_SNAPSHOT_BATCH", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SnapshotInterval != DefaultSnapshotInterval {
		t.Errorf("SnapshotInterval=%s want %s", cfg.SnapshotInterval, DefaultSnapshotInterval)
	}
	if cfg.SnapshotInterval <= 0 {
		t.Error("the default snapshot interval is not positive — checkpointing would be OFF by " +
			"default and every NAV request would scan the whole journal")
	}
	if cfg.SnapshotBatch != DefaultSnapshotBatch || cfg.SnapshotBatch <= 0 {
		t.Errorf("SnapshotBatch=%d want %d and positive", cfg.SnapshotBatch, DefaultSnapshotBatch)
	}
}

func TestSnapshotSettingsAreRead(t *testing.T) {
	t.Setenv("ACCOUNTING_SNAPSHOT_INTERVAL", "90s")
	t.Setenv("ACCOUNTING_SNAPSHOT_BATCH", "7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SnapshotInterval != 90*time.Second {
		t.Errorf("SnapshotInterval=%s want 90s", cfg.SnapshotInterval)
	}
	if cfg.SnapshotBatch != 7 {
		t.Errorf("SnapshotBatch=%d want 7", cfg.SnapshotBatch)
	}
}

// A MALFORMED VALUE MUST REFUSE THE BOOT, NOT FALL BACK TO THE DEFAULT.
//
// Falling back is the #229 failure mode in miniature: "5 minutes" is a
// plausible typo for "5m", and silently substituting the default would leave
// the operator believing they had configured a schedule they had not, with a
// clean-looking start and no way to tell.
func TestMalformedSnapshotSettingsRefuseToLoad(t *testing.T) {
	for name, env := range map[string]struct{ key, val string }{
		"interval with a space": {"ACCOUNTING_SNAPSHOT_INTERVAL", "5 minutes"},
		"interval not a number": {"ACCOUNTING_SNAPSHOT_INTERVAL", "soon"},
		"batch not a number":    {"ACCOUNTING_SNAPSHOT_BATCH", "lots"},
		"batch with a suffix":   {"ACCOUNTING_SNAPSHOT_BATCH", "64k"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ACCOUNTING_SNAPSHOT_INTERVAL", "")
			t.Setenv("ACCOUNTING_SNAPSHOT_BATCH", "")
			t.Setenv(env.key, env.val)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %s=%q — a typo must not silently become the default",
					env.key, env.val)
			}
			// The message must name the variable and the value, or an operator
			// reading a crash-looping pod's last line cannot act on it.
			if !strings.Contains(err.Error(), env.key) || !strings.Contains(err.Error(), env.val) {
				t.Errorf("error %q names neither the variable nor the value", err)
			}
		})
	}
}
