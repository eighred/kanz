// Package grpcsrv adapts the order store to the order.v1 read surface (#399).
//
// IT IS A THIN ADAPTER OVER THE STORE, AND THAT IS THE WHOLE DESIGN. The OMS's
// orders table IS the book; this package reads it and stamps the tenant. There
// is deliberately no projection, no cache and no second copy — a projection
// would be a second order book, and the day it disagreed with the OMS nobody
// would know which one the fund had actually traded.
//
// IT SERVES READS AND NEVER ACCEPTS AN ORDER. Commands arrive on the bus and
// nowhere else, so admission, the compliance gate and the outbox stay on one
// path. A gRPC surface that could submit would be a second door into the capital
// path with none of that behind it.
package grpcsrv

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// Reader is the half of order.Store this surface needs. Narrow on purpose: a
// read server holding the full Store could Create or Save, and the compiler
// should be what stops that rather than a review.
type Reader interface {
	ListByPortfolio(ctx context.Context, portfolioID string, limit int) ([]*orderpb.OrderState, int64, error)
}

// Server implements order.v1.OrderQueryService over a Reader.
type Server struct {
	orderpb.UnimplementedOrderQueryServiceServer
	store Reader
	// ownerTenant is the tenant that owns every order this OMS holds. The OMS is
	// single-tenant per deployment (OMS_TENANT, Postgres RLS scoped to it), so an
	// order visible here belongs to exactly this tenant. It is stamped on the
	// reply as the deny-by-default authz-gate input a governed client checks the
	// caller against — the same contract query.v1 established for portfolios.
	ownerTenant string
}

// New returns a Server. An empty ownerTenant leaves owner_tenant blank on every
// reply, which a deny-by-default gate treats as a denial — an unconfigured
// tenant fails closed, never open.
func New(store Reader, ownerTenant string) *Server {
	return &Server{store: store, ownerTenant: ownerTenant}
}

// Register attaches the service to a gRPC server.
func (s *Server) Register(g *grpc.Server) { orderpb.RegisterOrderQueryServiceServer(g, s) }

// ListOrders returns one portfolio's orders, newest first.
func (s *Server) ListOrders(ctx context.Context, req *orderpb.ListOrdersRequest) (*orderpb.ListOrdersResponse, error) {
	// AN EMPTY PORTFOLIO IS REFUSED, NOT WIDENED. In the orders table an empty
	// portfolio_id is migration 0007's NOT-INDEXED marker, so treating this as
	// "all portfolios" would return exactly the orders whose portfolio nobody
	// can attribute — the least defensible possible answer to a question about
	// one portfolio.
	if req.GetPortfolioId() == "" {
		return nil, status.Error(codes.InvalidArgument, "portfolio_id is required")
	}

	orders, unindexed, err := s.store.ListByPortfolio(ctx, req.GetPortfolioId(), int(req.GetLimit()))
	if err != nil {
		return nil, mapError(err)
	}
	return &orderpb.ListOrdersResponse{
		Orders:      orders,
		OwnerTenant: s.ownerTenant,
		// Passed through rather than dropped: a caller that cannot see how much
		// history is unindexed will present this page as the whole of it.
		Unindexed: unindexed,
	}, nil
}

// mapError turns a store failure into a gRPC status.
//
// THE STORE'S MESSAGE IS NOT RELAYED. It carries connection strings, table names
// and driver detail — an internal description of the OMS's own plumbing, and a
// read surface is not the place to publish it. The code is enough for a caller
// to decide whether to retry.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The CALLER gave up, or its deadline passed. Reporting that as an
		// unavailable store would send an operator looking at Postgres.
		return status.Error(codes.Canceled, "the request was cancelled")
	}
	return status.Error(codes.Unavailable, "the order store could not be read")
}
