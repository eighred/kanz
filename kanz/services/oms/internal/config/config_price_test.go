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
