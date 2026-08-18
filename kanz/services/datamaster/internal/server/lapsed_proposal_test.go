package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A PROPOSAL NOBODY SIGNED MUST BE VISIBLE AS LAPSED, NOT ABSENT (#563).
//
// #547 gave held ORDERS this property with a terminal FACT. That answer does not
// transfer here and the reason is worth stating where the test lives: it worked
// because ORDER_REJECTED already had a reader — the trader watching for any
// rejection. This service publishes one subject, data.exception.overridden, and
// nothing consumes it but the audit projector, so an expiry FACT would have been
// a new subject and grant with nobody on the other end. The surface the proposer
// already uses is the reader that exists.
//
// What was wrong: an override proposal reaching expires_at simply stopped
// appearing, and one absence had to serve for "never proposed", "somebody is
// still considering it" and "it died unsigned".

type listedProposal struct {
	ProposalID string `json:"proposal_id"`
	State      string `json:"state"`
	Proposer   string `json:"proposer"`
}

func (a armed) list(t *testing.T, subject string) []listedProposal {
	t.Helper()
	rec := a.do(t, http.MethodGet, "/v1/exceptions/pending-overrides", subject, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out []listedProposal
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return out
}

func (a armed) find(t *testing.T, subject, id string) (listedProposal, bool) {
	t.Helper()
	for _, p := range a.list(t, subject) {
		if p.ProposalID == id {
			return p, true
		}
	}
	return listedProposal{}, false
}

func TestAProposalNobodySignedIsListedAsLapsed(t *testing.T) {
	a := newDualControlServer(t, true)
	ex := openExceptionID(t, a.s, a.exceptions)
	id := a.propose(t, ex, "alice@kanz", "130")

	// WHILE IT IS STILL ACTIONABLE.
	p, ok := a.find(t, "bob@kanz", id)
	if !ok {
		t.Fatal("a proposal awaiting a signature is not on the queue an approver reads")
	}
	if p.State != "pending" {
		t.Fatalf("state = %q, want pending — a client that reads state must be told this one is work", p.State)
	}

	// AND AFTER IT DIES.
	*a.clock = a.clock.Add(48 * time.Hour)

	p, ok = a.find(t, "alice@kanz", id)
	if !ok {
		t.Fatal("a lapsed proposal vanished from the surface it was proposed on.\n\n" +
			"That is the defect: the proposer cannot tell 'I never proposed it' from 'somebody is " +
			"still considering it' from 'it died unsigned', and nothing anywhere recorded that a " +
			"resolution had been proposed and had lapsed")
	}
	if p.State != "lapsed" {
		t.Fatalf("state = %q, want lapsed — an expired proposal listed as pending is worse than "+
			"absent: it sends an approver to sign something that can no longer be signed", p.State)
	}
	if p.Proposer != "alice@kanz" {
		t.Errorf("proposer = %q — a lapsed entry that does not say WHO proposed it cannot tell them "+
			"it was theirs", p.Proposer)
	}
}

// STATE IS ON EVERY ENTRY, NOT ONLY THE NEW ONES. A client that ignores unknown
// keys — which is every client written before this — would read a lapsed
// proposal as work waiting for it if pending entries carried no state to
// distinguish them.
func TestEveryListedProposalCarriesItsState(t *testing.T) {
	a := newDualControlServer(t, true)
	ex := openExceptionID(t, a.s, a.exceptions)
	dead := a.propose(t, ex, "alice@kanz", "130")
	*a.clock = a.clock.Add(48 * time.Hour)
	fresh := a.propose(t, ex, "carol@kanz", "131")

	states := map[string]string{}
	for _, p := range a.list(t, "bob@kanz") {
		if p.State == "" {
			t.Fatalf("proposal %s has no state field", p.ProposalID)
		}
		states[p.ProposalID] = p.State
	}
	if states[dead] != "lapsed" || states[fresh] != "pending" {
		t.Fatalf("states = %v, want %s lapsed and %s pending — one list must separate what can still "+
			"be signed from what cannot", states, dead, fresh)
	}
}

// A LAPSED PROPOSAL IS STILL UNSIGNABLE. Listing it must not make it approvable
// again: that would turn a visibility fix into a way to sign a stale view of the
// book, which is exactly what the TTL exists to prevent.
func TestALapsedProposalCannotBeApproved(t *testing.T) {
	a := newDualControlServer(t, true)
	ex := openExceptionID(t, a.s, a.exceptions)
	id := a.propose(t, ex, "alice@kanz", "130")
	*a.clock = a.clock.Add(48 * time.Hour)

	if _, ok := a.find(t, "bob@kanz", id); !ok {
		t.Fatal("precondition: the lapsed proposal should be listed")
	}
	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+ex+"/override/approve", "user:bob@kanz",
		`{"proposal_id":"`+id+`","reason":"vendor confirmed after corp action","chosen_price":"130"}`)
	if rec.Code == http.StatusAccepted {
		t.Fatal("a lapsed proposal was approved — listing it made it signable, and the TTL now means " +
			"nothing")
	}
}
