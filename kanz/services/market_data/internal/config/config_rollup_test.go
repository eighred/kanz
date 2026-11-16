package config

import (
	"strings"
	"testing"
	"time"
)

// THE ROLLUP CADENCE REFUSES A MALFORMED VALUE, AND STILL REFUSES A NON-POSITIVE
// ONE (#692).
//
// market-data read both rollup durations through a local helper that folded
// `err != nil || d <= 0` into a single fall-back to the default. De-duplicating
// it onto env.Duration split those two apart: env.Duration refuses a bad parse,
// and the positivity check moved to the call site, where the two mistakes get
// two different messages.
//
// WHY THIS SERIES IS THE ONE TO PIN: the rollup folds 1-minute bars into 1h and
// 1d, the store is append-only, and a bar written at a knowledge time is
// IMMUTABLE — a re-run corrects it only by writing a restatement beside the
// wrong one. A cadence nobody chose is therefore not a latency bug here, it is
// durable wrong data.

func TestLoadRefusesAMalformedRollupInterval(t *testing.T) {
	for _, key := range []string{
		"MARKET_DATA_ROLLUP_INTERVAL",
		"MARKET_DATA_ROLLUP_WATERMARK_LAG",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "1hour")
			_, err := Load()
			if err == nil {
				t.Fatalf("%s=1hour was accepted; the rollup runs on a default nobody chose", key)
			}
			for _, want := range []string{key, "1hour"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

// A NON-POSITIVE VALUE IS A DIFFERENT MISTAKE AND GETS A DIFFERENT MESSAGE. The
// old helper could not tell these apart; this asserts the split survived, not
// merely that both are refused.
func TestLoadRefusesANonPositiveRollupInterval(t *testing.T) {
	t.Setenv("MARKET_DATA_ROLLUP_INTERVAL", "0s")
	_, err := Load()
	if err == nil {
		t.Fatal("a zero cadence was accepted — the rollup ticker would panic on it")
	}
	if !strings.Contains(err.Error(), "positive cadence") {
		t.Errorf("a well-formed zero reports the same failure as a typo, which is the fold "+
			"this change undid: %v", err)
	}

	t.Setenv("MARKET_DATA_ROLLUP_INTERVAL", "1h")
	t.Setenv("MARKET_DATA_ROLLUP_WATERMARK_LAG", "-5m")
	if _, err := Load(); err == nil {
		t.Fatal("a negative watermark lag was accepted — the rollup would fold buckets that " +
			"have not ended yet, producing bars that look finished and are short")
	}
}

// UNSET STILL LANDS ON THE DOCUMENTED DEFAULTS. The non-vacuity arm.
func TestLoadUsesTheRollupDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RollupInterval != DefaultRollupInterval {
		t.Errorf("RollupInterval = %s, want %s", cfg.RollupInterval, DefaultRollupInterval)
	}
	if cfg.RollupWatermarkLag != DefaultRollupWatermarkLag {
		t.Errorf("RollupWatermarkLag = %s, want %s", cfg.RollupWatermarkLag, DefaultRollupWatermarkLag)
	}

	t.Setenv("MARKET_DATA_ROLLUP_INTERVAL", "15m")
	t.Setenv("MARKET_DATA_ROLLUP_WATERMARK_LAG", "90s")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RollupInterval != 15*time.Minute || cfg.RollupWatermarkLag != 90*time.Second {
		t.Errorf("got %s / %s, want 15m / 90s", cfg.RollupInterval, cfg.RollupWatermarkLag)
	}
}
