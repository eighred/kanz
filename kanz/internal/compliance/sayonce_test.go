package compliance

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// A STABLE KEY IS SAID ONCE AND REMEMBERED. This is the property the whole
// mechanism exists for, and #814's repair must not weaken it: an ungoverned
// portfolio is ungoverned all day, and nothing may re-announce it.
func TestSayOnceStableSaysItOnceAndKeepsIt(t *testing.T) {
	var s sayOnce

	if !s.first("ungoverned:t1:p1") {
		t.Fatal("the first sighting of a stable key must be the one that speaks")
	}
	for i := 0; i < 1000; i++ {
		if s.first("ungoverned:t1:p1") {
			t.Fatalf("stable key spoke again on sighting %d — say-it-once is broken", i+2)
		}
	}
	if !s.first("ungoverned:t2:p1") {
		t.Fatal("a second TENANT's key must speak: keyed by portfolio alone, tenant A silenced tenant B (#243)")
	}
}

// THE CALLER-KEYED LEDGER IS BOUNDED, AND THIS IS #814's OWN "Verified when".
//
// It walks past the ceiling by a factor of four and checks the length after
// EVERY insert, not only at the end — a design that oscillates above the bound
// between sweeps is still an unbounded heap to the pod that gets OOM-killed.
func TestSayOnceVolatileNeverExceedsItsCeiling(t *testing.T) {
	var s sayOnce

	const n = 4 * maxVolatileWarnings
	for i := 0; i < n; i++ {
		s.firstAbout("unpriced:t1:p1", fmt.Sprintf("INSTRUMENT-%d", i))
		if got := len(s.volatile); got > maxVolatileWarnings {
			t.Fatalf("the caller-keyed ledger reached %d entries after %d distinct instrument ids — "+
				"the ceiling is %d and it is meant to be HARD, not a target it oscillates around",
				got, i+1, maxVolatileWarnings)
		}
	}
	if len(s.volatile) >= n {
		t.Fatalf("after %d distinct instrument ids the ledger holds %d — nothing was evicted at all",
			n, len(s.volatile))
	}
}

// EVICTION ACTUALLY HAPPENS, AND ITS COST IS WHAT THE COMMENT SAYS IT IS: a key
// that fell out re-warns. Asserted rather than described, because "bounded"
// with nothing ever dropped would pass the test above by never filling.
func TestSayOnceVolatileForgetsTheTailAndReWarns(t *testing.T) {
	var s sayOnce

	if !s.firstAbout("unpriced:t1:p1", "COLD") {
		t.Fatal("first sighting of COLD must speak")
	}
	for i := 0; i < 3*maxVolatileWarnings; i++ {
		s.firstAbout("unpriced:t1:p1", fmt.Sprintf("CHURN-%d", i))
	}
	if !s.firstAbout("unpriced:t1:p1", "COLD") {
		t.Fatal("COLD was never evicted despite three ceilings of churn — the sweep is not shedding, " +
			"so the bound above is being met some other way")
	}
}

// WHAT SURVIVES IS WHAT KEEPS ARRIVING. This is the argument that eviction does
// not become a log flood: the instrument that would spam the log is the one the
// sweep protects, because every sighting refreshes its place.
func TestSayOnceVolatileKeepsTheInstrumentThatKeepsArriving(t *testing.T) {
	var s sayOnce

	if !s.firstAbout("unpriced:t1:p1", "HOT") {
		t.Fatal("first sighting of HOT must speak")
	}
	for i := 0; i < 3*maxVolatileWarnings; i++ {
		s.firstAbout("unpriced:t1:p1", fmt.Sprintf("CHURN-%d", i))
		if i%1000 == 0 {
			if s.firstAbout("unpriced:t1:p1", "HOT") {
				t.Fatalf("HOT spoke again after %d churned ids while being sighted every 1000 — "+
					"the sweep is evicting the entries whose suppression is the whole point", i+1)
			}
		}
	}
	if s.firstAbout("unpriced:t1:p1", "HOT") {
		t.Fatal("HOT spoke again at the end despite continuous sightings")
	}
}

// A CALLER CANNOT EVICT ANOTHER CLASS'S WARNING. This is why the split is in the
// type: one evicting ledger over both classes would let invented instrument ids
// push the portfolio-level warnings out, and each would then re-fire per order —
// a memory leak traded for a log flood.
func TestSayOnceVolatileChurnCannotDisturbStable(t *testing.T) {
	var s sayOnce

	if !s.first("ungoverned:t1:p1") {
		t.Fatal("first sighting of the stable key must speak")
	}
	for i := 0; i < 4*maxVolatileWarnings; i++ {
		s.firstAbout("unpriced:t1:p1", fmt.Sprintf("INSTRUMENT-%d", i))
	}
	if s.first("ungoverned:t1:p1") {
		t.Fatal("the portfolio-level warning spoke again after caller-keyed churn — instrument ids " +
			"are evicting portfolio warnings, which is the trade this repair is not allowed to make")
	}
}

// THE CALLER-SUPPLIED COMPONENT CANNOT BE MADE TO STRADDLE THE FIXED PREFIX. A
// digest over a plain concatenation would let a chosen instrument id land on
// another portfolio's key and silence its first warning — the cross-silencing
// #243 fixed by putting the tenant in the key, reintroduced through the hash.
func TestSayOnceVolatileSeparatesKeyFromSuppliedComponent(t *testing.T) {
	var s sayOnce

	if !s.firstAbout("unpriced:t1:p", "X") {
		t.Fatal("first sighting must speak")
	}
	if !s.firstAbout("unpriced:t1:pX", "") {
		t.Fatal("a different (key, supplied) split collapsed onto the same digest — a caller can " +
			"silence a warning about a portfolio it chose by naming the right instrument")
	}
}

// #814's ACCEPTANCE, AT THE GATE ITSELF: N orders across N distinct instrument
// ids leave the gate's ledger bounded rather than N.
//
// It drives noteUnpriced, which is the refusal path an ordinary caller reaches
// by submitting a market order for an instrument nothing quotes — the whole
// order is REFUSED, so the growth costs its author nothing.
func TestPreTradeGateUnpricedWarningsAreBounded(t *testing.T) {
	g := NewPreTradeGate(nil, nil, nil, nil, nil, discardLogger())

	const n = 2 * maxVolatileWarnings
	for i := 0; i < n; i++ {
		g.noteUnpriced("tenant-a", "flagship", fmt.Sprintf("MADE-UP-%d", i))
	}

	if got := len(g.warned.volatile); got > maxVolatileWarnings {
		t.Fatalf("%d orders across %d distinct instrument ids left %d remembered warnings on the "+
			"pre-trade gate — the ceiling is %d. Unbounded growth here is an OMS OOM kill, which "+
			"arrives as orders in flight at an unknown state (#814)",
			n, n, got, maxVolatileWarnings)
	}
	if len(g.warned.stable) != 0 {
		t.Fatalf("the unpriced path wrote %d entries into the never-evicted ledger — it must use "+
			"firstTimeAbout, or the ceiling above is decorative", len(g.warned.stable))
	}
}

// THE PORTFOLIO-LEVEL PATHS STILL SAY IT ONCE. Routing them through the shared
// ledger must not have changed what an operator sees.
func TestPreTradeGatePortfolioWarningsStillSayItOnce(t *testing.T) {
	g := NewPreTradeGate(nil, nil, nil, nil, nil, discardLogger())

	for i := 0; i < 100; i++ {
		g.noteUngoverned("tenant-a", "flagship", NeverMandated)
	}
	if got := len(g.warned.stable); got != 1 {
		t.Fatalf("100 orders against one ungoverned portfolio left %d ledger entries, want 1", got)
	}
	g.noteUngoverned("tenant-b", "flagship", NeverMandated)
	if got := len(g.warned.stable); got != 2 {
		t.Fatalf("a second tenant's ungoverned book left %d entries, want 2 — the tenant must stay "+
			"in the key (#243)", got)
	}
}

// THE MANDATE REGISTRY SHARES THE LEDGER AND STILL WARNS ONCE. It is the second
// copy of the helper this change deleted, so its behaviour is asserted here
// rather than assumed from the gate's.
func TestMandateRegistryWarnsOncePerKey(t *testing.T) {
	r := NewMandateRegistry(WithMandateLogger(discardLogger()))

	if !r.warnOnce("misfiled:t1:p1") {
		t.Fatal("first sighting must speak")
	}
	if r.warnOnce("misfiled:t1:p1") {
		t.Fatal("the registry warned twice about one key")
	}
	if !r.warnOnce("misfiled:t2:p1") {
		t.Fatal("a second tenant's misfiling must speak (#243)")
	}
}
