package grpcsrv_test

// THE PENDING-APPROVAL READ SURFACE (#410).
//
// A held order is in order_proposals and NOWHERE ELSE — not in the orders table,
// so ListOrders cannot see it and no blotter built on that route ever will.
// #410's acceptance is explicit that an unapproved act must be VISIBLY PENDING
// rather than silently dropped, so this route is the difference between a
// control and a drop. These tests hold it to that.

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
	omsorder "github.com/eighred/kanz/services/oms/internal/order"
)

var queueT0 = time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

// fakePending is the read half of the proposals store. It is a slice rather than
// a real store because what is under test is the ADAPTER — that it asks, filters
// and stamps — not the store, which has its own gated tests.
type fakePending struct {
	all []omsorder.OrderProposal
	err error
	// askedAt records the instant the server passed in, so a test can prove the
	// expiry bound is really handed to the store rather than ignored.
	askedAt time.Time
}

func (f *fakePending) Pending(_ context.Context, now time.Time) ([]omsorder.OrderProposal, error) {
	f.askedAt = now
	if f.err != nil {
		return nil, f.err
	}
	var out []omsorder.OrderProposal
	for _, p := range f.all {
		if p.Pending(now) {
			out = append(out, p)
		}
	}
	return out, nil
}

func held(orderID, portfolio, proposer string) omsorder.OrderProposal {
	return omsorder.OrderProposal{
		Proposal: dualcontrol.Proposal{
			ID: orderID, Subject: orderID, Act: dualcontrol.ActOrderSubmission,
			Proposer: proposer, Digest: "d-" + orderID,
			CreatedAt: queueT0, ExpiresAt: queueT0.Add(24 * time.Hour),
		},
		Command:     &orderpb.SubmitOrder{OrderId: orderID, PortfolioId: portfolio, InstrumentId: "AAPL"},
		PortfolioID: portfolio,
	}
}

func clock(at time.Time) func() time.Time { return func() time.Time { return at } }

// TestTheQueueShowsAHeldOrderWithEverythingAnApproverNeeds.
func TestTheQueueShowsAHeldOrderWithEverythingAnApproverNeeds(t *testing.T) {
	pending := &fakePending{all: []omsorder.OrderProposal{held("o1", "pf1", "user:alice@kanz")}}
	srv := grpcsrv.New(nil, pending, clock(queueT0.Add(time.Hour)), "acme")

	resp, err := srv.ListPendingApprovals(context.Background(), &orderpb.ListPendingApprovalsRequest{})
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(resp.GetPending()) != 1 {
		t.Fatalf("queue holds %d orders, want 1 — a held order that appears on no list is "+
			"indistinguishable from one that was dropped", len(resp.GetPending()))
	}
	got := resp.GetPending()[0]
	if got.GetOrderId() != "o1" || got.GetProposer() != "user:alice@kanz" || got.GetDigest() != "d-o1" {
		t.Errorf("the queue entry does not identify what is being approved or by whom: %v", got)
	}
	// THE COMMAND IS THE POINT. An approval bound to an id the approver cannot
	// inspect is a signature on something unread.
	if got.GetCommand().GetInstrumentId() != "AAPL" {
		t.Errorf("the queue entry does not carry the order an approver has to read: %v", got.GetCommand())
	}
	if !got.GetExpiresAt().AsTime().After(got.GetProposedAt().AsTime()) {
		t.Error("the queue does not say how long is left — an order about to be dropped looks " +
			"exactly like one proposed a minute ago")
	}
	if resp.GetOwnerTenant() != "acme" {
		t.Errorf("owner_tenant = %q — a deny-by-default gate reads an empty one as a denial",
			resp.GetOwnerTenant())
	}
}

// TestAnExpiredProposalIsNotOfferedAsWork. The server must hand the store the
// instant, or an approver is shown orders no approval could still apply.
func TestAnExpiredProposalIsNotOfferedAsWork(t *testing.T) {
	pending := &fakePending{all: []omsorder.OrderProposal{held("o1", "pf1", "user:alice@kanz")}}
	after := queueT0.Add(48 * time.Hour)
	srv := grpcsrv.New(nil, pending, clock(after), "acme")

	resp, err := srv.ListPendingApprovals(context.Background(), &orderpb.ListPendingApprovalsRequest{})
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(resp.GetPending()) != 0 {
		t.Fatalf("an EXPIRED proposal is still offered as work (%d entries) — approving it would "+
			"fail, and until somebody tried, the queue would read as a backlog somebody could clear",
			len(resp.GetPending()))
	}
	if !pending.askedAt.Equal(after) {
		t.Errorf("the store was asked at %v, not the server's clock %v — the expiry bound is being "+
			"decided somewhere other than where the test can see it", pending.askedAt, after)
	}
}

// TestAnEmptyPortfolioIsTheWHOLEQueue — the deliberate asymmetry with
// ListOrders. A pending list readable only one portfolio at a time is a queue
// nobody watches.
func TestAnEmptyPortfolioIsTheWHOLEQueue(t *testing.T) {
	pending := &fakePending{all: []omsorder.OrderProposal{
		held("o1", "pf1", "user:alice@kanz"),
		held("o2", "pf2", "user:alice@kanz"),
	}}
	srv := grpcsrv.New(nil, pending, clock(queueT0.Add(time.Hour)), "acme")

	resp, err := srv.ListPendingApprovals(context.Background(), &orderpb.ListPendingApprovalsRequest{})
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(resp.GetPending()) != 2 {
		t.Fatalf("the whole-queue read returned %d of 2 — an approver asking 'what is waiting on "+
			"me' has to know every portfolio id first", len(resp.GetPending()))
	}

	one, err := srv.ListPendingApprovals(context.Background(),
		&orderpb.ListPendingApprovalsRequest{PortfolioId: "pf2"})
	if err != nil {
		t.Fatalf("ListPendingApprovals(pf2): %v", err)
	}
	if len(one.GetPending()) != 1 || one.GetPending()[0].GetOrderId() != "o2" {
		t.Fatalf("narrowing to one portfolio returned %v", one.GetPending())
	}
}

// TestAServerBuiltWithNoQueueREFUSESRatherThanAnsweringNothing. An empty page
// would tell an approver there is nothing waiting on them while orders sat held
// and expiring: "nothing configured" and "checked, and fine" looking the same,
// on the one route whose absence turns this control into a drop.
func TestAServerBuiltWithNoQueueREFUSESRatherThanAnsweringNothing(t *testing.T) {
	srv := grpcsrv.New(nil, nil, nil, "acme")

	_, err := srv.ListPendingApprovals(context.Background(), &orderpb.ListPendingApprovalsRequest{})
	if err == nil {
		t.Fatal("a server with no pending queue answered 'nothing is waiting' — an approver would " +
			"read an empty queue while held orders expired unfilled")
	}
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented so a caller can tell 'this build cannot answer' "+
			"from 'there is nothing'", status.Code(err))
	}
}

// TestAStoreFailureIsNotAnEmptyQueue. Same rule one layer down: a read that
// could not be answered must not look like an answer of zero.
func TestAStoreFailureIsNotAnEmptyQueue(t *testing.T) {
	pending := &fakePending{err: context.DeadlineExceeded}
	srv := grpcsrv.New(nil, pending, clock(queueT0), "acme")

	if _, err := srv.ListPendingApprovals(context.Background(), &orderpb.ListPendingApprovalsRequest{}); err == nil {
		t.Fatal("a store failure was reported as an empty pending queue")
	}
}
