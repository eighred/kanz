package order

import (
	"context"
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
)

// WHOSE COLLATERAL DOES AN ORDER SPEND?
//
// An exchange margins, nets and LIQUIDATES per ACCOUNT. Two portfolios settling into
// one exchange account are ONE collateral pool, whatever the ledger says: a drawdown in
// the first triggers a liquidation, the exchange sells what is in the account, and the
// second portfolio's margin is gone while its books still show the cash.
//
// The OMS is where that is decided, because it is the last thing that touches an order
// before a credential does.

func submitEnvFor(tenant string) *envelopepb.Envelope {
	return &envelopepb.Envelope{EventType: SubjectSubmit, TenantId: tenant}
}

// BOUND: the order executes against ITS OWN account, and the state records which.
func TestSubmit_StampsThePortfoliosBoundAccount(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	bindings := mustBind(t, "acme/fund-alpha@XSIM=okx-alpha")
	router := execution.NewRouter(execution.NewSimVenue("XSIM", execution.WithAccount("okx-alpha")))

	svc, err := NewService(store, NewEmitter(fb), nil, router, nil, nil,
		WithAccountBindings(bindings, true, nil))
	if err != nil {
		t.Fatal(err)
	}
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XSIM"
	cmd.PortfolioId = "fund-alpha"

	if err := svc.Handle(testCtx(), submitEnvFor("acme"), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	st, err := store.Load(context.Background(), cmd.GetOrderId())
	if err != nil {
		t.Fatalf("the order was not admitted: %v", err)
	}
	if got := st.GetVenueAccountId(); got != "okx-alpha" {
		t.Fatalf("order.venue_account_id = %q, want okx-alpha — the order was admitted without the "+
			"platform recording whose collateral it spends", got)
	}
}

// UNBOUND + REQUIRED: refused, under its own code.
//
// Nothing is broken and no rule was breached. Nobody has said which collateral this
// portfolio may spend, and the platform will not guess — because guessing means
// spending somebody else's.
func TestSubmit_UnboundPortfolioIsRefusedWhenAccountsAreRequired(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	bindings := mustBind(t, "acme/fund-alpha@XSIM=okx-alpha") // fund-beta is bound to NOTHING
	router := execution.NewRouter(execution.NewSimVenue("XSIM", execution.WithAccount("okx-alpha")))

	svc, err := NewService(store, NewEmitter(fb), nil, router, nil, nil,
		WithAccountBindings(bindings, true, nil))
	if err != nil {
		t.Fatal(err)
	}
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XSIM"
	cmd.PortfolioId = "fund-beta"

	if err := svc.Handle(testCtx(), submitEnvFor("acme"), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Refused at ADMISSION: no order exists, and nothing was routed or filled. Had it
	// been admitted, it would have executed on okx-alpha — fund-alpha's collateral —
	// because that is the only adapter at XSIM.
	if _, lerr := store.Load(context.Background(), cmd.GetOrderId()); lerr == nil {
		t.Fatal("an order for a portfolio bound to NO account was admitted. It can only execute " +
			"against somebody else's collateral, and the ledger would report both funds' cash intact")
	}
	for _, ev := range fb.types() {
		if ev == EventTypeRouted || ev == EventTypeFilled {
			t.Fatalf("the unbound order was %s — it reached a credential", ev)
		}
	}
	oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc == nil || oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatal("the strategy was not told its order was refused")
	}
	if oc.GetErrorCode() != "VENUE_ACCOUNT_UNBOUND" {
		t.Errorf("rejection code = %q, want VENUE_ACCOUNT_UNBOUND — this is not a breach and not a "+
			"misconfigured venue; nothing GOVERNS whose collateral this portfolio may spend",
			oc.GetErrorCode())
	}
}

// UNBOUND + ADVISORY: it trades — and the platform records the account it ACTUALLY hit.
//
// This is the default state of a platform that has never configured a binding, and the
// point is that the shared pool becomes VISIBLE. Stamping the real account is what puts
// it in the ledger instead of hiding it: two funds, one account, on the record.
func TestSubmit_UnboundPortfolioTradesTheSharedAccountAndSaysSo(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	router := execution.NewRouter(execution.NewSimVenue("XSIM", execution.WithAccount("shared-pool")))

	svc, err := NewService(store, NewEmitter(fb), nil, router, nil, nil,
		WithAccountBindings(mustBind(t, ""), false, nil)) // nothing bound, advisory
	if err != nil {
		t.Fatal(err)
	}
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XSIM"
	cmd.PortfolioId = "fund-beta"

	if err := svc.Handle(testCtx(), submitEnvFor("acme"), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	st, err := store.Load(context.Background(), cmd.GetOrderId())
	if err != nil {
		t.Fatalf("the order was refused in ADVISORY mode: %v", err)
	}
	if got := st.GetVenueAccountId(); got != "shared-pool" {
		t.Fatalf("order.venue_account_id = %q, want shared-pool. An unbound order still spends a REAL "+
			"account's collateral; leaving the field empty is the ledger pretending it did not", got)
	}
}

// BOUND TO AN ACCOUNT THIS OMS CANNOT REACH: refused, never rested.
//
// Working it anywhere else means working it on another portfolio's collateral, which is
// precisely what the binding forbids. So it can never be executed — and an order that
// can never be executed is refused at admission (EXEC-M8), not left looking like it is
// working.
func TestSubmit_BoundToAnAccountNoAdapterHolds_IsRefused(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	bindings := mustBind(t, "acme/fund-alpha@XSIM=okx-alpha")
	// The only adapter at XSIM holds a DIFFERENT account.
	router := execution.NewRouter(execution.NewSimVenue("XSIM", execution.WithAccount("okx-beta")))

	svc, err := NewService(store, NewEmitter(fb), nil, router, nil, nil,
		WithAccountBindings(bindings, true, nil))
	if err != nil {
		t.Fatal(err)
	}
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XSIM"
	cmd.PortfolioId = "fund-alpha"

	if err := svc.Handle(testCtx(), submitEnvFor("acme"), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, lerr := store.Load(context.Background(), cmd.GetOrderId()); lerr == nil {
		t.Fatal("the order was admitted although the only adapter at its venue holds a DIFFERENT " +
			"account — it could only ever have executed on the wrong collateral")
	}
	oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetErrorCode() != "VENUE_ACCOUNT_UNREACHABLE" {
		t.Errorf("rejection code = %q, want VENUE_ACCOUNT_UNREACHABLE", oc.GetErrorCode())
	}
}

// The fill carries the account that executed it, all the way to the ledger's producer.
func TestSubmit_FillCarriesTheAccountItSettledAgainst(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	router := execution.NewRouter(execution.NewSimVenue("XSIM", execution.WithAccount("okx-alpha")))

	svc, err := NewService(store, NewEmitter(fb), nil, router, nil, nil,
		WithAccountBindings(mustBind(t, "acme/fund-alpha@XSIM=okx-alpha"), true, nil))
	if err != nil {
		t.Fatal(err)
	}
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XSIM"
	cmd.PortfolioId = "fund-alpha"

	if err := svc.Handle(testCtx(), submitEnvFor("acme"), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	filled, _ := fb.last(EventTypeFilled).(*orderpb.OrderFilled)
	if filled == nil {
		t.Fatal("the order did not fill")
	}
	if got := filled.GetFill().GetVenueAccountId(); got != "okx-alpha" {
		t.Fatalf("fill.venue_account_id = %q, want okx-alpha — the ledger posts where the cash went, "+
			"and it cannot if the fill does not say", got)
	}
}

func mustBind(t *testing.T, spec string) *execution.AccountBindings {
	t.Helper()
	b, err := execution.ParseBindings(spec)
	if err != nil {
		t.Fatalf("ParseBindings(%q): %v", spec, err)
	}
	return b
}
