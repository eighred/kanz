package compliance

import (
	"context"
	"errors"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// configChanged marshals a ConfigChanged the way the publisher puts one on the
// compacted mandate stream.
func configChanged(t *testing.T, key, value string) []byte {
	t.Helper()
	b, err := proto.Marshal(&lifecyclepb.ConfigChanged{ConfigKey: key, NewValue: value})
	if err != nil {
		t.Fatalf("marshal ConfigChanged: %v", err)
	}
	return b
}

func goodMandateValue(t *testing.T, tenant, portfolio string) string {
	t.Helper()
	v, err := MarshalMandateValue(&compliancepb.Mandate{
		MandateId: "m-" + portfolio, TenantId: tenant, PortfolioId: portfolio,
		Version: 1, EffectiveAt: timestamppb.New(time.Unix(0, 0).UTC()),
	})
	if err != nil {
		t.Fatalf("marshal mandate: %v", err)
	}
	return v
}

// A mandate the consumer cannot apply must leave the portfolio REFUSED, not
// absent. The stream is compacted, so the message that failed is the LAST one on
// that portfolio's subject: every consumer that boots from now on arms itself
// with it, forever, and the portfolio is otherwise ungoverned permanently.
func TestAMandateThatCannotBeAppliedLeavesThatPortfolioUnreadable(t *testing.T) {
	reg := NewMandateRegistry()
	c := NewMandateConsumer(reg, nil)

	key := MandateConfigKey("acme", "pf-1")
	if err := c.Handle(context.Background(), nil, configChanged(t, key, "{not json")); err != nil {
		t.Fatalf("the handler must still ACK poison, got %v", err)
	}

	_, ok, err := reg.Mandate(context.Background(), "acme", "pf-1", time.Now())
	if !ok.NoMandate() {
		t.Fatal("a mandate that failed to apply was reported as governing")
	}
	if !errors.Is(err, ErrMandateUnreadable) {
		t.Fatalf("lookup err = %v, want ErrMandateUnreadable — an absent mandate and an "+
			"unreadable one must not be the same answer", err)
	}
}

// THE REFUSAL DOES NOT DEPEND ON POSTURE. RequireMandate is a policy question
// about portfolios nobody has written a mandate for. This is not that: a mandate
// EXISTS and the platform cannot read it, which is the same category as Unscoped.
func TestAnUnreadableMandateIsRefusedEvenWhenMandatesAreNotRequired(t *testing.T) {
	reg := NewMandateRegistry()
	c := NewMandateConsumer(reg, nil)
	if err := c.Handle(context.Background(), nil, configChanged(t, MandateConfigKey("t1", "p1"), "{}}")); err != nil {
		t.Fatal(err)
	}

	// requireMandate deliberately left OFF — the permissive posture.
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, nil, nil)
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(1, 0), Price: dec(1000, 0), Currency: "USD", OrderID: "o1", AsOf: t0,
	})
	if err != nil {
		t.Fatalf("an unreadable mandate is terminal and must be a verdict, not a redelivery: %v", err)
	}
	if got.Allowed {
		t.Fatal("an order was ADMITTED against a portfolio whose mandate could not be read")
	}
	if !got.Unreadable {
		t.Fatalf("decision = %+v, want Unreadable — the flag is what tells an operator "+
			"the mandate is broken rather than missing", got)
	}
	if got.Ungoverned {
		t.Fatal("an unreadable mandate was reported as Ungoverned, which is the collapse this fixes")
	}
}

// The two states must stay apart: nobody has written one, versus somebody wrote
// one and it cannot be read.
func TestAnUnreadableMandateIsDistinguishableFromAnAbsentOne(t *testing.T) {
	reg := NewMandateRegistry()
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, nil, nil)
	d := OrderDelta{
		TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(1, 0), Price: dec(1000, 0), Currency: "USD", OrderID: "o1", AsOf: t0,
	}

	absent, err := g.Evaluate(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !absent.Ungoverned || absent.Unreadable {
		t.Fatalf("a portfolio nobody has put under mandate should be Ungoverned only: %+v", absent)
	}

	if err := NewMandateConsumer(reg, nil).Handle(context.Background(), nil,
		configChanged(t, MandateConfigKey("t1", "p1"), "not-json")); err != nil {
		t.Fatal(err)
	}
	broken, err := g.Evaluate(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !broken.Unreadable || broken.Ungoverned {
		t.Fatalf("a portfolio whose mandate failed to decode should be Unreadable only: %+v", broken)
	}
}

// A REPAIR MUST CLEAR THE MARK. The operator republishes a corrected mandate on
// the same compacted subject; if the rejection were sticky the portfolio would
// stay refused forever and the fix would be worse than the defect.
func TestARepublishedMandateClearsTheRejection(t *testing.T) {
	reg := NewMandateRegistry()
	c := NewMandateConsumer(reg, nil)
	key := MandateConfigKey("acme", "pf-1")

	if err := c.Handle(context.Background(), nil, configChanged(t, key, "{broken")); err != nil {
		t.Fatal(err)
	}
	if err := c.Handle(context.Background(), nil, configChanged(t, key, goodMandateValue(t, "acme", "pf-1"))); err != nil {
		t.Fatal(err)
	}

	_, ok, err := reg.Mandate(context.Background(), "acme", "pf-1", time.Now())
	if err != nil {
		t.Fatalf("a repaired mandate still reports an error: %v", err)
	}
	if ok.NoMandate() {
		t.Fatal("a republished, valid mandate did not arm the registry")
	}
}

// The drop is COUNTED, with the portfolio named — an operator's question is which
// mandate to go and fix, and a bare total cannot answer it.
func TestADroppedMandateIsCountedAndNamed(t *testing.T) {
	var gotTenant, gotPortfolio string
	var n int
	reg := NewMandateRegistry()
	c := NewMandateConsumer(reg, nil, WithMandateRejectionObserver(func(tenantID, portfolioID string) {
		n++
		gotTenant, gotPortfolio = tenantID, portfolioID
	}))

	if err := c.Handle(context.Background(), nil,
		configChanged(t, MandateConfigKey("acme", "pf-9"), "{bad")); err != nil {
		t.Fatal(err)
	}
	if n != 1 || gotTenant != "acme" || gotPortfolio != "pf-9" {
		t.Fatalf("observer got n=%d tenant=%q portfolio=%q, want 1/acme/pf-9", n, gotTenant, gotPortfolio)
	}
}

// A payload that is not a ConfigChanged at all names no portfolio, so nothing can
// be marked — but the registry must still stop claiming it is complete, or the
// drop is invisible in exactly the way this issue is about.
func TestAnUndecodableEnvelopeIsRecordedEvenThoughNoPortfolioCanBeNamed(t *testing.T) {
	reg := NewMandateRegistry()
	c := NewMandateConsumer(reg, nil)

	if err := c.Handle(context.Background(), nil, []byte{0xff, 0xff, 0xff, 0xff}); err != nil {
		t.Fatalf("the handler must still ACK poison, got %v", err)
	}
	if reg.Complete() {
		t.Fatal("the registry reports itself complete after silently dropping a message it could not decode")
	}
	if got := reg.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
}

// A registry that folded everything it was given is complete, and a mandate with
// ZERO RULES is a legal, deliberate choice ("constrains nothing") — it must not
// be confused with a drop.
func TestACompleteRegistrySaysSoAndAnEmptyRulesetIsNotADrop(t *testing.T) {
	reg := NewMandateRegistry()
	c := NewMandateConsumer(reg, nil)
	if err := c.Handle(context.Background(), nil,
		configChanged(t, MandateConfigKey("acme", "pf-1"), goodMandateValue(t, "acme", "pf-1"))); err != nil {
		t.Fatal(err)
	}
	if !reg.Complete() {
		t.Fatal("a registry that applied every message reports itself incomplete")
	}
	if reg.Dropped() != 0 {
		t.Fatalf("Dropped() = %d, want 0", reg.Dropped())
	}
}

// A BROKEN MANDATE SUPERSEDES A GOOD ONE ALREADY IN MEMORY, and that is the safe
// direction rather than an oversight.
//
// The stream keeps ONE message per (tenant, portfolio) subject. Once v2 is
// published the earlier version is gone from it, so a pod booting now sees only
// the broken v2 and has nothing to fall back to. Continuing to trade this process
// under a v1 the operator has REPLACED would mean two pods of the same service
// enforcing different rules on the same book, and the one still enforcing v1 would
// be enforcing rules nobody has approved any more.
func TestABrokenMandateSupersedesAValidOneAlreadyHeld(t *testing.T) {
	reg := NewMandateRegistry()
	c := NewMandateConsumer(reg, nil)
	key := MandateConfigKey("acme", "pf-1")

	if err := c.Handle(context.Background(), nil, configChanged(t, key, goodMandateValue(t, "acme", "pf-1"))); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := reg.Mandate(context.Background(), "acme", "pf-1", time.Now()); ok.NoMandate() || err != nil {
		t.Fatalf("premise broken: the good mandate did not arm (ok=%v err=%v)", ok, err)
	}

	if err := c.Handle(context.Background(), nil, configChanged(t, key, "{superseded-but-broken")); err != nil {
		t.Fatal(err)
	}
	_, ok, err := reg.Mandate(context.Background(), "acme", "pf-1", time.Now())
	if !ok.NoMandate() {
		t.Fatal("still governing under a mandate version the operator has replaced")
	}
	if !errors.Is(err, ErrMandateUnreadable) {
		t.Fatalf("err = %v, want ErrMandateUnreadable", err)
	}
}
