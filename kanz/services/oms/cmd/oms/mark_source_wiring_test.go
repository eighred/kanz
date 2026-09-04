package main

// THE TWO STALENESS BOUNDS, EXERCISED WHERE THEY ARE ACTUALLY WIRED (#956).
//
// internal/marketdata/mark proves that a Source given two bounds refuses a width
// before it refuses a mark. That is a statement about the fold. It says nothing
// about whether THIS process hands the fold the right two numbers, and a bound
// wired from the wrong config field is invisible to every signal the estate has:
// no crash, no refusal, no metric — just a spread cost measured against a market
// that was thirty seconds gone, reported to four decimal places.
//
// So this reads the numbers back through the builder run() actually calls, with
// a fake clock, folding real market events.

import (
	"testing"
	"time"

	"github.com/eighred/kanz/services/oms/internal/config"
)

// TestTheQuoteBoundIsWiredFromItsOwnConfigField folds one quote, ages the clock
// past OMS_QUOTE_MAX_AGE and not past OMS_PRICE_MAX_AGE, and asserts the two
// accessors disagree.
//
// THE TWO CONFIG VALUES ARE DELIBERATELY FAR APART (2s and 60s), and neither is
// a shipped default. A builder that passed PriceMaxAge to both — the behaviour
// before this change — passes every other test in this package and fails here.
// So does one that passed QuoteMaxAge to both, which is the accident that would
// turn a measurement fix into PRICE_UNAVAILABLE refusals on the order path.
func TestTheQuoteBoundIsWiredFromItsOwnConfigField(t *testing.T) {
	now := coverageBase
	marks := newMarkSource(func() time.Time { return now }, config.Config{
		PriceMaxAge: 60 * time.Second,
		QuoteMaxAge: 2 * time.Second,
	})

	foldQuoteEvent(t, marks, "BTC-USD", 59990, 60010, coverageBase)

	// Past the quote bound, inside the price bound.
	now = coverageBase.Add(10 * time.Second)

	if marks.Mark("BTC-USD") == nil {
		t.Fatal("Mark refused a 10s-old mark under a 60s OMS_PRICE_MAX_AGE — the OMS is wiring " +
			"the quote bound onto the price map, and every MARKET and STOP order whose mark is " +
			"older than OMS_QUOTE_MAX_AGE is now refused PRICE_UNAVAILABLE")
	}
	if _, _, _, ok := marks.Touch("BTC-USD"); ok {
		t.Fatal("Touch answered for a width 10s past a 2s OMS_QUOTE_MAX_AGE — the OMS is still " +
			"wiring one bound onto both maps, so the spread leg of every attribution is " +
			"measured under the mark's tolerance rather than the width's")
	}

	// And past both: the mark must go too, or the price bound is not wired either.
	now = coverageBase.Add(61 * time.Second)
	if marks.Mark("BTC-USD") != nil {
		t.Fatal("Mark answered past OMS_PRICE_MAX_AGE — the price bound is not reaching the fold")
	}
}

// THE SHIPPED PAIR, READ BACK FROM THE REAL DEFAULTS. The case above proves the
// two fields reach two maps; this proves the numbers a pod boots with are the
// ones the manifest reasons about, so a change to either default fails here
// rather than in a cost report nobody can explain.
//
// 6s IS SIX MISSED PUBLISHES against market-ingest's 1s snapshot ticker, the same
// six-observation outage tolerance the mark's 30s buys against a 5s ticker poll.
// The derivation, and the measured width ages it is checked against, are in
// infra/deploy/oms-deploy.yaml beside both values.
func TestTheShippedBoundsAreSixMissedPublishesAndSixMissedPolls(t *testing.T) {
	t.Setenv("OMS_DATABASE_URL", "postgres://u:p@localhost:5432/kanz")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.PriceMaxAge != 30*time.Second {
		t.Fatalf("PriceMaxAge = %v, want 30s", cfg.PriceMaxAge)
	}
	if cfg.QuoteMaxAge != 6*time.Second {
		t.Fatalf("QuoteMaxAge = %v, want 6s — six publishes of market-ingest's 1s snapshot "+
			"ticker, the same tolerance the mark's 30s buys against a 5s ticker poll. If this "+
			"moved, the derivation in infra/deploy/oms-deploy.yaml moved with it or is now wrong",
			cfg.QuoteMaxAge)
	}
	if cfg.QuoteMaxAge >= cfg.PriceMaxAge {
		t.Fatalf("QuoteMaxAge %v is not tighter than PriceMaxAge %v — the width is back under "+
			"the mark's tolerance and #956 is undone", cfg.QuoteMaxAge, cfg.PriceMaxAge)
	}
}
