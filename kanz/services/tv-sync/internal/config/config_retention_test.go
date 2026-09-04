package config_test

// THE RESIDENT-HISTORY WINDOW IS REFUSED, NEVER DEFAULTED (#809).
//
// Two separate consequences hang off this one number, and they fail in opposite
// directions:
//
//   - TOO SMALL (or absent as an "off switch") is a DOUBLE-COUNTED FILL. The
//     window is also the fill-id dedup window for the dual fill path — the
//     synchronous venue response and the asynchronous websocket echo of the same
//     fill, two FACTs with two event_ids that tv_facts's primary key does not
//     deduplicate. An id dropped before its echo arrives doubles a position the
//     fund does not hold.
//   - NON-POSITIVE is the leak #809 filed: the heap grows with lifetime order and
//     fill volume until the pod is OOM-killed, and the only symptom is the pod
//     dying.
//
// Both are silent, so both are refused at startup rather than defaulted into.

import (
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/tv-sync/internal/config"
)

func TestRetentionDefaultsToAWeek(t *testing.T) {
	requireBook(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retention != 168*time.Hour {
		t.Fatalf("Retention = %s, want 168h", cfg.Retention)
	}
	if cfg.Retention < config.MinRetention {
		t.Fatalf("the DEFAULT is below the floor (%s < %s) — the shipped configuration would be "+
			"the one that double-counts an echoed fill", cfg.Retention, config.MinRetention)
	}
}

func TestANonPositiveOrTooShortRetentionRefusesTheBoot(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"zero is not an off switch", "0s"},
		{"negative", "-1h"},
		{"below the dedup floor", "1h"},
		{"just below the dedup floor", "23h59m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireBook(t)
			t.Setenv("TV_SYNC_RETENTION", tc.value)
			_, err := config.Load()
			if err == nil {
				t.Fatalf("TV_SYNC_RETENTION=%s started the service. A window below %s drops a fill "+
					"id before its websocket echo can arrive, which folds the fill twice and doubles "+
					"a position the fund does not hold; a non-positive one is the unbounded heap "+
					"#809 exists to end (#809)", tc.value, config.MinRetention)
			}
			if !strings.Contains(err.Error(), "TV_SYNC_RETENTION") {
				t.Fatalf("the refusal does not name the variable an operator has to change: %v", err)
			}
		})
	}
}

func TestAnUnparseableRetentionRefusesTheBoot(t *testing.T) {
	requireBook(t)
	t.Setenv("TV_SYNC_RETENTION", "one week")
	if _, err := config.Load(); err == nil {
		t.Fatal("an unparseable TV_SYNC_RETENTION started the service — a typo would silently fall " +
			"back to a default the operator did not choose")
	}
}

func TestAnExplicitRetentionIsHonoured(t *testing.T) {
	requireBook(t)
	t.Setenv("TV_SYNC_RETENTION", "720h")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retention != 720*time.Hour {
		t.Fatalf("Retention = %s, want 720h — the override was ignored, so every deployment runs the "+
			"default window whatever its manifest says", cfg.Retention)
	}
}
