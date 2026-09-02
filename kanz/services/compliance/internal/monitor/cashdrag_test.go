package monitor

// The cash-drag measurement rides the post-trade sweep (#963).
//
// The arch guard beside this asserts the CALL SITE — that observeCashDrag is in
// Monitor.evaluate and ahead of the mandate lookup. That is structural, and it
// cannot answer the question the design actually rests on: does a portfolio that
// NEVER TRADES get measured?
//
// That is the whole placement argument. Idle cash sits in books nobody is
// trading — a portfolio being rebalanced is by definition putting its cash to
// work — so a measurement woken only by order flow or position FACTs samples
// exactly the set least likely to have any. The interval sweep is what makes the
// difference, and only a behavioural test reaches it.

import (
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/treasury"
)

type dragCall struct {
	tenant, portfolio string
	drag              treasury.Drag
}

// A BOOK THAT NEVER TRADES AGAIN IS STILL MEASURED, BY THE SWEEP.
//
// One position FACT establishes the book; nothing else happens after it. The
// portfolio then sits there exactly as a fund holding cash and doing nothing
// does, and ReevaluateAll — the interval sweep — is what has to find it. If this
// regresses, the measurement silently narrows to portfolios with order flow and
// reports the drag of the books least likely to have any.
func TestASweepMeasuresAPortfolioThatIsNotTrading(t *testing.T) {
	var calls []dragCall
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, nil, nil, nil,
		WithCashDragObserver(func(tenant, pf string, d treasury.Drag) {
			calls = append(calls, dragCall{tenant, pf, d})
		}))
	m.now = func() time.Time { return t0.Add(time.Hour) }

	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, t0)); err != nil {
		t.Fatal(err)
	}
	fromFact := len(calls)
	if fromFact == 0 {
		t.Fatal("a position FACT produced no cash-drag observation at all — evaluate is not " +
			"measuring, and the sweep below cannot be distinguished from it")
	}

	// Nothing trades. The sweep is the only thing that runs.
	if err := m.ReevaluateAll(testCtx()); err != nil {
		t.Fatalf("ReevaluateAll: %v", err)
	}
	if len(calls) <= fromFact {
		t.Fatal("the interval sweep produced NO cash-drag observation. ReevaluateAll walks every " +
			"book the monitor holds precisely so a portfolio with no FACT behind it is still " +
			"looked at; if the measurement does not ride it, idle cash is only ever measured " +
			"for portfolios that ARE trading — which is the set least likely to be sitting on " +
			"any, and the whole reason this rides the post-trade path rather than the order " +
			"path (#963).")
	}
	if got := calls[len(calls)-1]; got.tenant != "t1" || got.portfolio != "p1" {
		t.Fatalf("the sweep reported %s/%s, want t1/p1 — the tenant and portfolio come off the "+
			"monitor's own bookKey, and a measurement filed under the wrong one is unroutable",
			got.tenant, got.portfolio)
	}
}

// THE BOOK WITH NO CASH SOURCE REFUSES, RATHER THAN REPORTING ZERO IDLE CASH.
//
// This monitor is constructed with no WithCashSource, which is a legal posture —
// a deployment with no book of record. comp.JoinEquity leaves Cash nil, and the
// measurement must report cash_unknown: a book whose balance nobody has announced
// is not a book holding no cash, and 0% idle is the flattering reading that
// nothing downstream would question.
func TestABookWithNoAnnouncedCashIsRefusedNotReportedAsZero(t *testing.T) {
	var calls []dragCall
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, nil, nil, nil,
		WithCashDragObserver(func(tenant, pf string, d treasury.Drag) {
			calls = append(calls, dragCall{tenant, pf, d})
		}))
	m.now = func() time.Time { return t0.Add(time.Hour) }

	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, t0)); err != nil {
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Fatal("no observation")
	}
	got := calls[0].drag
	if got.Measured() {
		t.Fatalf("a book with NO announced cash reported a measured idle share of %s. Nothing has "+
			"told this deployment what the portfolio can spend, so the only honest answer is that "+
			"the drag is unknown — reporting a share here is a number computed from a nil (#963).",
			got.IdleShare.RatString())
	}
	if got.Reason != treasury.ReasonCashUnknown {
		t.Fatalf("reason = %q, want %q — this deployment has no cash source at all, which is "+
			"distinct from having one whose completeness nobody vouched for",
			got.Reason, treasury.ReasonCashUnknown)
	}
}

// A MONITOR WITH NO OBSERVER DOES NOT PANIC AND STILL EVALUATES.
//
// The observer is optional: a deployment that wires none measures no cash drag,
// which is a legal posture. It must not turn into a nil dereference inside the
// loop that watches every book on the estate for passive breaches — a
// measurement that can take down the control it rides on is a worse trade than
// a missing sample.
func TestAMonitorWithNoCashDragObserverStillEvaluates(t *testing.T) {
	m := NewMonitor(comp.NewEngine(nil), concentrationRegistry(t), nil, nil, nil, nil)
	m.now = func() time.Time { return t0.Add(time.Hour) }

	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, t0)); err != nil {
		t.Fatalf("Handle with no cash-drag observer: %v", err)
	}
	if err := m.ReevaluateAll(testCtx()); err != nil {
		t.Fatalf("ReevaluateAll with no cash-drag observer: %v", err)
	}
}
