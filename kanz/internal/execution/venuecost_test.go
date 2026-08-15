package execution

import (
	"fmt"
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

// observeN records n observations of one venue at the same cost, each from a
// DIFFERENT order.
//
// The distinct decision id per iteration is what these tests have always meant —
// "this venue has been measured n independent times" — and since #483 the ranker
// reads it that way rather than counting fills. Equal notional keeps the
// weighted mean equal to the plain mean, so every assertion below is about
// ranking rather than about weighting.
func observeN(v *VenueCosts, venue string, bps float64, n int) {
	for i := 0; i < n; i++ {
		v.Observe(venue, bps, 1000, fmt.Sprintf("%s-order-%d", venue, i))
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

// ===== ONE DECISION IS ONE DECISION, HOWEVER MANY SLICES WORKED IT (#483) =====

// A PARENT WORKED IN MANY SLICES DOES NOT CLEAR THE EVIDENCE FLOOR.
//
// This is #483's assertion. Once #435 let one order be worked as fifty child
// orders, fifty fills arrived where one used to — and a floor counted in FILLS
// was cleared by a SINGLE decision. The ranker would then start overriding the
// operator's declared default on the evidence of one order, and the venue it
// picked is where the next order goes, so the error feeds itself.
func TestVenueCosts_ManySlicesOfOneOrderDoNotClearTheFloor(t *testing.T) {
	costs := costsAt(t, costNow) // floor of 3 decisions

	// Fifty fills, all slices of ONE parent order.
	for i := 0; i < 50; i++ {
		costs.Observe("OKX", 5, 100, "parent-1")
	}
	observeN(costs, "BINANCE", 40, 5)

	if mic, ok := costs.Preferred([]string{"BINANCE", "OKX"}); ok {
		t.Fatalf("preferred %q on fifty slices of ONE order — that is one observation of this "+
			"venue under one set of market conditions, and the floor exists precisely to stop "+
			"the ranker acting on it", mic)
	}

	// Two more genuine decisions clear it.
	costs.Observe("OKX", 5, 100, "parent-2")
	costs.Observe("OKX", 5, 100, "order-3")
	if mic, ok := costs.Preferred([]string{"BINANCE", "OKX"}); !ok || mic != "OKX" {
		t.Fatalf("got (%q, %v) after three distinct decisions, want OKX — the floor counts "+
			"decisions, and three of them is what it asks for", mic, ok)
	}
}

// THE MEAN IS NOTIONAL-WEIGHTED, so a decision worked in slices scores exactly
// what the same decision sent whole would have scored.
//
// Under an unweighted mean the two halves of this test disagree: the sliced
// venue's cheap slices outvote its expensive one fifty to one. Weighted, the
// money decides, which is what "this venue cost us more" has to mean.
func TestVenueCosts_TheMeanIsWeightedByNotionalNotByFillCount(t *testing.T) {
	whole := costsAt(t, costNow, WithMinSamples(1))
	// One decision: 100 notional at 10 bps, 900 notional at 0 bps.
	whole.Observe("V", 10, 100, "d1")
	whole.Observe("V", 0, 900, "d1")

	sliced := costsAt(t, costNow, WithMinSamples(1))
	// The SAME money, worked as many small pieces: 1 unit at 10 bps ×100, then
	// 9 units at 0 bps ×100.
	for i := 0; i < 100; i++ {
		sliced.Observe("V", 10, 1, "d1")
	}
	for i := 0; i < 100; i++ {
		sliced.Observe("V", 0, 9, "d1")
	}

	a := meanOf(t, whole, "V")
	b := meanOf(t, sliced, "V")
	if a != b {
		t.Fatalf("whole=%v sliced=%v — the same money at the same prices scored differently "+
			"because it was worked in more pieces", a, b)
	}
	// 1000 bps·notional over 1000 notional = 1 bps. An UNWEIGHTED mean of the
	// whole case is 5; of the sliced case, also 5 — so the assertion above alone
	// would pass on the broken implementation. This is the one that pins it.
	if a != 1 {
		t.Fatalf("weighted mean = %v, want 1 bps (100×10 + 900×0, over 1000 notional). An "+
			"unweighted mean gives 5, which is the cost of the small fill mistaken for the "+
			"cost of the order", a)
	}
}

// A FILL NOBODY COULD SIZE IS DROPPED, not counted as zero weight. Letting it
// through would add it to the evidence floor while contributing nothing to the
// mean — evidence for a number it had no part in.
func TestVenueCosts_AnUnsizedFillIsNotEvidence(t *testing.T) {
	// A FLOOR OF 2, AND ONE REAL FILL. If the unsized ones counted, this venue
	// would reach three decisions and clear the floor on the strength of a single
	// measurement — which is the harm: they add to the EVIDENCE while
	// contributing nothing to the number that evidence is about.
	//
	// Asserting "no summary" instead would prove nothing: a zero-weight fill
	// leaves the weight at zero either way, so the summary is absent whether or
	// not the guard is there.
	costs := costsAt(t, costNow, WithMinSamples(2))
	costs.Observe("V", 10, 0, "unsized-1")
	costs.Observe("V", 10, -5, "unsized-2")
	costs.Observe("V", 10, 100, "real-1")
	observeN(costs, "W", 40, 5)

	if s := summaryOf(t, costs, "V"); s == nil {
		t.Fatal("the real fill was dropped along with the unsized ones")
	} else if s.Decisions != 1 {
		t.Fatalf("Decisions = %d, want 1 — two fills nobody could size were counted as evidence "+
			"for a mean they contributed nothing to", s.Decisions)
	}
	if mic, ok := costs.Preferred([]string{"V", "W"}); ok {
		t.Fatalf("preferred %q on one real measurement plus two unsized ones", mic)
	}
}

// AN UNATTRIBUTABLE MEASUREMENT IS NOT EVIDENCE EITHER. Treating "" as a
// decision would collapse every such fill into one permanent sample, which is
// worse than ignoring them: it would sit one short of the floor forever, or
// clear a floor of one on the first orphan.
func TestVenueCosts_AnUnattributedFillIsNotADecision(t *testing.T) {
	// A FLOOR OF 1, AND A MEASURED COMPETITOR. Both matter: with the floor at 1,
	// collapsing every orphan into the single key "" clears it outright; and W
	// must have real evidence, or Preferred abstains for the unrelated reason
	// that one candidate is unmeasured — which would make this pass whatever V did.
	costs := costsAt(t, costNow, WithMinSamples(1))
	for i := 0; i < 10; i++ {
		costs.Observe("V", 10, 100, "") // cheap, and unattributable
	}
	observeN(costs, "W", 40, 3) // expensive, and real

	if mic, ok := costs.Preferred([]string{"V", "W"}); ok {
		t.Fatalf("preferred %q on ten unattributed fills — treating \"\" as one decision "+
			"clears a floor of one on the first orphan, and every orphan after it collapses "+
			"into the same permanent sample", mic)
	}
	// They still WEIGH, because the money really did move — they are simply not
	// evidence about how many times this venue has been tried.
	if s := summaryOf(t, costs, "V"); s == nil || s.Samples != 10 {
		t.Errorf("unattributed fills were dropped entirely: %+v — the money moved and the "+
			"measurement is real, it just cannot be counted as a separate decision", s)
	}
}

// THE DECISION SET IS BOUNDED BY THE FLOOR, which is what makes holding one
// affordable at all. Past the floor no further id can change the verdict, so
// nothing more is stored — an unbounded set would grow for every order a busy
// venue sees inside the six-hour window.
func TestVenueCosts_TheDecisionSetStopsGrowingAtTheFloor(t *testing.T) {
	costs := costsAt(t, costNow) // floor of 3
	for i := 0; i < 500; i++ {
		costs.Observe("V", 10, 100, fmt.Sprintf("order-%d", i))
	}
	s := summaryOf(t, costs, "V")
	if s == nil {
		t.Fatal("no summary")
	}
	if s.Decisions != 3 {
		t.Errorf("Decisions = %d after 500 distinct orders, want it to stop at the floor of 3 — "+
			"an unbounded set grows with every order the venue sees", s.Decisions)
	}
	if s.Samples != 500 {
		t.Errorf("Samples = %d, want all 500 fills counted — the gap between Samples and "+
			"Decisions is what shows evidence coming from a few heavily-sliced orders", s.Samples)
	}
}

func meanOf(t *testing.T, v *VenueCosts, venue string) float64 {
	t.Helper()
	s := summaryOf(t, v, venue)
	if s == nil {
		t.Fatalf("no measurement for %s", venue)
	}
	return s.MeanBps
}

func summaryOf(t *testing.T, v *VenueCosts, venue string) *VenueCostSummary {
	t.Helper()
	for _, s := range v.Snapshot() {
		if s.Venue == venue {
			return &s
		}
	}
	return nil
}
