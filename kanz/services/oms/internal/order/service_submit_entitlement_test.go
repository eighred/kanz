package order

import (
	"context"
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/platform/halt"
)

// SUBMIT SKIPPED THE CHECK CANCEL AND AMEND BOTH RAN (#225).
//
// handleCancel and handleAmend called entitledTo immediately after store.Load,
// under "Entitlement, BEFORE any side effect". handleSubmit — 350 lines of
// decimal domain, dedup/resume, compliance gate, venue check, account resolution
// and store.Create — never consulted it. Combined with the OIDC bridge dropping
// the claim, that meant a caller scoped to `research` could OPEN a position in
// `flagship` and then be refused every attempt to close it: a position you can
// enter through the API and cannot exit through the API.
//
// These tests assert the ABSENCE OF THE SIDE EFFECT, not just the outcome code.
// A refusal that still admitted the order, or still reached the venue, has done
// the damage whatever the ledger says — the same standard
// TestCancel_RefusesPrincipalNotEntitledToTheOrdersPortfolio holds cancel to.

// countingStore wraps a Store and records how many times Create was called, so a
// test can prove the order was never admitted rather than inferring it from a
// later Load miss.
type countingStore struct {
	Store
	creates int
}

func (c *countingStore) Create(ctx context.Context, st *orderpb.OrderState, announce []outbox.Record) error {
	c.creates++
	return c.Store.Create(ctx, st, announce)
}

// submitAs builds the "o1"/"pf1" limit order carrying a DELEGATED issuer — the
// shape services/api-gateway/internal/orders/orders.go's bindMetadata mints for
// an authenticated human, where Issuer and PrincipalPortfolios both come from
// the verified principal.
func submitAs(portfolios ...string) *orderpb.SubmitOrder {
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Metadata = &commandpb.CommandMetadata{
		Issuer:              "user:mallory",
		TargetId:            cmd.GetOrderId(),
		PrincipalPortfolios: portfolios,
	}
	return cmd
}

func submitService(t *testing.T, store Store) (*Service, *fakeBus, *closerVenue) {
	t.Helper()
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatal(err)
	}
	return svc, fb, venue
}

// The core gap: same tenant, valid command, a portfolio the caller does not hold.
func TestSubmit_RefusesPrincipalNotEntitledToTheOrdersPortfolio(t *testing.T) {
	store := &countingStore{Store: NewMemoryStore()}
	svc, fb, _ := submitService(t, store)

	if err := svc.Handle(testCtx(), submitEnvFor(testTenant), mustMarshal(t, submitAs("pf2", "pf3"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// THE SIDE EFFECT, ASSERTED AS AN ABSENCE. store.Create is the admission
	// gate; an order that reached it exists, is swept on restart, and can be
	// routed. Counting the call rather than checking a later Load means a future
	// "create then delete on refusal" cannot pass this test.
	if store.creates != 0 {
		t.Fatalf("store.Create called %d times — an unentitled caller OPENED a position "+
			"in a portfolio they hold no entitlement to", store.creates)
	}
	if _, _, err := store.Load(context.Background(), "o1"); err == nil {
		t.Fatal("the order was admitted despite the refusal")
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome = %v, want REJECTED", oc.GetStatus())
	}
	if oc.GetErrorCode() != ReasonNotEntitled {
		t.Fatalf("outcome code = %q, want %q", oc.GetErrorCode(), ReasonNotEntitled)
	}
}

// AN ABSENT ALLOW-LIST DENIES, and this is the live production shape until the
// IdP issues the claim (#99). It is the row that makes the "just make it
// permissive" hotfix an authorization bypass: permitting here authorizes every
// authenticated caller against every portfolio in the tenant.
func TestSubmit_RefusesWhenPrincipalCarriesNoPortfolioScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		portfolios []string
	}{
		{"nil", nil},
		{"empty", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &countingStore{Store: NewMemoryStore()}
			svc, fb, _ := submitService(t, store)

			if err := svc.Handle(testCtx(), submitEnvFor(testTenant), mustMarshal(t, submitAs(tc.portfolios...))); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if store.creates != 0 {
				t.Fatalf("store.Create called %d times — an empty allow-list admitted an order. "+
					"On the capital path the absence of proof is not proof", store.creates)
			}
			oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
			if oc.GetErrorCode() != ReasonNotEntitled {
				t.Fatalf("outcome code = %q, want %q", oc.GetErrorCode(), ReasonNotEntitled)
			}
		})
	}
}

// The positive control. Without it the two refusals above would also pass on a
// handler that refused everything.
func TestSubmit_AdmitsAPrincipalEntitledToThePortfolio(t *testing.T) {
	store := &countingStore{Store: NewMemoryStore()}
	svc, _, _ := submitService(t, store)

	if err := svc.Handle(testCtx(), submitEnvFor(testTenant), mustMarshal(t, submitAs("pf9", "pf1"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if store.creates != 1 {
		t.Fatalf("store.Create called %d times, want 1 — an entitled caller was refused", store.creates)
	}
	if _, _, err := store.Load(context.Background(), "o1"); err != nil {
		t.Fatalf("the order was not admitted: %v", err)
	}
}

// A SERVICE-ISSUED ORDER HAS NO HUMAN AND NO CLAIM, AND MUST STILL TRADE.
//
// Two producers besides the gateway publish order.order.submit — webhook-ingest
// fanning a strategy signal out per venue (Issuer "strategy:{id}") and
// optimization materializing a rebalance (services/optimization/internal/bridge)
// — and neither has a portfolio claim to carry, because neither is acting for a
// person. Running the human rule over them would refuse every automated order on
// the platform NOT_ENTITLED, with no claim an operator could populate to clear
// it: a bigger outage than the one #225 reports.
//
// This is the test that makes the delegated-issuer discriminator load-bearing.
// Delete it and the "simplification" of checking every submit unconditionally
// looks correct right up to the point the automated path stops trading.
func TestSubmit_ServiceIssuedOrderIsNotSubjectToTheHumanEntitlementRule(t *testing.T) {
	for _, issuer := range []string{"strategy:momentum-1", "optimization:rebalance", ""} {
		t.Run("issuer="+issuer, func(t *testing.T) {
			store := &countingStore{Store: NewMemoryStore()}
			svc, _, _ := submitService(t, store)

			cmd := limitOrder(d(100, 0), d(1025, -2))
			cmd.Metadata = &commandpb.CommandMetadata{Issuer: issuer, TargetId: cmd.GetOrderId()}

			if err := svc.Handle(testCtx(), submitEnvFor(testTenant), mustMarshal(t, cmd)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if store.creates != 1 {
				t.Fatalf("store.Create called %d times, want 1 — a service-issued order was "+
					"refused for want of a portfolio claim it can never have", store.creates)
			}
		})
	}
}

// A SUBMIT NAMING AN ORDER THAT ALREADY EXISTS REACHES resume(), WHICH QUERIES
// THE VENUE. Checking only the portfolio the COMMAND names would leave that door
// open: an unentitled caller names their own portfolio, the order_id belongs to
// somebody else, and resume can re-drive or close a live order they have no
// claim to. The stored order's portfolio is the one that governs here.
func TestSubmit_CannotDriveAnExistingOrderInAnotherPortfolio(t *testing.T) {
	store := &countingStore{Store: NewMemoryStore()}
	svc, fb, _ := submitService(t, store)

	// Admit "o1" on "pf1" as its rightful owner.
	if err := svc.Handle(testCtx(), submitEnvFor(testTenant), mustMarshal(t, submitAs("pf1"))); err != nil {
		t.Fatalf("admit: %v", err)
	}
	admitted, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	before := len(fb.types())

	// Mallory resubmits the same order_id, naming a portfolio she DOES hold.
	mallory := submitAs("pf-mallory")
	mallory.PortfolioId = "pf-mallory"
	if err := svc.Handle(testCtx(), submitEnvFor(testTenant), mustMarshal(t, mallory)); err != nil {
		t.Fatalf("resubmit: %v", err)
	}

	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetErrorCode() != ReasonNotEntitled {
		t.Fatalf("outcome code = %q, want %q — an unentitled resubmit reached resume(), "+
			"which queries the venue and can re-drive somebody else's live order",
			oc.GetErrorCode(), ReasonNotEntitled)
	}
	// outcomeReject, NOT refuse: the order exists and may be working at a venue,
	// so no ORDER_REJECTED FACT may be emitted for it.
	for _, et := range fb.types()[before:] {
		if et == EventTypeRejected {
			t.Fatal("an ORDER_REJECTED FACT was emitted for a live order — every downstream " +
				"fold now believes it was refused")
		}
	}
	// The victim's order is untouched: same portfolio, same status.
	after, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if after.GetPortfolioId() != "pf1" || after.GetStatus() != admitted.GetStatus() {
		t.Fatalf("order changed under an unentitled resubmit: portfolio %q status %v, want pf1 %v",
			after.GetPortfolioId(), after.GetStatus(), admitted.GetStatus())
	}
}
