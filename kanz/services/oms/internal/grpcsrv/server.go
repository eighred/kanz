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
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/services/oms/internal/order"
)

// Reader is the half of order.Store this surface needs. Narrow on purpose: a
// read server holding the full Store could Create or Save, and the compiler
// should be what stops that rather than a review.
type Reader interface {
	ListByPortfolio(ctx context.Context, portfolioID string, limit int) ([]*orderpb.OrderState, int64, error)
}

// PendingReader is the read half of order.ProposalStore (#410) — and it is
// SEPARATE FROM Reader for the reason Reader's own doc gives, one level down.
// order.ProposalStore carries Claim, which RECORDS A SECOND SIGNATURE; a read
// surface holding it could approve an order, which is exactly the second door
// into the capital path this package refuses. Structural typing narrows it here,
// so the compiler is what stops it.
type PendingReader interface {
	Pending(ctx context.Context, now time.Time) ([]order.OrderProposal, error)
}

// Server implements order.v1.OrderQueryService over a Reader.
type Server struct {
	orderpb.UnimplementedOrderQueryServiceServer
	store Reader
	// pending is the queue of orders held for a second signature (#410).
	pending PendingReader
	// now bounds which proposals are still approvable. Injected rather than
	// read from the clock inside, so a test can prove an EXPIRED proposal is
	// dropped from the queue — the case that decides whether an approver is
	// shown work they can still do.
	now func() time.Time
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
//
// pending IS A REQUIRED ARGUMENT RATHER THAN AN OPTION, and that is the point.
// A held order appears nowhere else — not in the book, not on any blotter built
// on ListOrders — so a composition root that could build this surface WITHOUT
// the pending queue would produce an OMS that holds orders nobody can see or
// approve. That is the silent drop #410 exists to end, delivered by a forgotten
// constructor argument. nil is still constructible in Go, which is why
// ListPendingApprovals refuses it loudly rather than returning an empty page.
func New(store Reader, pending PendingReader, now func() time.Time, ownerTenant string) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{store: store, pending: pending, now: now, ownerTenant: ownerTenant}
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

// ListPendingApprovals returns the orders held for a second signature, oldest
// first (#410).
//
// AN EMPTY portfolio_id IS THE WHOLE QUEUE, unlike ListOrders — see the request
// message's own doc for why the asymmetry is deliberate rather than an
// inconsistency.
func (s *Server) ListPendingApprovals(ctx context.Context, req *orderpb.ListPendingApprovalsRequest) (*orderpb.ListPendingApprovalsResponse, error) {
	if s.pending == nil {
		// REFUSED, NOT EMPTY. An empty page here would tell an approver there is
		// nothing waiting on them while orders sat held and expiring — "nothing
		// configured" and "checked, and fine" looking the same, on the one route
		// whose absence turns this control into a drop.
		return nil, status.Error(codes.Unimplemented,
			"this OMS was built with no pending-approval queue, so it cannot say what is awaiting a "+
				"second signature — it must not answer 'nothing'")
	}
	held, err := s.pending.Pending(ctx, s.now())
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*orderpb.PendingApproval, 0, len(held))
	for _, p := range held {
		if req.GetPortfolioId() != "" && p.PortfolioID != req.GetPortfolioId() {
			continue
		}
		out = append(out, &orderpb.PendingApproval{
			OrderId:    p.ID,
			Command:    p.Command,
			Proposer:   p.Proposer,
			Act:        string(p.Act),
			Digest:     p.Digest,
			ProposedAt: timestamppb.New(p.CreatedAt),
			ExpiresAt:  timestamppb.New(p.ExpiresAt),
		})
		if limit := int(req.GetLimit()); limit > 0 && len(out) == limit {
			break
		}
	}
	return &orderpb.ListPendingApprovalsResponse{Pending: out, OwnerTenant: s.ownerTenant}, nil
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
