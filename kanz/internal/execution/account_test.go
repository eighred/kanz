package execution

import (
	"context"
	"errors"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// A SHARED ACCOUNT IS NOT A CONFIG TYPO. IT IS A LIE IN THE BOOKS.
//
// Bind two portfolios to one exchange account and the ledger will report each of them
// holding its own cash, while the exchange holds ONE pool it will liquidate as one. A
// drawdown in the first eats the second's margin, and nothing in the second's books
// will say so until the margin call.
//
// So this is refused at construction, and the OMS refuses to start on it.
func TestParseBindings_RefusesAnAccountSharedByTwoPortfolios(t *testing.T) {
	_, err := ParseBindings("acme/fund-alpha@XNAS=okx-sub-1,acme/fund-beta@XNAS=okx-sub-1")
	if !errors.Is(err, ErrAccountShared) {
		t.Fatalf("err = %v, want ErrAccountShared — two portfolios were given ONE collateral pool "+
			"and the platform accepted it", err)
	}
}

// The same account across tenants is the same failure, and worse: it is two CUSTOMERS
// in one collateral pool.
func TestParseBindings_RefusesAnAccountSharedAcrossTenants(t *testing.T) {
	_, err := ParseBindings("acme/fund-a@XNAS=shared-1,rival/fund-b@XNAS=shared-1")
	if !errors.Is(err, ErrAccountShared) {
		t.Fatalf("err = %v, want ErrAccountShared — two TENANTS were given one collateral pool", err)
	}
}

// A portfolio holding one account per venue is the normal, correct case.
func TestParseBindings_OnePortfolioMayHoldAnAccountAtEachVenue(t *testing.T) {
	b, err := ParseBindings("acme/fund-alpha@XNAS=okx-sub-1,acme/fund-alpha@XLON=binance-main")
	if err != nil {
		t.Fatalf("ParseBindings: %v", err)
	}
	if a, ok := b.Account("acme", "fund-alpha", "XNAS"); !ok || a != "okx-sub-1" {
		t.Fatalf("XNAS account = %q, %v; want okx-sub-1", a, ok)
	}
	if a, ok := b.Account("acme", "fund-alpha", "XLON"); !ok || a != "binance-main" {
		t.Fatalf("XLON account = %q, %v; want binance-main", a, ok)
	}
	if _, ok := b.Account("acme", "fund-beta", "XNAS"); ok {
		t.Fatal("fund-beta is bound to nothing and must resolve to nothing — an unbound portfolio " +
			"inheriting somebody else's account is the whole failure")
	}
}

func TestParseBindings_Empty(t *testing.T) {
	b, err := ParseBindings("")
	if err != nil || !b.Empty() {
		t.Fatalf("empty spec: b.Empty()=%v err=%v", b.Empty(), err)
	}
	if _, ok := b.Account("acme", "fund-alpha", "XNAS"); ok {
		t.Fatal("empty bindings resolved an account — nothing bound must mean NOTHING bound")
	}
}

// THE ROUTER IS THE LAST PLACE THE MONEY CAN BE MISDIRECTED.
//
// An adapter holds one API credential and therefore IS one exchange account. Route on
// the MIC alone and Basket Beta's order reaches whichever adapter is configured for
// that venue — including the one holding Basket Alpha's credential, whose collateral it
// then margins against. That is cross-collateralization happening in a for-loop.
func TestRoute_WillNotSendAnOrderToAnotherPortfoliosCollateral(t *testing.T) {
	alpha := NewSimVenue("XNAS", WithAccount("okx-alpha"))
	beta := NewSimVenue("XNAS", WithAccount("okx-beta")) // same venue, different account
	r := NewRouter(alpha, beta)

	// An order bound to beta's account must reach beta's adapter — never alpha's,
	// which is the first one configured and the one a MIC-only router would pick.
	v, err := r.Route(&orderpb.OrderState{Venue: "XNAS", VenueAccountId: "okx-beta"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if v.Account() != "okx-beta" {
		t.Fatalf("routed to account %q, want okx-beta — the order was sent to another portfolio's "+
			"collateral", v.Account())
	}
}

// An order naming an account this OMS cannot reach can NEVER be executed — routing it
// anywhere else would put it on somebody else's collateral, which is exactly what its
// binding forbids. It is refused, never quietly rested.
func TestRoute_RefusesAnAccountNoAdapterHolds(t *testing.T) {
	r := NewRouter(NewSimVenue("XNAS", WithAccount("okx-alpha")))

	_, err := r.Route(&orderpb.OrderState{Venue: "XNAS", VenueAccountId: "okx-beta"})
	if !errors.Is(err, ErrVenueNotConfigured) {
		t.Fatalf("err = %v, want ErrVenueNotConfigured — the router found SOME adapter at XNAS and "+
			"used it, which means it used the wrong account's collateral", err)
	}
}

func TestSupportsAndAccountFor(t *testing.T) {
	r := NewRouter(NewSimVenue("XNAS", WithAccount("okx-alpha")))

	if !r.Supports("XNAS", "") {
		t.Fatal("Supports(XNAS) = false")
	}
	if !r.Supports("XNAS", "okx-alpha") {
		t.Fatal("Supports(XNAS, okx-alpha) = false")
	}
	if r.Supports("XNAS", "okx-beta") {
		t.Fatal("Supports reported an account no adapter holds — the OMS would admit an order it " +
			"can only execute against the wrong collateral")
	}
	if a, ok := r.AccountFor("XNAS"); !ok || a != "okx-alpha" {
		t.Fatalf("AccountFor(XNAS) = %q, %v; want okx-alpha", a, ok)
	}
}

// A fill reports the account it SETTLED against. The ledger posts where the cash went,
// not where it was meant to go.
func TestSimVenue_FillCarriesTheAccountThatExecutedIt(t *testing.T) {
	v := NewSimVenue("XNAS", WithAccount("okx-alpha"))
	st := &orderpb.OrderState{
		OrderId:        "o1",
		InstrumentId:   "BTC-USD",
		Side:           orderpb.Side_SIDE_BUY,
		OrderType:      orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:     d(50000, 0),
		LeavesQuantity: d(1, 0),
	}
	fills, err := v.Execute(context.Background(), st)
	if err != nil || len(fills) != 1 {
		t.Fatalf("Execute: %d fills, err=%v", len(fills), err)
	}
	if got := fills[0].GetVenueAccountId(); got != "okx-alpha" {
		t.Fatalf("fill.venue_account_id = %q, want okx-alpha — the ledger cannot post collateral it "+
			"cannot locate", got)
	}
}
