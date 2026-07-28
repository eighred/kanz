package config_test

import (
	"strings"
	"testing"

	"github.com/eighred/kanz/services/tv-sync/internal/config"
)

// requireBook sets the two env vars validateBook demands, so these tests
// exercise PriceSubjects without tripping over the durable-book requirement
// (EXEC-M21), which is unrelated to what they check.
func requireBook(t *testing.T) {
	t.Helper()
	t.Setenv("TV_SYNC_DATABASE_URL", "postgres://localhost/tv_sync_test")
	t.Setenv("TV_SYNC_TENANT", "test-tenant")
}

func TestPriceSubjectsDefaultToTheFoldsConsumptionSet(t *testing.T) {
	requireBook(t)
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
// the fold unmarshals as the wrong type. A bids-only snapshot decodes as a
// Trade at the deepest resting bid (the two messages are wire-compatible by
// construction) and would poison tv-sync's unrealized P&L with a mark below
// mid. It is also the highest-volume stream in the estate.
func TestPriceSubjectsDoNotIncludeTheBookSpine(t *testing.T) {
	requireBook(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range cfg.PriceSubjects {
		if s == "market.>" || strings.HasPrefix(s, "market.book") {
			t.Fatalf("PriceSubjects contains %q, which delivers OrderBookSnapshot messages the "+
				"mark fold cannot use — a bids-only snapshot would poison unrealized P&L with a "+
				"resting bid price", s)
		}
	}
}

func TestPriceSubjectsAreConfigurable(t *testing.T) {
	requireBook(t)
	t.Setenv("TV_SYNC_PRICE_SUBJECTS", "market.crypto.trade, market.crypto.quote")
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
// unrealized P&L figure silently goes missing — an outage that looks like a
// quiet config typo.
func TestEmptyPriceSubjectsIsAnError(t *testing.T) {
	requireBook(t)
	for _, v := range []string{" ", ",", " , "} {
		t.Setenv("TV_SYNC_PRICE_SUBJECTS", v)
		if _, err := config.Load(); err == nil {
			t.Fatalf("Load accepted TV_SYNC_PRICE_SUBJECTS=%q, which subscribes to nothing and "+
				"silently shows no unrealized P&L", v)
		}
	}
}
