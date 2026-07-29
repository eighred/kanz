package order

import (
	"context"
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// Order-ownership enforcement.
//
// Tenant isolation (per-tenant OMS + FORCE RLS) already prevents reaching
// ANOTHER TENANT's order. It says nothing about WHICH CALLER inside the tenant
// may act on one: `orders` carries no owner column, and the gateway stamps the
// caller only as audit metadata. So every holder of the coarse trade capability
// could cancel or amend any order in the tenant, including one belonging to a
// portfolio they have no entitlement to.
//
// The entitlement and the order live in different processes — the gateway knows
// the principal but never reads the order; the OMS holds the order but has no
// identity store — so the caller's scope arrives on CommandMetadata and is
// checked HERE, against the order's own portfolio_id.
//
// The helpers (restingOrderOn, closerVenue, cancelEnv, mustMarshal, fakeBus)
// live in service_close_test.go; limitOrder admits order "o1" on portfolio "pf1".

// cancelAs builds a cancel for "o1" issued by a principal entitled to exactly
// the given portfolios.
func cancelAs(portfolios ...string) *orderpb.CancelOrder {
	return &orderpb.CancelOrder{
		OrderId: "o1",
		Metadata: &commandpb.CommandMetadata{
			Issuer:              "user:mallory",
			TargetId:            "o1",
			PrincipalPortfolios: portfolios,
		},
	}
}

// A caller entitled only to OTHER portfolios must not be able to cancel this
// order. This is the gap itself: same tenant, valid trade capability, someone
// else's position.
func TestCancel_RefusesPrincipalNotEntitledToTheOrdersPortfolio(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, _ := restingOrderOn(t, fb, venue)

	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf2", "pf3"))); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The refusal must reach the EXCHANGE boundary, not merely the ledger: an
	// unauthorized cancel that still withdraws the order at the venue has done
	// the damage regardless of what the ledger records.
	if len(venue.cancelled) != 0 {
		t.Fatalf("venue cancels = %v, want none — an unentitled caller reached the exchange", venue.cancelled)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome = %v, want REJECTED", oc.GetStatus())
	}
	// The order must still be live and cancellable by someone who IS entitled.
	st, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatal("order was CANCELLED by a caller with no entitlement to its portfolio")
	}
}

// An ABSENT allow-list denies. pkg/auth's PolicyAuthorizer treats an empty
// portfolio claim as UNRESTRICTED, and that default is wrong on the capital
// path: it means a dropped claim, a misconfigured issuer, or a command minted
// without scope silently authorizes everything. Empty means deny here.
func TestCancel_RefusesWhenPrincipalCarriesNoPortfolioScope(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, _ := restingOrderOn(t, fb, venue)

	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs())); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if len(venue.cancelled) != 0 {
		t.Fatalf("venue cancels = %v, want none — an unscoped command must not reach the exchange", venue.cancelled)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome = %v, want REJECTED — an empty allow-list must fail CLOSED", oc.GetStatus())
	}
}

// NON-VACUITY: the entitled caller must still succeed. Without this, a guard
// that rejected every cancel would pass both tests above and look correct while
// having broken the cancel path entirely.
func TestCancel_AllowsPrincipalEntitledToTheOrdersPortfolio(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, _ := restingOrderOn(t, fb, venue)

	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if len(venue.cancelled) != 1 || venue.cancelled[0] != "o1" {
		t.Fatalf("venue cancels = %v, want [o1] — the ENTITLED caller's cancel must still reach the exchange", venue.cancelled)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v, want EXECUTED", oc.GetStatus())
	}
}

// amendEnv routes a command to the amend handler.
func amendEnv() *envelopepb.Envelope { return &envelopepb.Envelope{EventType: SubjectAmend} }

// Amend carries the same exposure as cancel — it rewrites the size or price of
// someone else's live order — so it must refuse the same caller.
func TestAmend_RefusesPrincipalNotEntitledToTheOrdersPortfolio(t *testing.T) {
	fb := &fakeBus{}
	svc, _ := restingOrderOn(t, fb, &closerVenue{mic: "BINANCE"})

	amend := &orderpb.AmendOrder{
		OrderId:     "o1",
		NewQuantity: d(1, 0),
		Metadata: &commandpb.CommandMetadata{
			Issuer: "user:mallory", TargetId: "o1", PrincipalPortfolios: []string{"pf2"},
		},
	}
	if err := svc.Handle(testCtx(), amendEnv(), mustMarshal(t, amend)); err != nil {
		t.Fatalf("amend: %v", err)
	}

	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome = %v, want REJECTED", oc.GetStatus())
	}
	// The order's quantity must be untouched — a rejected amend that still
	// mutated state would be the same breach with a different label.
	st, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := st.GetOrderedQuantity().GetCoefficient(); got != 100 {
		t.Fatalf("quantity = %d, want 100 — an unentitled caller resized the order", got)
	}
}

// A QUARANTINED ORDER MUST REFUSE AN AMEND, for the same reason it refuses a
// cancel: quarantine means the platform could not establish what the venue did
// with this order, and rewriting its size or price is a guess about a state
// nobody can currently confirm. IsTerminal does not catch a quarantined order
// (it commonly sits at ROUTED), so without this refusal Amend() would apply
// and persist the new quantity with no venue agreement that the order — or
// this version of it — even exists to amend.
func TestAmend_RefusesQuarantinedOrder(t *testing.T) {
	fb := &fakeBus{}
	svc, _ := restingOrderOn(t, fb, &closerVenue{mic: "BINANCE"})

	st, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := svc.quarantine(context.Background(), st, "test: venue truth could not be established"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	amend := &orderpb.AmendOrder{
		OrderId:     "o1",
		NewQuantity: d(1, 0),
		Metadata: &commandpb.CommandMetadata{
			Issuer: "user:owner", TargetId: "o1", PrincipalPortfolios: []string{"pf1"},
		},
	}
	if err := svc.Handle(testCtx(), amendEnv(), mustMarshal(t, amend)); err != nil {
		t.Fatalf("amend: %v", err)
	}

	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED || oc.GetErrorCode() != "ORDER_QUARANTINED" {
		t.Fatalf("outcome = %v/%q, want REJECTED/ORDER_QUARANTINED", oc.GetStatus(), oc.GetErrorCode())
	}
	// The order's quantity must be untouched — a rejected amend that still
	// mutated state would be the same breach with a different label.
	st, err = svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := st.GetOrderedQuantity().GetCoefficient(); got != 100 {
		t.Fatalf("quantity = %d, want 100 — a quarantined order was amended", got)
	}
}

// NON-VACUITY for amend: the entitled caller must still be able to amend.
func TestAmend_AllowsPrincipalEntitledToTheOrdersPortfolio(t *testing.T) {
	fb := &fakeBus{}
	svc, _ := restingOrderOn(t, fb, &closerVenue{mic: "BINANCE"})

	amend := &orderpb.AmendOrder{
		OrderId:     "o1",
		NewQuantity: d(50, 0),
		Metadata: &commandpb.CommandMetadata{
			Issuer: "user:owner", TargetId: "o1", PrincipalPortfolios: []string{"pf1"},
		},
	}
	if err := svc.Handle(testCtx(), amendEnv(), mustMarshal(t, amend)); err != nil {
		t.Fatalf("amend: %v", err)
	}

	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v, want EXECUTED", oc.GetStatus())
	}
	st, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := st.GetOrderedQuantity().GetCoefficient(); got != 50 {
		t.Fatalf("quantity = %d, want 50 — the ENTITLED caller's amend did not apply", got)
	}
}
