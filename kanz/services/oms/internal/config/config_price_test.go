package config_test

import (
	"testing"
	"time"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
)

func TestPriceDefaults(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PriceSubject != "market.>" {
		t.Errorf("PriceSubject = %q, want \"market.>\"", cfg.PriceSubject)
	}
	if cfg.PriceMaxAge != 30*time.Second {
		t.Errorf("PriceMaxAge = %v, want 30s", cfg.PriceMaxAge)
	}
}

func TestPriceMaxAgeIsConfigurable(t *testing.T) {
	t.Setenv("OMS_PRICE_MAX_AGE", "5s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PriceMaxAge != 5*time.Second {
		t.Errorf("PriceMaxAge = %v, want 5s", cfg.PriceMaxAge)
	}
}

// An unparseable duration must not silently become zero — zero means "never
// expire" to the mark source, so a typo would disable the staleness bound
// entirely and admit orders against arbitrarily old prices.
func TestUnparseableMaxAgeIsAnError(t *testing.T) {
	t.Setenv("OMS_PRICE_MAX_AGE", "half a minute")
	if _, err := config.Load(); err == nil {
		t.Fatal("Load returned nil error for an unparseable OMS_PRICE_MAX_AGE — " +
			"a typo would silently disable the staleness bound")
	}
}

// A negative duration would disable the staleness bound (maxAge <= 0 means never
// expire in the mark source), admitting orders against arbitrarily old prices.
func TestNegativeMaxAgeIsAnError(t *testing.T) {
	t.Setenv("OMS_PRICE_MAX_AGE", "-5s")
	if _, err := config.Load(); err == nil {
		t.Fatal("Load returned nil error for a negative OMS_PRICE_MAX_AGE — " +
			"a negative value would disable the staleness bound")
	}
}

// A zero duration would disable the staleness bound (maxAge <= 0 means never
// expire in the mark source), admitting orders against arbitrarily old prices.
func TestZeroMaxAgeIsAnError(t *testing.T) {
	t.Setenv("OMS_PRICE_MAX_AGE", "0s")
	if _, err := config.Load(); err == nil {
		t.Fatal("Load returned nil error for zero OMS_PRICE_MAX_AGE — " +
			"a zero value would disable the staleness bound")
	}
}

// Positive durations still load correctly, and unset still yields the 30s default.
func TestValidMaxAgeAndDefault(t *testing.T) {
	// Test that unset yields default
	cfg1, err := config.Load()
	if err != nil {
		t.Fatalf("Load with unset OMS_PRICE_MAX_AGE: %v", err)
	}
	if cfg1.PriceMaxAge != 30*time.Second {
		t.Errorf("unset PriceMaxAge = %v, want 30s", cfg1.PriceMaxAge)
	}

	// Test that valid positive value loads correctly
	t.Setenv("OMS_PRICE_MAX_AGE", "5s")
	cfg2, err := config.Load()
	if err != nil {
		t.Fatalf("Load with OMS_PRICE_MAX_AGE=5s: %v", err)
	}
	if cfg2.PriceMaxAge != 5*time.Second {
		t.Errorf("valid PriceMaxAge = %v, want 5s", cfg2.PriceMaxAge)
	}
}
