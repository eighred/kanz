package execution

import (
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// VENUE RANKING ON MEASURED COST (#437 option B).
//
// #437's "Verified when" is one sentence:
//
//	two venues both support an order, the second has a materially better
//	price/fee, and Route returns the second — i.e. the test fails against
//	venues[0].
//
// TestRouter_RanksByMeasuredCost is that test. The rest guard the ways a ranker
// can be WORSE than the config line it replaces: acting on noise, acting on
// stale data, comparing a measured venue against an unmeasured one, or quietly
// reintroducing the array index this issue exists to remove.

var costNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func costsAt(t *testing.T, now time.Time, opts ...VenueCostsOption) *VenueCosts {
	t.Helper()
	base := []VenueCostsOption{WithCostClock(func() time.Time { return now }), WithMinSamples(3)}
	return NewVenueCosts(append(base, opts...)...)
}

func observeN(v *VenueCosts, venue string, bps float64, n int) {
	for i := 0; i < n; i++ {
		v.Observe(venue, bps)
	}
}

// #437'S VERIFIED WHEN. Two venues, the SECOND materially cheaper, and an
// untargeted order reaches it — so the test fails against venues[0].
func TestRouter_RanksByMeasuredCost(t *testing.T) {
	costs := costsAt(t, costNow)
	observeN(costs, "BINANCE", 40, 5) // 40 bps: expensive
	observeN(costs, "OKX", 5, 5)      // 5 bps: materially better

	r := NewRouter(
		[]Venue{NewSimVenue("BINANCE"), NewSimVenue("OKX")},
		WithDefaultVenue("BINANCE"),
		WithCostRanker(costs),
	)

	v, err := r.Route(&orderpb.OrderState{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.MIC() != "OKX" {
		t.Fatalf("routed to %q, want OKX — it is 35 bps cheaper on measured cost. Against "+
			"venues[0], or against the declared default alone, this returns BINANCE", v.MIC())
	}
}

// A TARGETED ORDER IS NEVER RE-RANKED. The fan-out already decided where that
// leg belongs, and overriding it would move a portfolio's order onto another
// portfolio's collateral — the cross-collateralization failure the exact-match
// path exists to prevent.
func TestRouter_RankingNeverOverridesAnExplicitTarget(t *testing.T) {
	costs := costsAt(t, costNow)
	observeN(costs, "BINANCE", 400, 5) // ruinous
	observeN(costs, "OKX", 1, 5)       // excellent

	r := NewRouter([]Venue{NewSimVenue("BINANCE"), NewSimVenue("OKX")}, WithCostRanker(costs))

	v, err := r.Route(&orderpb.OrderState{Venue: "BINANCE"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.MIC() != "BINANCE" {
		t.Fatalf("a TARGETED order was re-routed to %q on cost — the allocation fan-out's "+
			"choice is not a suggestion", v.MIC())
	}
}

// TOO LITTLE EVIDENCE ⇒ ABSTAIN, and the declared default stands.
//
// This is the rule that stops the ranker being worse than the config line.
// Execution cost is noisy; two fills can rank a good venue below a bad one purely
// on which happened to catch a spread.
func TestVenueCosts_AbstainsBelowTheSampleFloor(t *testing.T) {
	costs := costsAt(t, costNow)
	observeN(costs, "BINANCE", 40, 5)
	observeN(costs, "OKX", 5, 2) // below the floor of 3

	if mic, ok := costs.Preferred([]string{"BINANCE", "OKX"}); ok {
		t.Fatalf("preferred %q on two fills — a ranker acting on noise is worse than the "+
			"config line it replaces", mic)
	}

	r := NewRouter([]Venue{NewSimVenue("BINANCE"), NewSimVenue("OKX")},
		WithDefaultVenue("BINANCE"), WithCostRanker(costs))
	v, err := r.Route(&orderpb.OrderState{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.MIC() != "BINANCE" {
		t.Errorf("an abstaining ranker did not fall back to the DECLARED default: got %q", v.MIC())
	}
}

// AN UNMEASURED CANDIDATE BLOCKS THE COMPARISON. Ranking a venue with evidence
// against one with none does not compare them — it prefers whichever the
// platform happens to have used, which is how a default entrenches itself and
// stops being questioned.
func TestVenueCosts_AbstainsWhenACandidateHasNoEvidence(t *testing.T) {
	costs := costsAt(t, costNow)
	observeN(costs, "BINANCE", 40, 10)
	// OKX: never traded.

	if mic, ok := costs.Preferred([]string{"BINANCE", "OKX"}); ok {
		t.Fatalf("preferred %q while OKX had no measurements at all — that is not a comparison, "+
			"it is a preference for the incumbent", mic)
	}
}

// STALE EVIDENCE IS NOT EVIDENCE. A venue's cost is a property of the market and
// of that venue's queue TODAY; a week-old average would keep preferring a venue
// that has since degraded, and would do it confidently.
func TestVenueCosts_AbstainsOnStaleEvidence(t *testing.T) {
	now := costNow
	costs := NewVenueCosts(
		WithCostClock(func() time.Time { return now }),
		WithMinSamples(3), WithCostWindow(time.Hour),
	)
	observeN(costs, "BINANCE", 40, 5)
	observeN(costs, "OKX", 5, 5)

	if _, ok := costs.Preferred([]string{"BINANCE", "OKX"}); !ok {
		t.Fatal("fresh evidence was refused")
	}
	now = now.Add(2 * time.Hour)
	if mic, ok := costs.Preferred([]string{"BINANCE", "OKX"}); ok {
		t.Fatalf("preferred %q on evidence two hours past the window — a stale ranking outlives "+
			"the market it measured", mic)
	}
}

// A TIE IS AN ABSTENTION, not an alphabetical pick. Two venues with the same
// measured cost give the ranker no reason to override the operator, and choosing
// deterministically would be an array index by another name — which is the exact
// defect this issue exists to remove.
func TestVenueCosts_ATieAbstainsRatherThanPickingAnOrder(t *testing.T) {
	costs := costsAt(t, costNow)
	observeN(costs, "BINANCE", 12, 5)
	observeN(costs, "OKX", 12, 5)

	if mic, ok := costs.Preferred([]string{"BINANCE", "OKX"}); ok {
		t.Fatalf("preferred %q on identical measured cost — that is a slice order wearing a "+
			"ranking's name", mic)
	}
}

// ONE CANDIDATE IS NOT A CHOICE. Answering here would let a single-venue
// deployment report itself as ranked when nothing was compared.
func TestVenueCosts_SingleCandidateAbstains(t *testing.T) {
	costs := costsAt(t, costNow)
	observeN(costs, "BINANCE", 40, 10)

	if _, ok := costs.Preferred([]string{"BINANCE"}); ok {
		t.Fatal("a single candidate was reported as preferred — nothing was compared")
	}
}

// A RANKER NAMING A VENUE THIS ROUTER DOES NOT HOLD IS IGNORED, not fatal. A
// stale or mis-scoped ranking must not take down untargeted routing; the declared
// default is still correct.
func TestRouter_IgnoresARankerNamingAnUnknownVenue(t *testing.T) {
	r := NewRouter([]Venue{NewSimVenue("BINANCE")},
		WithDefaultVenue("BINANCE"), WithCostRanker(fixedRanker{"KRAKEN"}))

	v, err := r.Route(&orderpb.OrderState{})
	if err != nil {
		t.Fatalf("a ranker naming an unheld venue broke routing: %v", err)
	}
	if v.MIC() != "BINANCE" {
		t.Errorf("routed to %q, want the declared default BINANCE", v.MIC())
	}
}

// NO RANKER AT ALL IS THE SHIPPED BEHAVIOUR, unchanged.
func TestRouter_WithoutARankerIsUnchanged(t *testing.T) {
	r := NewRouter([]Venue{NewSimVenue("BINANCE"), NewSimVenue("OKX")}, WithDefaultVenue("OKX"))
	v, err := r.Route(&orderpb.OrderState{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.MIC() != "OKX" {
		t.Errorf("routed to %q, want the declared default OKX", v.MIC())
	}
}

type fixedRanker struct{ mic string }

func (f fixedRanker) Preferred([]string) (string, bool) { return f.mic, true }
