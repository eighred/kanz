package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/oms/internal/config"
)

func TestPriceDefaults(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantSubjects := []string{"market.*.trade", "market.*.quote"}
	if len(cfg.PriceSubjects) != len(wantSubjects) {
		t.Errorf("PriceSubjects = %v, want %v", cfg.PriceSubjects, wantSubjects)
	} else {
		for i := range wantSubjects {
			if cfg.PriceSubjects[i] != wantSubjects[i] {
				t.Errorf("PriceSubjects = %v, want %v", cfg.PriceSubjects, wantSubjects)
				break
			}
		}
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

func TestPriceSubjectsDefaultToTheFoldsConsumptionSet(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"market.*.trade", "market.*.quote"}
	if len(cfg.PriceSubjects) != len(want) {
		t.Fatalf("PriceSubjects = %v, want %v", cfg.PriceSubjects, want)
	}
	for i := range want {
		if cfg.PriceSubjects[i] != want[i] {
			t.Fatalf("PriceSubjects = %v, want %v", cfg.PriceSubjects, want)
		}
	}
}

// The default must NOT be a bare market.> wildcard. That subject carries
// market.book.snapshot — an OrderBookSnapshot, not a MarketDataEvent — which
// the fold unmarshals as the wrong type and discards. It is the highest-volume
// stream in the estate, and folding it was pure waste.
func TestPriceSubjectsDoNotIncludeTheBookSpine(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range cfg.PriceSubjects {
		if s == "market.>" || strings.HasPrefix(s, "market.book") {
			t.Fatalf("PriceSubjects contains %q, which delivers OrderBookSnapshot messages the "+
				"mark fold cannot use — the OMS would decode and discard the estate's "+
				"highest-volume stream", s)
		}
	}
}

func TestPriceSubjectsAreConfigurable(t *testing.T) {
	t.Setenv("OMS_PRICE_SUBJECTS", "market.crypto.trade, market.crypto.quote")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"market.crypto.trade", "market.crypto.quote"}
	if len(cfg.PriceSubjects) != 2 || cfg.PriceSubjects[0] != want[0] || cfg.PriceSubjects[1] != want[1] {
		t.Fatalf("PriceSubjects = %v, want %v (whitespace around commas must be trimmed)",
			cfg.PriceSubjects, want)
	}
}

// An empty or whitespace-only override must be an ERROR, not silently zero
// subjects. A pod that subscribes to nothing folds no marks, and every
// MARKET/STOP order is then refused PRICE_UNAVAILABLE forever — a trading
// outage that looks like a quiet config typo.
func TestEmptyPriceSubjectsIsAnError(t *testing.T) {
	for _, v := range []string{" ", ",", " , "} {
		t.Setenv("OMS_PRICE_SUBJECTS", v)
		if _, err := config.Load(); err == nil {
			t.Fatalf("Load accepted OMS_PRICE_SUBJECTS=%q, which subscribes to nothing and "+
				"refuses every market order forever", v)
		}
	}
}
