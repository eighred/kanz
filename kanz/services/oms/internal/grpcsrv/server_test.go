package grpcsrv_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
)

// fakeReader is a stand-in for the order store's read half.
type fakeReader struct {
	orders    []*orderpb.OrderState
	unindexed int64
	err       error

	gotPortfolio string
	gotLimit     int
	calls        int
}

func (f *fakeReader) ListByPortfolio(_ context.Context, portfolio string, limit int) ([]*orderpb.OrderState, int64, error) {
	f.calls++
	f.gotPortfolio, f.gotLimit = portfolio, limit
	return f.orders, f.unindexed, f.err
}

func order(id, portfolio string) *orderpb.OrderState {
	return &orderpb.OrderState{OrderId: id, PortfolioId: portfolio}
}

// THE OWNERSHIP STAMP IS THE GATE INPUT, and it matters more on a list than on a
// single read: a caller naming an order already knows its id, while this hands
// over a portfolio's whole trading history.
func TestTheReplyCarriesTheOwningTenant(t *testing.T) {
	store := &fakeReader{orders: []*orderpb.OrderState{order("o1", "flagship")}}
	resp, err := grpcsrv.New(store, "acme").ListOrders(context.Background(),
		&orderpb.ListOrdersRequest{PortfolioId: "flagship"})
	if err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if resp.GetOwnerTenant() != "acme" {
		t.Fatalf("owner_tenant = %q, want acme — without it a governed client cannot enforce "+
			"cross-tenant isolation and its gate always passes", resp.GetOwnerTenant())
	}
}

// AN UNCONFIGURED TENANT FAILS CLOSED. Empty leaves the field blank, which a
// deny-by-default gate reads as a denial rather than an allow.
func TestAnUnconfiguredTenantLeavesTheStampEmpty(t *testing.T) {
	store := &fakeReader{orders: []*orderpb.OrderState{order("o1", "flagship")}}
	resp, err := grpcsrv.New(store, "").ListOrders(context.Background(),
		&orderpb.ListOrdersRequest{PortfolioId: "flagship"})
	if err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if resp.GetOwnerTenant() != "" {
		t.Fatalf("owner_tenant = %q, want empty", resp.GetOwnerTenant())
	}
}

// AN EMPTY PORTFOLIO IS REFUSED, NOT WIDENED.
//
// In the orders table an empty portfolio_id is migration 0007's NOT-INDEXED
// marker. Treating the request as "all portfolios" would return exactly the
// orders whose portfolio nobody can attribute — the least defensible possible
// answer to a question about one portfolio — and it would do so while looking
// like a generous default.
func TestAnEmptyPortfolioIsRefusedAndNeverReachesTheStore(t *testing.T) {
	store := &fakeReader{}
	_, err := grpcsrv.New(store, "acme").ListOrders(context.Background(),
		&orderpb.ListOrdersRequest{})

	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
	if store.calls != 0 {
		t.Fatalf("the store was queried %d times for an empty portfolio — the refusal must come "+
			"before the read, or the NOT-INDEXED rows are what comes back", store.calls)
	}
}

// unindexed IS PART OF THE ANSWER. Dropping it leaves the caller presenting one
// page as a portfolio's whole history.
func TestTheCountOfUnindexedOrdersIsPassedThrough(t *testing.T) {
	store := &fakeReader{orders: []*orderpb.OrderState{order("o1", "flagship")}, unindexed: 412}
	resp, err := grpcsrv.New(store, "acme").ListOrders(context.Background(),
		&orderpb.ListOrdersRequest{PortfolioId: "flagship"})
	if err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if resp.GetUnindexed() != 412 {
		t.Fatalf("unindexed = %d, want 412 — orders that predate the portfolio index cannot "+
			"appear in any page, so the count is the only way a caller can say so",
			resp.GetUnindexed())
	}
}

func TestTheRequestReachesTheStoreUnchanged(t *testing.T) {
	store := &fakeReader{}
	if _, err := grpcsrv.New(store, "acme").ListOrders(context.Background(),
		&orderpb.ListOrdersRequest{PortfolioId: "flagship", Limit: 25}); err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if store.gotPortfolio != "flagship" || store.gotLimit != 25 {
		t.Fatalf("store received (%q, %d), want (flagship, 25)", store.gotPortfolio, store.gotLimit)
	}
}

// THE STORE'S MESSAGE IS NOT RELAYED. It carries connection strings, table names
// and driver detail — an internal description of the OMS's plumbing, and a read
// surface is not where that gets published.
func TestAStoreFailureDoesNotLeakItsMessage(t *testing.T) {
	store := &fakeReader{err: errors.New(`dial tcp 10.4.2.9:5432: connect: connection refused (db "kanzapp" user "kanzapp")`)}
	_, err := grpcsrv.New(store, "acme").ListOrders(context.Background(),
		&orderpb.ListOrdersRequest{PortfolioId: "flagship"})

	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", status.Code(err))
	}
	if got := status.Convert(err).Message(); got == "" || contains(got, "5432") || contains(got, "kanzapp") {
		t.Fatalf("message %q relays the store's internals", got)
	}
}

// A CANCELLED CALLER IS NOT AN UNAVAILABLE STORE. Reporting one as the other
// sends an operator to look at Postgres for a client that gave up.
func TestACancelledRequestIsNotReportedAsAStoreFailure(t *testing.T) {
	store := &fakeReader{err: context.Canceled}
	_, err := grpcsrv.New(store, "acme").ListOrders(context.Background(),
		&orderpb.ListOrdersRequest{PortfolioId: "flagship"})

	if status.Code(err) != codes.Canceled {
		t.Fatalf("code = %v, want Canceled", status.Code(err))
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
