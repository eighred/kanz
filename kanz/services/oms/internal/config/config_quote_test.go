package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/oms/internal/config"
)

// THE QUOTED WIDTH'S OWN STALENESS BOUND (#956).
//
// OMS_QUOTE_MAX_AGE is a SECOND bound, not a rename of OMS_PRICE_MAX_AGE, and
// the cases here are the ones that would let it quietly become the first again:
// a wrong default, a value that is not tighter than the mark's, a value that
// drags the mark's bound with it, and the three routes to a non-positive number
// that mark.Source reads as NEVER EXPIRES.

func TestQuoteMaxAgeDefaultsToSixMissedPublishes(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.QuoteMaxAge != 6*time.Second {
		t.Errorf("QuoteMaxAge = %v, want 6s — six publishes of market-ingest's 1s snapshot "+
			"ticker, the same six-observation tolerance PriceMaxAge's 30s buys against the "+
			"venue adapters' 5s ticker poll", cfg.QuoteMaxAge)
	}
	if cfg.QuoteMaxAge >= cfg.PriceMaxAge {
		t.Errorf("QuoteMaxAge %v is not tighter than PriceMaxAge %v — a width is back under the "+
			"mark's tolerance, which is thirty missed publishes against the mark's six",
			cfg.QuoteMaxAge, cfg.PriceMaxAge)
	}
}

func TestQuoteMaxAgeIsConfigurableWithoutMovingTheMarksBound(t *testing.T) {
	t.Setenv("OMS_QUOTE_MAX_AGE", "2s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.QuoteMaxAge != 2*time.Second {
		t.Errorf("QuoteMaxAge = %v, want 2s", cfg.QuoteMaxAge)
	}
	if cfg.PriceMaxAge != 30*time.Second {
		t.Errorf("PriceMaxAge = %v, want the untouched 30s default — OMS_QUOTE_MAX_AGE moved "+
			"the mark's bound as well, which is the PRICE_UNAVAILABLE direction this whole "+
			"change exists to avoid", cfg.PriceMaxAge)
	}
}

// Non-positive and unparseable are refused for exactly the reason the mark's
// bound refuses them: mark.Source reads a non-positive bound as never expires,
// so a typo — or an operator reaching for an off switch — would silently let an
// execution report measure a spread against an arbitrarily old market.
func TestANonPositiveQuoteMaxAgeIsAnError(t *testing.T) {
	for _, v := range []string{"0s", "-5s", "two seconds"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("OMS_QUOTE_MAX_AGE", v)
			_, err := config.Load()
			if err == nil {
				t.Fatalf("Load returned nil error for OMS_QUOTE_MAX_AGE=%q — the quoted "+
					"width's staleness bound is disabled and nothing says so", v)
			}
			if !strings.Contains(err.Error(), "OMS_QUOTE_MAX_AGE") {
				t.Fatalf("error %q does not name OMS_QUOTE_MAX_AGE, so an operator cannot "+
					"tell which value they got wrong", err)
			}
		})
	}
}
