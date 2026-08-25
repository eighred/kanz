package execution

import (
	"fmt"
	"strings"
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

// ===== AN UNTARGETED ORDER GOES WHERE IT CAN ACTUALLY BE PLACED (#405) =====
//
// Admission refuses a TARGETED order whose named venue cannot place its type. An
// untargeted one had no such check: cost ranking and the declared default both
// choose a destination without asking whether the order can be placed there at
// all. In a two-venue deployment where only one supports stops, a stop-loss was
// admitted, stored, announced — and then routed, half the time, to the adapter
// that refuses it.

// typed wraps a venue with a declared order-type set, as the real connectors do.
func typed(mic string, types ...orderpb.OrderType) Venue {
	return WithOrderTypes(NewSimVenue(mic), types)
}

var spotOnly = []orderpb.OrderType{
	orderpb.OrderType_ORDER_TYPE_MARKET,
	orderpb.OrderType_ORDER_TYPE_LIMIT,
}

var withStops = []orderpb.OrderType{
	orderpb.OrderType_ORDER_TYPE_MARKET,
	orderpb.OrderType_ORDER_TYPE_LIMIT,
	orderpb.OrderType_ORDER_TYPE_STOP,
	orderpb.OrderType_ORDER_TYPE_STOP_LIMIT,
}

func TestRouter_AnUntargetedStopGoesToTheVenueThatCanPlaceIt(t *testing.T) {
	// The DEFAULT cannot place a stop; the other venue can. There is no choice
	// left to make, so the order belongs there rather than being refused.
	r := NewRouter(
		[]Venue{typed("OKX", spotOnly...), typed("BINANCE", withStops...)},
		WithDefaultVenue("OKX"),
	)

	v, err := r.Route(&orderpb.OrderState{OrderType: orderpb.OrderType_ORDER_TYPE_STOP})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.MIC() != "BINANCE" {
		t.Fatalf("routed a stop to %q, which cannot place one — it would be admitted, stored, "+
			"announced, and then refused at the exchange, which is the defect #405 exists to end",
			v.MIC())
	}
	// AND AN ORDINARY ORDER STILL GOES TO THE DECLARED DEFAULT. A filter that
	// changed untargeted routing for types every venue supports would be a
	// behaviour change wearing a bug fix's name.
	v, err = r.Route(&orderpb.OrderState{OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.MIC() != "OKX" {
		t.Errorf("an ordinary limit went to %q, want the declared default OKX", v.MIC())
	}
}

// THE COST RANKER IS ONLY OFFERED VENUES THAT CAN PLACE THE ORDER. Otherwise the
// cheapest venue wins a stop it cannot express.
func TestRouter_RankingNeverPrefersAVenueThatCannotPlaceTheType(t *testing.T) {
	costs := costsAt(t, costNow)
	observeN(costs, "OKX", 1, 5)       // by far the cheapest…
	observeN(costs, "BINANCE", 400, 5) // …and ruinous

	r := NewRouter(
		[]Venue{typed("OKX", spotOnly...), typed("BINANCE", withStops...)},
		WithDefaultVenue("BINANCE"), WithCostRanker(costs),
	)

	v, err := r.Route(&orderpb.OrderState{OrderType: orderpb.OrderType_ORDER_TYPE_STOP_LIMIT})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.MIC() != "BINANCE" {
		t.Fatalf("cost ranking sent a stop-limit to %q, which cannot place one — cheapest is "+
			"not a reason to send an order somewhere it cannot go", v.MIC())
	}
}

// NO VENUE CAN PLACE IT ⇒ A REFUSAL THAT NAMES THE TYPE, rather than routing to
// one that will fail.
func TestRouter_RefusesWhenNoVenueCanPlaceTheType(t *testing.T) {
	r := NewRouter([]Venue{typed("OKX", spotOnly...)}, WithDefaultVenue("OKX"))

	_, err := r.Route(&orderpb.OrderState{OrderType: orderpb.OrderType_ORDER_TYPE_STOP})
	if err == nil {
		t.Fatal("a stop was routed to a deployment where no venue can place one")
	}
	if !strings.Contains(err.Error(), "ORDER_TYPE_STOP") {
		t.Errorf("error = %q, want it to name the type an operator has to change", err)
	}
}

// SEVERAL COULD PLACE IT AND THE DEFAULT COULD NOT ⇒ REFUSE, rather than pick.
// Choosing among them would be the array index #437 removed, wearing a narrower
// disguise: the operator named a default that does not apply here, so the
// destination is genuinely unchosen.
func TestRouter_RefusesRatherThanPickingAmongSeveralCapableVenues(t *testing.T) {
	r := NewRouter(
		[]Venue{typed("OKX", spotOnly...), typed("BINANCE", withStops...), typed("KRAKEN", withStops...)},
		WithDefaultVenue("OKX"),
	)

	_, err := r.Route(&orderpb.OrderState{OrderType: orderpb.OrderType_ORDER_TYPE_STOP})
	if err == nil {
		t.Fatal("the router picked among two capable venues with no stated preference — that is " +
			"a destination chosen by slice position, which is exactly what #437 removed")
	}
	if !strings.Contains(err.Error(), "BINANCE") || !strings.Contains(err.Error(), "KRAKEN") {
		t.Errorf("error = %q, want it to name the venues that CAN place it so the operator can "+
			"choose one", err)
	}
}

// A VENUE THAT DECLARES NOTHING IS STILL A CANDIDATE. "Did not say" is permissive
// everywhere else in this router, and a deployment whose adapters predate the
// declaration must keep working rather than silently losing every venue.
func TestRouter_AnUndeclaringVenueIsStillRoutable(t *testing.T) {
	r := NewRouter([]Venue{NewSimVenue("SIM")}, WithDefaultVenue("SIM"))

	v, err := r.Route(&orderpb.OrderState{OrderType: orderpb.OrderType_ORDER_TYPE_STOP})
	if err != nil {
		t.Fatalf("an undeclaring venue was filtered out: %v — every deployment whose adapters "+
			"predate the declaration would stop routing entirely", err)
	}
	if v.MIC() != "SIM" {
		t.Errorf("routed to %q, want SIM", v.MIC())
	}
}

// AN UNTARGETED ORDER IS NARROWED BY TIME-IN-FORCE TOO (#486).
//
// Admission refuses a TARGETED order whose venue cannot express its
// time-in-force. The untargeted path chooses a destination by cost or by the
// declared default, neither of which asks whether the instruction can be honoured
// there — so an IOC would go, half the time, to the adapter that refuses it.
//
// Same gap as the order-type one this mirrors, and it matters more: an IOC that
// the connector refuses is at least loud, but the whole reason the connectors now
// refuse is that sending it as good-til-cancelled RESTS an order the trader asked
// not to hold.
func typedTIF(mic string, tifs ...orderpb.TimeInForce) Venue {
	return WithTimeInForce(NewSimVenue(mic), tifs)
}

func TestRouter_AnUntargetedOrderGoesToAVenueThatCanExpressItsTimeInForce(t *testing.T) {
	// The DEFAULT cannot express IOC; the other venue can.
	r := NewRouter(
		[]Venue{
			typedTIF("OKX", orderpb.TimeInForce_TIME_IN_FORCE_GTC),
			typedTIF("BINANCE", orderpb.TimeInForce_TIME_IN_FORCE_GTC, orderpb.TimeInForce_TIME_IN_FORCE_IOC),
		},
		WithDefaultVenue("OKX"),
	)

	v, err := r.Route(&orderpb.OrderState{
		OrderType:   orderpb.OrderType_ORDER_TYPE_LIMIT,
		TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_IOC,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.MIC() != "BINANCE" {
		t.Fatalf("routed an IOC to %q, which cannot express it — it would be admitted, announced, "+
			"and then refused by the connector", v.MIC())
	}

	// AND A GTC STILL GOES TO THE DECLARED DEFAULT. A filter that changed
	// untargeted routing for instructions every venue supports would be a
	// behaviour change wearing a bug fix's name.
	v, err = r.Route(&orderpb.OrderState{
		OrderType:   orderpb.OrderType_ORDER_TYPE_LIMIT,
		TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.MIC() != "OKX" {
		t.Errorf("a GTC went to %q, want the declared default OKX", v.MIC())
	}
}

// NO VENUE CAN EXPRESS IT ⇒ A REFUSAL THAT NAMES THE INSTRUCTION, rather than
// routing to one that will refuse it later.
func TestRouter_RefusesWhenNoVenueCanExpressTheTimeInForce(t *testing.T) {
	r := NewRouter([]Venue{typedTIF("OKX", orderpb.TimeInForce_TIME_IN_FORCE_GTC)},
		WithDefaultVenue("OKX"))

	_, err := r.Route(&orderpb.OrderState{
		OrderType:   orderpb.OrderType_ORDER_TYPE_LIMIT,
		TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTD,
	})
	if err == nil {
		t.Fatal("a GTD was routed to a deployment where no venue can express one")
	}
	if !strings.Contains(err.Error(), "GTD") {
		t.Errorf("error = %q, want it to name the instruction an operator has to change", err)
	}
}

// typedMargin declares which collateral regimes a venue can work, mirroring
// typed and typedTIF above.
func typedMargin(mic string, modes ...orderpb.MarginMode) Venue {
	return WithMarginModes(NewSimVenue(mic), modes)
}

// AN UNTARGETED ORDER MUST NOT BE ROUTED SOMEWHERE ITS REGIME CANNOT BE WORKED
// (#417, correcting Part 1).
//
// This is the third instance of one defect on one path. Admission refuses a
// TARGETED order whose named venue cannot work its margin mode; an untargeted
// one had no such check, and the destination was chosen by cost rank or by the
// declared default — neither of which asked whether the order could be worked
// there at all.
//
// It was the WORST of the three, because the other two end in a refusal an
// operator can see. #405's stop reached a connector that refused it; #486's IOC
// reached one that rested it. A CROSS order reaching a spot-only connector was
// placed as ORDINARY SPOT — the position is live, at the size asked for, under a
// regime nobody granted, and the audit root records the regime the trader chose.
// Nothing errors.
//
// The two halves had to land together. Until Accept carried margin_mode onto the
// OrderState, st.GetMarginMode() was UNSPECIFIED here for every order ever
// admitted, so narrowing on it would have filtered on a constant — a green test
// over a field the production path never set.
func TestRouter_AnUntargetedOrderGoesToAVenueThatCanWorkItsMarginMode(t *testing.T) {
	// The DEFAULT is spot-only; the other venue can work cross margin.
	r := NewRouter(
		[]Venue{
			typedMargin("OKX", orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED),
			typedMargin("BINANCE", orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED,
				orderpb.MarginMode_MARGIN_MODE_CROSS),
		},
		WithDefaultVenue("OKX"),
	)

	v, err := r.Route(&orderpb.OrderState{
		OrderType:  orderpb.OrderType_ORDER_TYPE_LIMIT,
		MarginMode: orderpb.MarginMode_MARGIN_MODE_CROSS,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.MIC() != "BINANCE" {
		t.Fatalf("routed a CROSS order to %q, which works spot only — it would be placed UNLEVERED "+
			"with no error anywhere, while the audit root records the regime the trader asked for",
			v.MIC())
	}

	// AND A SPOT ORDER STILL GOES TO THE DECLARED DEFAULT. A filter that changed
	// untargeted routing for orders every venue can work would be a behaviour
	// change wearing a bug fix's name.
	v, err = r.Route(&orderpb.OrderState{
		OrderType:  orderpb.OrderType_ORDER_TYPE_LIMIT,
		MarginMode: orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED,
	})
	if err != nil {
		t.Fatalf("Route (spot): %v", err)
	}
	if v.MIC() != "OKX" {
		t.Errorf("a spot order went to %q, want the declared default OKX", v.MIC())
	}
}

// AND WHEN NO VENUE CAN WORK IT, THE REFUSAL NAMES THE REGIME — because the
// operator's next question is which of the two to change.
func TestRouter_RefusesWhenNoVenueCanWorkTheMarginMode(t *testing.T) {
	r := NewRouter([]Venue{typedMargin("OKX", orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED)},
		WithDefaultVenue("OKX"))

	_, err := r.Route(&orderpb.OrderState{
		OrderType:  orderpb.OrderType_ORDER_TYPE_LIMIT,
		MarginMode: orderpb.MarginMode_MARGIN_MODE_ISOLATED,
	})
	if err == nil {
		t.Fatal("an ISOLATED order was routed to a deployment where every venue works spot only")
	}
	if !strings.Contains(err.Error(), "ISOLATED") {
		t.Errorf("error = %q, want it to name the regime an operator has to change", err)
	}
}

// AN UNDECLARING VENUE IS STILL ROUTABLE. Every adapter in the estate answered
// nothing about margin before #417, so reading silence as refusal here would
// take untargeted routing down on a schema addition.
func TestRouter_AnUndeclaringVenueStillTakesAMarginOrder(t *testing.T) {
	r := NewRouter([]Venue{NewSimVenue("SIM")}, WithDefaultVenue("SIM"))

	v, err := r.Route(&orderpb.OrderState{
		OrderType:  orderpb.OrderType_ORDER_TYPE_LIMIT,
		MarginMode: orderpb.MarginMode_MARGIN_MODE_CROSS,
	})
	if err != nil {
		t.Fatalf("an adapter that declared nothing refused a margin order: %v — empty means DID "+
			"NOT SAY, never SUPPORTS NOTHING", err)
	}
	if v.MIC() != "SIM" {
		t.Fatalf("routed to %q, want SIM", v.MIC())
	}
}
