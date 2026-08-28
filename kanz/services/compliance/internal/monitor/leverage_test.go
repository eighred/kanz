package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/cashview"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/marketdata/mark"
)

// LEVERAGE, ENFORCED AFTER THE TRADE AND NOT ONLY BEFORE IT (#787).
//
// The monitor's own input is position FACTs, which carry holdings and nothing
// else: no cash, and values the OMS position projector recorded at AVERAGE COST.
// Gross exposure is the sum of the same values, so a max_gross_leverage cap
// scored exactly 1.0 for any long-only book and bound on nothing (#780). Since
// #786 that is a refusal naming the gap rather than a silent pass — honest, and
// still not enforcement.
//
// With the two folds wired the monitor joins the same three inputs the OMS
// pre-trade gate does, through the same comp.JoinEquity. These tests use the
// REAL folds — a real accounting.v1.PortfolioCashBalance through cashview, a
// real market.v1.MarketDataEvent through mark.Source — because the thing that
// made this defect survive was three components each correct on its own
// producing numbers that did not meet.
//
// THE CENTRAL CASE IS THE ONE WITH NO FACT BEHIND IT. Leverage is a limit a book
// breaches WITHOUT TRADING: a financed book that falls raises it with no order
// placed anywhere, which is what TestAFallingBookBreachesWithNoPositionFACT
// drives and what nothing on this platform could previously see.

// clockAt stops the clock. BOTH folds expire on wall-clock age — cashview on the
// balance's AsOf, mark.Source on the print's event time — so a fixture dated t0
// against a real clock is months stale and every lookup refuses, which reads as a
// broken join rather than a working freshness bound. Windows' time.Now() is also
// coarse enough that a short bound flakes; a stopped clock makes the age exact.
func clockAt(at time.Time) func() time.Time { return func() time.Time { return at } }

// foldCash puts a real balance announcement through the real view. A negative
// total is a MARGIN LOAN, which is what being levered looks like on the cash
// side and the reason equity can sit below the value of the holdings.
func foldCash(t *testing.T, v *cashview.View, portfolio string, total int64, at time.Time) {
	t.Helper()
	payload, err := proto.Marshal(&accountingpb.PortfolioCashBalance{
		PortfolioId:   portfolio,
		BaseCurrency:  "USD",
		Total:         decv(total, 0),
		AsOf:          timestamppb.New(at),
		KnowledgeTime: timestamppb.New(at),
	})
	if err != nil {
		t.Fatalf("marshal balance: %v", err)
	}
	if err := v.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("fold balance: %v", err)
	}
}

// foldMark puts a real trade print through the real mark fold.
func foldMark(t *testing.T, src *mark.Source, instrument string, price int64, at time.Time) {
	t.Helper()
	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		EventTime:    timestamppb.New(at),
		Data:         &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: decv(price, 0)}},
	})
	if err != nil {
		t.Fatalf("marshal market data: %v", err)
	}
	if err := src.Handle(context.Background(), &envelopepb.Envelope{EventType: "market.instrument.trade"}, payload); err != nil {
		t.Fatalf("fold mark: %v", err)
	}
	if src.Mark(instrument) == nil {
		t.Fatalf("the fold accepted the event and holds no price for %s — this fixture is not "+
			"exercising what it claims to", instrument)
	}
}

func leverageRegistry(t *testing.T, limit int64, exp int32) *comp.MandateRegistry {
	t.Helper()
	reg := comp.NewMandateRegistry()
	if err := reg.Put(&compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		EffectiveAt: timestamppb.New(t0),
		Rules: []*compliancepb.Rule{{
			RuleId: "lev-1", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
			Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
				MaxGrossLeverage: decv(limit, exp),
			}},
		}},
	}); err != nil {
		t.Fatalf("registry refused a well-formed mandate: %v", err)
	}
	return reg
}

// levered builds a monitor over a book of 1,000 AAPL financed by a margin loan,
// with the real folds behind it, and returns everything a test needs to move the
// market or the cash underneath it.
type levered struct {
	mon   *Monitor
	bus   *fakeBus
	cash  *cashview.View
	marks *mark.Source
}

func leveredBook(t *testing.T, cap int64, capExp int32, price, cashTotal int64) levered {
	t.Helper()
	at := t0
	cash := cashview.New(cashview.WithMaxAge(time.Hour), cashview.WithClock(clockAt(at)))
	marks := mark.New(clockAt(at), time.Hour)
	foldCash(t, cash, "p1", cashTotal, at)
	foldMark(t, marks, "AAPL", price, at)

	fb := &fakeBus{}
	m := NewMonitor(comp.NewEngine(nil), leverageRegistry(t, cap, capExp), nil, NewEmitter(fb), nil, nil,
		WithCashSource(cash), WithMarkSource(marks))
	m.now = func() time.Time { return at }
	return levered{mon: m, bus: fb, cash: cash, marks: marks}
}

// hold folds one position FACT: 1,000 AAPL, whose recorded market value is the
// COST the OMS projector wrote. It is deliberately different from the mark, so a
// book valued at cost and one valued at market cannot be confused.
func (l levered) hold(t *testing.T, cost int64) {
	t.Helper()
	if err := l.mon.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 1_000, cost, t0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func (l levered) breaches() *compliancepb.ComplianceResult {
	for _, e := range l.bus.events {
		if br, ok := e.Payload.(*compliancepb.ComplianceBreach); ok {
			return br.GetResult()
		}
	}
	return nil
}

func violationText(res *compliancepb.ComplianceResult) string {
	var b strings.Builder
	for _, v := range res.GetViolations() {
		b.WriteString(v.GetMessage())
		for k, val := range v.GetEvidence() {
			b.WriteString(" " + k + "=" + val)
		}
	}
	return b.String()
}

// TestAnUnvaluedBookCannotReportARatio is the state this issue is about, pinned
// so the repair is visible as a change rather than asserted about.
//
// With no cash and no marks the monitor's book is a positions total. #786 makes
// that a refusal instead of a 1.0 that passes — but a refusal is not enforcement,
// and the point of the tests below is that they could not be written until the
// folds existed.
func TestAnUnvaluedBookCannotReportARatio(t *testing.T) {
	fb := &fakeBus{}
	m := NewMonitor(comp.NewEngine(nil), leverageRegistry(t, 25, -1), nil, NewEmitter(fb), nil, nil)
	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 1_000, 100_000, t0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	res := levered{bus: fb}.breaches()
	if res == nil {
		t.Fatal("a book whose leverage cannot be computed reported nothing at all")
	}
	got := violationText(res)
	if !strings.Contains(got, "not an equity figure") || !strings.Contains(got, "gross_positions") {
		t.Fatalf("violation = %q; want the refusal naming the basis. Anything else means the "+
			"monitor divided by a positions total again (#780)", got)
	}
}

// TestAFallingBookBreachesWithNoPositionFACT is #787's acceptance criterion.
//
// $100,000 of stock financed by $50,000 of equity is 2.0x — inside a 2.5x cap,
// and nothing is emitted. Then the market falls 20% and NOTHING ELSE HAPPENS: no
// order, no fill, no position FACT. The holdings are worth $80,000, the margin
// loan is unchanged, so equity is $30,000 and gross leverage is 2.67x.
//
// Until the sweep existed there was no path by which anything asked. The package
// doc calls this "a passive, market-move-induced breach" and names it as the
// reason the monitor exists; the position spine never delivered one, because the
// OMS position book values at average cost and republishes only when a fill lands.
func TestAFallingBookBreachesWithNoPositionFACT(t *testing.T) {
	l := leveredBook(t, 25, -1, 100, -50_000) // 2.5x cap, $100 mark, $50k margin loan
	l.hold(t, 100_000)

	if res := l.breaches(); res != nil {
		t.Fatalf("a book at 2.0x breached a 2.5x cap on the way in: %s", violationText(res))
	}

	// THE MARKET FALLS. One trade print, no position FACT anywhere.
	foldMark(t, l.marks, "AAPL", 80, t0)
	if err := l.mon.ReevaluateAll(context.Background()); err != nil {
		t.Fatalf("ReevaluateAll: %v", err)
	}

	res := l.breaches()
	if res == nil {
		t.Fatal("a financed book fell 20% and its gross leverage went from 2.0x to 2.67x against a " +
			"2.5x cap, and nothing was emitted. This is the breach the monitor exists to catch and " +
			"the one with no FACT behind it (#787)")
	}
	got := violationText(res)
	if !strings.Contains(got, "gross leverage exceeds cap") {
		t.Fatalf("violation = %q; want a real ratio. A refusal here means the book still could not "+
			"be valued", got)
	}
	if !strings.Contains(got, "observed=2.666") {
		t.Errorf("violation = %q; want observed 2.666… ($80,000 of stock over $30,000 of equity)", got)
	}
}

// TestACashDrawdownBreachesWithNoPositionFACT is the other half of "equity
// moved and nothing traded". Cash is the second input, so drawing further on a
// margin loan raises leverage exactly as a falling market does — and a fold that
// only updated the number would leave that waiting for an unrelated event.
func TestACashDrawdownBreachesWithNoPositionFACT(t *testing.T) {
	l := leveredBook(t, 25, -1, 100, -50_000)
	l.hold(t, 100_000)
	if res := l.breaches(); res != nil {
		t.Fatalf("a book at 2.0x breached a 2.5x cap on the way in: %s", violationText(res))
	}

	// The fund draws another $20,000 against the same holdings: equity $30,000,
	// gross $100,000, 3.33x.
	foldCash(t, l.cash, "p1", -70_000, t0)
	if err := l.mon.Reevaluate(context.Background(), "t1", "p1"); err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}

	res := l.breaches()
	if res == nil {
		t.Fatal("a fund drew its margin loan from $50k to $70k against unchanged holdings — 2.0x to " +
			"3.33x under a 2.5x cap — and nothing was emitted")
	}
	if got := violationText(res); !strings.Contains(got, "observed=3.333") {
		t.Errorf("violation = %q; want observed 3.333… ($100,000 over $30,000)", got)
	}
}

// TestTheSweepDoesNotReEmitAStandingBreach. The sweep runs every interval
// forever; a book that is breaching stays breaching, and re-emitting each pass
// would turn one incident into a stream nobody reads. Every path goes through
// evaluate, which emits only on the TRANSITION — this pins that the sweep did
// not acquire its own copy of that logic.
func TestTheSweepDoesNotReEmitAStandingBreach(t *testing.T) {
	l := leveredBook(t, 15, -1, 100, -50_000) // 1.5x cap against a 2.0x book
	l.hold(t, 100_000)
	if l.breaches() == nil {
		t.Fatal("a book at 2.0x did not breach a 1.5x cap")
	}
	before := len(l.bus.events)

	for i := 0; i < 3; i++ {
		if err := l.mon.ReevaluateAll(context.Background()); err != nil {
			t.Fatalf("ReevaluateAll: %v", err)
		}
	}
	if got := len(l.bus.events); got != before {
		t.Fatalf("three sweeps over a standing breach emitted %d more FACT(s); a persistently "+
			"breaching book must emit once", got-before)
	}
}

// TestReevaluateIgnoresAPortfolioTheSpineHasNeverDescribed. A cash announcement
// names a portfolio; the position spine may never have described it. Evaluating
// an empty book would report it CLEAN against every concentration limit there is
// (EXEC-M18), which is worse than not evaluating at all.
func TestReevaluateIgnoresAPortfolioTheSpineHasNeverDescribed(t *testing.T) {
	l := leveredBook(t, 15, -1, 100, -50_000)

	if err := l.mon.Reevaluate(context.Background(), "t1", "never-seen"); err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}
	if len(l.bus.events) != 0 {
		t.Fatalf("evaluating a portfolio with no positions emitted %d event(s) — an empty book "+
			"passes every limit, and reporting that is a false clean", len(l.bus.events))
	}
}

// TestAStaleMarkStopsTheMonitorValuingTheBook. mark.Source expires a price past
// its bound and returns nil, refusing to distinguish "never seen" from "seen and
// stale" on a decision path. A monitor that kept quoting the last price it saw
// would enforce a leverage cap against a number that stopped moving — which is
// the same silence one layer over.
func TestAStaleMarkStopsTheMonitorValuingTheBook(t *testing.T) {
	at := t0
	cash := cashview.New(cashview.WithMaxAge(time.Hour), cashview.WithClock(clockAt(at)))
	foldCash(t, cash, "p1", -50_000, at)
	// An hour-old print against a one-minute bound, on a stopped clock.
	marks := mark.New(clockAt(at), time.Minute)
	foldMarkAged(t, marks, "AAPL", 100, at.Add(-time.Hour))

	fb := &fakeBus{}
	m := NewMonitor(comp.NewEngine(nil), leverageRegistry(t, 15, -1), nil, NewEmitter(fb), nil, nil,
		WithCashSource(cash), WithMarkSource(marks))
	if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 1_000, 100_000, t0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	res := levered{bus: fb}.breaches()
	if res == nil {
		t.Fatal("a book that could not be valued reported nothing")
	}
	got := violationText(res)
	if !strings.Contains(got, "no live mark") {
		t.Fatalf("violation = %q; want the refusal naming the unpriced instrument. A ratio here "+
			"means the monitor valued the book off an EXPIRED price", got)
	}
}

// foldMarkAged folds a print whose event time is in the past, without asserting
// the result is live — which is the whole point of the stale fixture.
func foldMarkAged(t *testing.T, src *mark.Source, instrument string, price int64, at time.Time) {
	t.Helper()
	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		EventTime:    timestamppb.New(at),
		Data:         &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: decv(price, 0)}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := src.Handle(context.Background(), &envelopepb.Envelope{EventType: "market.instrument.trade"}, payload); err != nil {
		t.Fatalf("fold mark: %v", err)
	}
	if _, _, seen := src.Lookup(instrument); !seen {
		t.Fatal("the fold rejected the event outright — this fixture is testing a REJECTED print, " +
			"not a stale one")
	}
}

var _ = commonpb.Decimal{}

// TestTheSweepPublishesUnderTheBooksTenant pins a defect these tests found.
//
// Emitter.EmitBreach sets no Event.TenantID and compliance configures no
// producer-level fallback, so the tenant a breach publishes under is whatever is
// on the context — which bus.Consumer stamps from the INBOUND ENVELOPE. The
// sweep has no inbound envelope: it runs on a ticker. Every breach it found
// failed to publish with "tenant_id required", which is the shape that once
// crash-looped the OMS, and it would have been invisible until a passive breach
// actually occurred in production.
//
// The context here deliberately carries NO tenant, which is what the ticker
// goroutine passes.
func TestTheSweepPublishesUnderTheBooksTenant(t *testing.T) {
	l := leveredBook(t, 15, -1, 100, -50_000) // 1.5x cap against a 2.0x book
	if err := l.mon.ReevaluateAll(context.Background()); err != nil {
		t.Fatalf("ReevaluateAll on an empty monitor: %v", err)
	}
	l.hold(t, 100_000)
	if l.breaches() == nil {
		t.Fatal("a book at 2.0x did not breach a 1.5x cap through the FACT path")
	}

	// A SECOND BOOK, breaching, reached ONLY by the sweep — with a bare context.
	l2 := leveredBook(t, 15, -1, 100, -50_000)
	l2.hold(t, 100_000)
	l2.bus.events = nil
	l2.mon.resetStatus(bookKey{tenant: "t1", portfolio: "p1"})

	if err := l2.mon.ReevaluateAll(context.Background()); err != nil {
		t.Fatalf("the sweep could not publish what it found: %v. The fakeBus rejects an empty "+
			"tenant exactly as the broker does, so this is the production failure and not a "+
			"fixture artefact", err)
	}
	if l2.breaches() == nil {
		t.Fatal("the sweep found a breaching book and emitted nothing")
	}
}
