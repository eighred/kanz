package grpcsrv_test

// THE REFUSAL ON THE QUEUE (#558).
//
// An approver whose signature was refused used to be told nothing: the gateway
// answered 202 at publish, the OMS logged a WARN and acked, and the only signal
// was the order still sitting on this queue — identical to an order nobody had
// touched. These hold the route to saying which.

import (
	"context"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
	omsorder "github.com/eighred/kanz/services/oms/internal/order"
)

// refused annotates a held order the way handleApprove's refusal branch does.
func refused(p omsorder.OrderProposal, reason, by string, at time.Time) omsorder.OrderProposal {
	p.RefusalReason = reason
	p.RefusedBy = by
	p.RefusedAt = at
	return p
}

// TestAnUnrefusedEntryStillCarriesItsState is the #563 rule, and it is the half
// that is easy to leave out.
//
// A state field written only onto the interesting rows makes its absence
// ambiguous: a client cannot tell "nobody has been refused" from "this server
// predates the field", and those want opposite renderings. Emitting it on every
// entry is what makes the value readable at all.
func TestAnUnrefusedEntryStillCarriesItsState(t *testing.T) {
	pending := &fakePending{all: []omsorder.OrderProposal{held("o1", "pf1", "user:alice@kanz")}}
	srv := grpcsrv.New(nil, pending, clock(queueT0.Add(time.Hour)), "acme")

	resp, err := srv.ListPendingApprovals(context.Background(), &orderpb.ListPendingApprovalsRequest{})
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	got := resp.GetPending()[0]
	if got.GetState() != dualcontrol.StatePending {
		t.Errorf("state = %q, want %q — an entry with no state leaves a client unable to tell "+
			"an untouched order from one this server is too old to describe", got.GetState(),
			dualcontrol.StatePending)
	}
	if got.GetLastRefusalReason() != "" || got.GetLastRefusedBy() != "" || got.GetLastRefusedAt() != nil {
		t.Errorf("an order nobody attempted carries a refusal: reason=%q by=%q at=%v",
			got.GetLastRefusalReason(), got.GetLastRefusedBy(), got.GetLastRefusedAt())
	}
}

// TestARefusedEntryIsDistinguishableFromOneAwaitingASignature is the whole of
// #558 at the read surface: two orders, both awaiting a signature, and the queue
// says which one turned somebody away and why.
func TestARefusedEntryIsDistinguishableFromOneAwaitingASignature(t *testing.T) {
	at := queueT0.Add(30 * time.Minute)
	pending := &fakePending{all: []omsorder.OrderProposal{
		held("o-untouched", "pf1", "user:alice@kanz"),
		refused(held("o-refused", "pf1", "user:alice@kanz"),
			omsorder.RefusalSelfApproval, "user:alice@kanz", at),
	}}
	srv := grpcsrv.New(nil, pending, clock(queueT0.Add(time.Hour)), "acme")

	resp, err := srv.ListPendingApprovals(context.Background(), &orderpb.ListPendingApprovalsRequest{})
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	byID := map[string]*orderpb.PendingApproval{}
	for _, p := range resp.GetPending() {
		byID[p.GetOrderId()] = p
	}
	if len(byID) != 2 {
		t.Fatalf("queue holds %d entries, want 2 — a refused proposal must STAY on the queue: "+
			"it is still awaiting a signature somebody else may legitimately give", len(byID))
	}

	if s := byID["o-untouched"].GetState(); s != dualcontrol.StatePending {
		t.Errorf("the untouched order's state = %q, want %q", s, dualcontrol.StatePending)
	}
	r := byID["o-refused"]
	if r.GetState() != dualcontrol.StateRefused {
		t.Fatalf("the refused order's state = %q, want %q — it renders identically to an order "+
			"nobody has touched, which is exactly the silence #558 was filed for",
			r.GetState(), dualcontrol.StateRefused)
	}
	if r.GetLastRefusalReason() != omsorder.RefusalSelfApproval {
		t.Errorf("last_refusal_reason = %q, want %q — the approver is told THAT they were "+
			"refused and not why, so they cannot tell 'find another approver' from 're-propose "+
			"the order'", r.GetLastRefusalReason(), omsorder.RefusalSelfApproval)
	}
	if r.GetLastRefusedBy() != "user:alice@kanz" {
		t.Errorf("last_refused_by = %q — an approver cannot tell their own refused signature "+
			"from somebody else's", r.GetLastRefusedBy())
	}
	if r.GetLastRefusedAt().AsTime() != at {
		t.Errorf("last_refused_at = %v, want %v", r.GetLastRefusedAt().AsTime(), at)
	}
	// AND IT IS STILL WORK. A refused entry that dropped its expiry or its
	// command would be an annotation replacing the thing being annotated: the
	// approver could see the refusal and no longer see what they are being asked
	// to sign.
	if r.GetCommand().GetOrderId() != "o-refused" || r.GetExpiresAt() == nil {
		t.Error("the refused entry lost the order it is asking for a signature on")
	}
}
