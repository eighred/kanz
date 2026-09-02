package compliance

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func concentrationMandate(maxPct int64) *compliancepb.Mandate {
	m := mandate(&compliancepb.Rule{
		RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
			MaxWeight: dec(maxPct, -2),
		}},
	})
	m.EffectiveAt = timestamppb.New(t0)
	return m
}

// currentBook: AAPL 100 @ 1000 = 100k, MSFT 100k. AAPL is 50% of gross.
func currentBook() *Book {
	return &Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: money(200000, 0, "USD"),
		NAVBasis: NAVBasisEquity,
		Positions: []Position{
			{InstrumentID: "AAPL", Quantity: dec(100, 0), MarketValue: money(100000, 0, "USD")},
			{InstrumentID: "MSFT", Quantity: dec(100, 0), MarketValue: money(100000, 0, "USD")},
		},
	}
}

func newGate(t *testing.T, m *compliancepb.Mandate) *PreTradeGate {
	t.Helper()
	reg := NewMandateRegistry()
	if m != nil {
		mustPut(t, reg, m)
	}
	return NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, nil, nil)
}

func TestPreTradeGate_RejectsBreachingOrder(t *testing.T) {
	g := newGate(t, concentrationMandate(60)) // 60% cap
	// Buy 100 more AAPL @ 1000 ⇒ AAPL 200k of 300k = 66.7% > 60%.
	dec1 := OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(100, 0), Price: dec(1000, 0), Currency: "USD",
		OrderID: "o1", AsOf: t0,
	}
	got, err := g.Evaluate(context.Background(), dec1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatalf("order pushing AAPL to 66.7%% over a 60%% cap must be rejected")
	}
	if got.Result.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("want BREACH, got %v", got.Result.GetStatus())
	}
}

func TestPreTradeGate_AdmitsCompliantOrder(t *testing.T) {
	g := newGate(t, concentrationMandate(60))
	// Buy 10 AAPL ⇒ AAPL 110k of 210k ≈ 52% < 60%.
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(10, 0), Price: dec(1000, 0), Currency: "USD", OrderID: "o2", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatalf("compliant order must be admitted; result=%v", got.Result.GetStatus())
	}
}

func TestPreTradeGate_NoMandateAdmits(t *testing.T) {
	g := newGate(t, nil) // no mandate for p1
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(1000, 0), Price: dec(1000, 0), Currency: "USD", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatalf("order on an unmandated portfolio must be admitted")
	}
}

// recordingRecorder captures decision records for assertions.
type recordingRecorder struct{ records []DecisionRecord }

func (r *recordingRecorder) Record(_ context.Context, rec DecisionRecord) error {
	r.records = append(r.records, rec)
	return nil
}

func TestPreTradeGate_RecordsDecision(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, concentrationMandate(60))
	rec := &recordingRecorder{}
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, rec, nil)
	if _, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(100, 0), Price: dec(1000, 0), Currency: "USD", OrderID: "o1", AsOf: t0,
	}); err != nil {
		t.Fatal(err)
	}
	if len(rec.records) != 1 || rec.records[0].Phase != PhasePreTrade || rec.records[0].Allowed {
		t.Fatalf("expected one pre-trade reject record, got %+v", rec.records)
	}
}

// TestAnUngovernedPortfolioIsNotSilent pins EXEC-M14.
//
// An order for a portfolio NO MANDATE GOVERNS used to return Allowed:true with no
// log, no metric, and no field on the decision to say so. "We forgot to put fund X
// under mandate" and "fund X passed compliance" were THE SAME OBSERVABLE EVENT — and
// the one that means nobody is checking anything is the one you cannot see.
func TestAnUngovernedPortfolioIsNotSilent(t *testing.T) {
	var counted []string
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, NewMandateRegistry(), nil, nil, nil,
		WithUngovernedObserver(func(_, pf string, _ Governance) { counted = append(counted, pf) }))

	dec, err := g.Evaluate(context.Background(), OrderDelta{TenantID: "t1", PortfolioID: "unmandated", AsOf: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !dec.Allowed {
		t.Fatal("the default posture ADMITS an ungoverned order — turning that off is a deliberate switch, not a surprise")
	}
	if !dec.Ungoverned {
		t.Fatal("the decision does not say the portfolio is ungoverned: it is indistinguishable from passing compliance")
	}
	if len(counted) != 1 || counted[0] != "unmandated" {
		t.Fatalf("the ungoverned order was not counted: %v", counted)
	}
}

// A mandate with NO RULES is a CHOICE — somebody decided, explicitly, to constrain
// nothing. It is not the same as no mandate at all, and it must not be reported as a
// gap: that is how a real signal gets buried under noise nobody can action.
func TestAMandateWithNoRulesIsGovernedNotUngoverned(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, &compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		EffectiveAt: timestamppb.New(time.Now().Add(-time.Hour)),
		// no rules, on purpose
	})
	var counted int
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, nil, nil,
		WithUngovernedObserver(func(string, string, Governance) { counted++ }))

	dec, err := g.Evaluate(context.Background(), OrderDelta{TenantID: "t1", PortfolioID: "p1", AsOf: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !dec.Allowed {
		t.Fatal("a mandate that declares no rules permits the order")
	}
	if dec.Ungoverned {
		t.Fatal("a portfolio governed by a permissive mandate was reported as UNGOVERNED — somebody made that decision, and " +
			"burying it among the real gaps is how the real gaps get ignored")
	}
	if counted != 0 {
		t.Fatalf("a deliberate choice was counted as a gap %d time(s)", counted)
	}
}

// With OMS_REQUIRE_MANDATE, an ungoverned portfolio cannot trade at all.
func TestRequireMandateRefusesAnUngovernedPortfolio(t *testing.T) {
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, NewMandateRegistry(), nil, nil, nil,
		WithRequireMandate(true))

	dec, err := g.Evaluate(context.Background(), OrderDelta{TenantID: "t1", PortfolioID: "unmandated", AsOf: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Allowed {
		t.Fatal("OMS_REQUIRE_MANDATE is set and the order was ADMITTED for a portfolio nothing governs")
	}
	if !dec.Ungoverned {
		t.Fatal("the refusal must say WHY: nothing was breached, because nothing governs this portfolio")
	}
}

// AND IT REFUSES THE GAP, NOT THE CHOICE (#779).
//
// This is the property the per-tenant posture rests on. A tenant's rendered OMS
// ships OMS_REQUIRE_MANDATE=true so that a portfolio NOBODY has put under
// mandate is refused rather than traded unconstrained. That is only safe if the
// armed posture still admits the OTHER state — a mandate carrying zero rules,
// which is somebody having decided, explicitly, to constrain nothing.
//
// Collapsing the two would make the flip unusable: an operator who genuinely has
// an unconstrained portfolio would have no way to say so, and the pressure would
// be to turn the control back off for the whole deployment. The counterpart
// above proves the gap is refused; this proves the decision is not.
//
// It also asserts the ungoverned OBSERVER stays silent. Ungoverned is a counter
// an alert fires on (kanz_compliance_ungoverned_orders_total); counting a
// deliberate zero-rule mandate there would page an operator about a portfolio
// somebody had already ruled on.
func TestRequireMandateStillAdmitsAZeroRuleMandate(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, &compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		EffectiveAt: timestamppb.New(t0),
		// no rules, on purpose: the explicit "constrain nothing" decision
	})
	counted := 0
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, nil, nil,
		WithRequireMandate(true),
		WithUngovernedObserver(func(string, string, Governance) { counted++ }))

	dec, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dec.Allowed {
		t.Fatal("OMS_REQUIRE_MANDATE refused a portfolio that IS under mandate — one carrying zero " +
			"rules. That collapses \"nobody decided\" into \"decided to constrain nothing\", and " +
			"leaves an operator with no way to run an unconstrained portfolio except by disarming " +
			"the control for the whole deployment (#779)")
	}
	if dec.Ungoverned {
		t.Fatal("a portfolio under a zero-rule mandate was reported as UNGOVERNED; the two states " +
			"are distinct and the refusal path must not claim the gap")
	}
	if counted != 0 {
		t.Fatalf("a deliberate zero-rule mandate incremented the ungoverned counter %d time(s) — "+
			"that counter is alerted on, and this would page an operator about a portfolio "+
			"somebody had already ruled on", counted)
	}
}

// restrictAAPLMandate denies holding AAPL outright — any book holding it, with
// no order at all, already breaches.
func restrictAAPLMandate() *compliancepb.Mandate {
	m := mandate(&compliancepb.Rule{
		RuleId: "r1", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
		Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
			Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
			Values:    []string{"AAPL"},
			Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
		}},
	})
	m.EffectiveAt = timestamppb.New(t0)
	return m
}

// TestPreTradeGate_MarketOrderCannotEraseAnExistingBreach is THE regression
// test for COMP-M1. currentBook() already holds AAPL, and restrictAAPLMandate
// forbids holding it — the book is BREACHING before any order is placed. A
// MARKET order (nil price, the only kind that lacks one) touching that same
// AAPL position must NOT be able to erase the breach from the check.
//
// Before the fix: project() re-marks AAPL at newQty × nil price = zero, and
// heldPositions (rules.go) treats a zero-value position as flat and skips it —
// so RestrictionRule never sees AAPL, and an order that should be refused is
// ADMITTED. That is the live bug: real money moving through a "checked" gate
// that checked nothing.
func TestPreTradeGate_MarketOrderCannotEraseAnExistingBreach(t *testing.T) {
	g := newGate(t, restrictAAPLMandate())
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(10, 0), Price: nil, // MARKET order: no limit price
		Currency: "USD", OrderID: "o-market", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatal("a MARKET order touching an ALREADY-BREACHING position must not be admitted — " +
			"the nil price erased the breach from the check (COMP-M1)")
	}
	if !got.Unpriced {
		t.Fatal("the refusal must say WHY: the order could not be valued, not that a rule fired")
	}
	if got.Result != nil && len(got.Result.GetViolations()) > 0 {
		t.Fatalf("an unpriced refusal must not report a rule violation — nothing was evaluated, got %+v", got.Result.GetViolations())
	}
}

// TestPreTradeGate_NilPriceRefusedWithNoViolation covers the general nil-price
// case on a governed portfolio: refused, but as Unpriced — not as a breach of
// any rule (nothing was evaluated).
func TestPreTradeGate_NilPriceRefusedWithNoViolation(t *testing.T) {
	g := newGate(t, concentrationMandate(60))
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(10, 0), Price: nil, Currency: "USD", OrderID: "o1", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatal("a nil-price order against a governed portfolio must be refused")
	}
	if !got.Unpriced {
		t.Fatal("want Unpriced:true")
	}
	if got.Result != nil && len(got.Result.GetViolations()) > 0 {
		t.Fatalf("nothing was evaluated — must not report a violation, got %+v", got.Result.GetViolations())
	}
}

// TestPreTradeGate_ZeroAndNegativePriceRefused: a zero or negative price is
// just as unusable as nil (bridge.go's cent-rounding can produce a real zero).
func TestPreTradeGate_ZeroAndNegativePriceRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		price *commonpb.Decimal
	}{
		{"zero", dec(0, 0)},
		{"negative", dec(-100, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGate(t, concentrationMandate(60))
			got, err := g.Evaluate(context.Background(), OrderDelta{
				TenantID:    "t1",
				PortfolioID: "p1", InstrumentID: "AAPL",
				SignedQuantity: dec(10, 0), Price: tc.price, Currency: "USD", OrderID: "o1", AsOf: t0,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Allowed || !got.Unpriced {
				t.Fatalf("a %s price must be refused as Unpriced, got Allowed=%v Unpriced=%v", tc.name, got.Allowed, got.Unpriced)
			}
		})
	}
}

// TestPreTradeGate_NilPriceZeroRulesStillAdmits: a mandate that constrains
// nothing performs no evaluation, so no price is needed to admit the order —
// blocking it would be a behaviour change with zero safety benefit.
func TestPreTradeGate_NilPriceZeroRulesStillAdmits(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, &compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		EffectiveAt: timestamppb.New(t0),
		// no rules, on purpose
	})
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, nil, nil)
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL", Price: nil, AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatal("a mandate with zero rules performs no evaluation and needs no price")
	}
	if got.Unpriced {
		t.Fatal("no evaluation occurred — this must not be reported as Unpriced")
	}
}

// TestPreTradeGate_NilPriceNoMandateStillUngoverned: the Ungoverned check must
// win over the new Unpriced check — placement in Evaluate matters.
func TestPreTradeGate_NilPriceNoMandateStillUngoverned(t *testing.T) {
	g := newGate(t, nil) // no mandate for p1
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL", Price: nil, AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed || !got.Ungoverned {
		t.Fatalf("an unmandated portfolio must still be Ungoverned regardless of price, got Allowed=%v Ungoverned=%v Unpriced=%v",
			got.Allowed, got.Ungoverned, got.Unpriced)
	}
	if got.Unpriced {
		t.Fatal("Ungoverned must not be shadowed by Unpriced")
	}
}

// TestPreTradeGate_PositivePriceLimitOrderUnaffected pins that a priced order
// evaluates exactly as before: this task removes a bypass, it does not retune
// any rule.
func TestPreTradeGate_PositivePriceLimitOrderUnaffected(t *testing.T) {
	g := newGate(t, concentrationMandate(60))
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(100, 0), Price: dec(1000, 0), Currency: "USD", OrderID: "o1", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatal("a priced order pushing AAPL over the cap must still be rejected")
	}
	if got.Unpriced {
		t.Fatal("a priced order must never be reported as Unpriced")
	}
	if got.Result.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("want BREACH, got %v", got.Result.GetStatus())
	}
}

// --- COMP-M1 Task 3: the zero-value-means-flat invariant --------------------
//
// heldPositions (rules.go) treats a position with zero MarketValue as flat and
// skips it. Task 1 stops an unpriced order from reaching that code at all, but
// nothing states the invariant that made the original bypass possible — and
// the bridge (bridge.go) already proves a second caller can hand Evaluate a
// nil price. This guards the invariant directly, at the seam, so a future
// caller cannot reintroduce the collapse by another door.

// candidateSpy captures the Candidate the engine evaluates, so the test can
// inspect the exact positions the rule engine saw.
type candidateSpy struct{ candidates []*Candidate }

func (s *candidateSpy) Register() *Registry {
	r := NewRegistry()
	spy := func(c *Candidate, _ *compliancepb.Rule) *compliancepb.Violation {
		s.candidates = append(s.candidates, c)
		return nil
	}
	r.Register(compliancepb.RuleType_RULE_TYPE_RESTRICTION, spy)
	return r
}

func TestNoCandidateHasAZeroValueNonZeroQuantityPosition(t *testing.T) {
	spy := &candidateSpy{}
	reg := NewMandateRegistry()
	mustPut(t, reg, mandate(&compliancepb.Rule{RuleId: "r1", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
		Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
			Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT, Mode: compliancepb.RestrictionMode_RESTRICTION_MODE_ALLOW_ONLY,
		}}}))
	g := NewPreTradeGate(NewEngine(spy.Register()), MapBookSource{"p1": currentBook()}, reg, nil, nil, nil)

	cases := []OrderDelta{
		{TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(10, 0), Price: nil, AsOf: t0},            // nil price
		{TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(10, 0), Price: dec(0, 0), AsOf: t0},      // zero price
		{TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(10, 0), Price: dec(-5, 0), AsOf: t0},     // negative price
		{TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(-100, 0), Price: dec(1000, 0), AsOf: t0}, // legitimate sell-to-flat
	}
	for _, d := range cases {
		if _, err := g.Evaluate(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}

	// Only the sell-to-flat case reaches the engine (the other three are refused
	// as Unpriced before project()/the engine runs at all).
	if len(spy.candidates) == 0 {
		t.Fatal("non-vacuous check: the engine never ran, so this test would pass for the wrong reason")
	}
	for _, c := range spy.candidates {
		for _, p := range c.Book.Positions {
			zero := p.MarketValue == nil || absRatFromMoney(p.MarketValue).Sign() == 0
			nonZeroQty := p.Quantity != nil && p.Quantity.GetCoefficient() != 0
			if zero && nonZeroQty {
				t.Fatalf("candidate reached the rule engine with a zero-value, non-zero-quantity position (%q) — "+
					"that is 'price unknown' masquerading as 'flat' (EXEC-M14 collapse)", p.InstrumentID)
			}
		}
	}
	// The legitimate sell-to-flat must still be ALLOWED — a genuinely flat
	// position IS zero-value, and heldPositions is correct to skip it.
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID:    "t1",
		PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(-100, 0), Price: dec(1000, 0), Currency: "USD", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatal("a legitimate sell-to-flat must still be allowed — a genuinely flat position is correctly skipped")
	}
}

// THE SLICE COUNT REACHES THE RECORD (#435, #484).
//
// A scheduled parent is checked ONCE, for the whole notional, and its children
// are admitted without re-checking (#483). The fills therefore land on N order
// ids while only the parent has a decision record — so the record has to say it
// authorised more orders than the one it names, or the audit log cannot be read
// backwards from a child.
//
// This pins the hop from OrderDelta to DecisionRecord. It is a plain field copy,
// which is exactly the kind of thing that is silently dropped in a refactor and
// noticed by nobody, because everything downstream keeps working and only the
// audit trail is quietly thinner.
func TestGateRecordsWorkedSlices(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, concentrationMandate(60))

	rec := &recordingRecorder{}
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, rec, nil)
	if _, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(100, 0), Price: dec(1000, 0), Currency: "USD",
		OrderID: "parent1", AsOf: t0, WorkedSlices: 6,
	}); err != nil {
		t.Fatal(err)
	}
	if len(rec.records) != 1 {
		t.Fatalf("expected one record, got %d", len(rec.records))
	}
	if got := rec.records[0].WorkedSlices; got != 6 {
		t.Fatalf("WorkedSlices = %d, want 6 — the decision authorised six child orders and the "+
			"record names only one of them, so an auditor arriving from a child cannot tell "+
			"whether they have the whole authorisation", got)
	}

	// AND AN ORDINARY ORDER RECORDS ZERO, which is what makes the non-zero case
	// mean something.
	rec2 := &recordingRecorder{}
	g2 := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, rec2, nil)
	if _, err := g2.Evaluate(context.Background(), OrderDelta{
		TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(100, 0), Price: dec(1000, 0), Currency: "USD",
		OrderID: "o1", AsOf: t0,
	}); err != nil {
		t.Fatal(err)
	}
	if got := rec2.records[0].WorkedSlices; got != 0 {
		t.Errorf("WorkedSlices = %d on an order nobody sliced, want 0", got)
	}
}
