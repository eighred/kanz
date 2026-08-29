package server

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/services/datamaster/internal/pricing"
	"github.com/eighred/kanz/services/datamaster/internal/projector"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

// MAKER-CHECKER ON THE PRICING OVERRIDE (#410).
//
// #410's "verified when" has four clauses, and each has a test below:
//
//	a compliance override requires approval by a DIFFERENT authenticated subject
//	  before taking effect                     -> TestArmed_AnOverrideDoesNotTakeEffectUntilApproved
//	                                              TestArmed_ADifferentSubjectApprovesAndItApplies
//	the approval records BOTH identities       -> TestArmed_TheTrailCarriesBothIdentities
//	a self-approval is refused, by a test,
//	  "because that is exactly the clause that
//	   rots into a comment"                    -> TestArmed_SelfApprovalIsRefused
//	an unapproved action is visibly PENDING
//	  rather than silently dropped             -> TestArmed_AnUnapprovedOverrideIsVisiblyPending

type armed struct {
	s          *Server
	exceptions store.ExceptionStore
	proposals  *store.MemoryProposals
	reg        *prometheus.Registry
	clock      *time.Time
}

func newDualControlServer(t *testing.T, require bool) armed {
	t.Helper()
	return newDualControlServerWith(t, require, nil)
}

// newDualControlServerWith builds the armed server, optionally wrapping the
// proposal store THE QUEUE CLAIMS AGAINST.
//
// The wrapper hook exists because the claim moved (#807): an approval no longer
// claims and then applies, it hands the claim to the override, so a test that
// wants to lose the claim has to lose it where the override takes it. Swapping
// s.proposals alone now only affects the read and the rejection path.
func newDualControlServerWith(t *testing.T, require bool, wrapQueueProposals func(store.ProposalStore) store.ProposalStore) armed {
	t.Helper()
	feeds := testFeeds()
	golden := store.NewMemoryGoldenStore()
	// THE QUEUE HOLDS THE SAME PROPOSAL STORE THE SERVER APPROVES AGAINST (#807).
	// An approved override consumes its proposal in the same call that applies
	// it, so a queue wired to a different instance would apply the override and
	// leave the proposal approvable — the same signature spendable twice.
	proposals := store.NewMemoryProposals()
	var queueProposals store.ProposalStore = proposals
	if wrapQueueProposals != nil {
		queueProposals = wrapQueueProposals(proposals)
	}
	exceptions := store.NewQueueStore(pricing.NewQueue(), queueProposals)
	proj := projector.New(feeds, golden, exceptions, nil, projector.WithClock(func() time.Time { return now }))
	if err := proj.Refresh(context.Background()); err != nil {
		t.Fatalf("projector refresh: %v", err)
	}
	r := &Readiness{}
	r.Set(true)
	clock := now
	reg := prometheus.NewRegistry()
	s := New(r, nil, testTenant, golden, exceptions, feeds,
		WithClock(func() time.Time { return clock }),
		WithDualControl(DualControl{Proposals: proposals, Require: require, Registerer: reg}))
	return armed{s: s, exceptions: exceptions, proposals: proposals, reg: reg, clock: &clock}
}

func (a armed) do(t *testing.T, method, path, subject, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Kanz-Principal-Tenant", testTenant)
	if subject != "" {
		req.Header.Set("X-Kanz-Principal-Subject", subject)
	}
	rec := httptest.NewRecorder()
	a.s.ServeHTTP(rec, req)
	return rec
}

// propose runs the override and returns the pending proposal id.
func (a armed) propose(t *testing.T, exceptionID, proposer, price string) string {
	t.Helper()
	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+exceptionID+"/override", proposer,
		`{"reason":"vendor confirmed after corp action","chosen_price":"`+price+`"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("propose: want 202 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Status     string `json:"status"`
		ProposalID string `json:"proposal_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode propose response: %v", err)
	}
	if out.Status != "PENDING_APPROVAL" || out.ProposalID == "" {
		t.Fatalf("propose returned %+v, want a PENDING_APPROVAL with a proposal id", out)
	}
	return out.ProposalID
}

func (a armed) exception(t *testing.T, id string) pricing.Exception {
	t.Helper()
	ex, ok, err := a.exceptions.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("read exception %s: ok=%v err=%v", id, ok, err)
	}
	return ex
}

// CLAUSE 1a: ARMED, AN OVERRIDE DOES NOT TAKE EFFECT.
//
// The 202 is load-bearing. A 200 would tell every existing client the override
// had been applied, which is the silent drop this control exists to prevent —
// and it is the failure an operator would discover at the next valuation rather
// than at the moment they acted.
func TestArmed_AnOverrideDoesNotTakeEffectUntilApproved(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)

	a.propose(t, id, "alice@kanz", "130")

	ex := a.exception(t, id)
	if len(ex.Overrides) != 0 {
		t.Errorf("the override was APPLIED on one signature: %+v", ex.Overrides)
	}
	if ex.Status == pricing.StatusOverridden {
		t.Errorf("exception status = %s after a proposal nobody approved, want it still open", ex.Status)
	}
}

// CLAUSE 1b: A DIFFERENT AUTHENTICATED SUBJECT APPROVES, AND IT APPLIES.
//
// Without this the control is an outage: refusing everything also satisfies
// "requires a second person".
func TestArmed_ADifferentSubjectApprovesAndItApplies(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve by a different person: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}

	ex := a.exception(t, id)
	if len(ex.Overrides) != 1 {
		t.Fatalf("want exactly 1 override after approval, got %d", len(ex.Overrides))
	}
	if ex.Status != pricing.StatusOverridden {
		t.Errorf("status = %s after an approved override, want OVERRIDDEN", ex.Status)
	}
}

// CLAUSE 2: THE TRAIL CARRIES BOTH IDENTITIES.
//
// An approval recorded under one name is indistinguishable from the
// single-signature world it replaced. The auditor's question is "who proposed
// this and who approved it", and the answer has to be IN the record.
func TestArmed_TheTrailCarriesBothIdentities(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	if rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d (%s)", rec.Code, rec.Body.String())
	}

	o := a.exception(t, id).Overrides[0]
	if o.Actor != "alice@kanz" {
		t.Errorf("actor = %q, want the PROPOSER alice@kanz", o.Actor)
	}
	if o.Approver != "bob@kanz" {
		t.Errorf("approver = %q, want bob@kanz — an override that does not name its second signature "+
			"reads exactly like one that never had it", o.Approver)
	}
	if !o.DualSigned() {
		t.Error("DualSigned() is false on a two-person override")
	}
	// AND THE RENDERED RECORD SAYS SO. The auditor reads JSON, not Go.
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"approver":"bob@kanz"`, `"dual_signed":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("rendered override is missing %s: %s", want, raw)
		}
	}
}

// CLAUSE 3: A SELF-APPROVAL IS REFUSED.
//
// #410: "asserted by a test, because 'the approver must not be the proposer' is
// exactly the clause that rots into a comment."
func TestArmed_SelfApprovalIsRefused(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "alice@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-approval: want 403 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(a.exception(t, id).Overrides) != 0 {
		t.Fatal("the override APPLIED on a self-approval — one person held both signatures")
	}
	// AND THE PROPOSAL SURVIVES. Consuming it here would let one person destroy a
	// pending decision by attempting to approve their own, turning a refused
	// attack into a denial of service on the colleague who could have approved.
	pending, err := a.proposals.Pending(context.Background(), *a.clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("after a refused self-approval there are %d pending proposals, want 1 — the refusal "+
			"consumed a decision somebody else could still have approved", len(pending))
	}
	// The counter an auditor asks for.
	if got := testutil.ToFloat64(a.s.overrideMetrics.proposals.WithLabelValues("refused_self_approval")); got != 1 {
		t.Errorf("refused_self_approval = %v, want 1 — the control fired but nothing counted it", got)
	}
}

// AND THE SAME NAME SPELLED DIFFERENTLY IS STILL THE SAME PERSON.
//
// This is the version of the attack that survives a naive implementation: the
// audit trail would show "Alice@kanz" approving "alice@kanz" and read as
// four-eyes to anyone scanning it.
func TestArmed_SelfApprovalByRespellingIsRefused(t *testing.T) {
	for _, approver := range []string{"Alice@kanz", "ALICE@KANZ", " alice@kanz "} {
		a := newDualControlServer(t, true)
		id := openExceptionID(t, a.s, a.exceptions)
		proposalID := a.propose(t, id, "alice@kanz", "130")

		rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", approver,
			`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%q approving alice@kanz: want 403 got %d (%s)", approver, rec.Code, rec.Body.String())
		}
		if n := len(a.exception(t, id).Overrides); n != 0 {
			t.Errorf("%q: the override applied — one person holds both signatures and the trail "+
				"shows two actors", approver)
		}
	}
}

// CLAUSE 4: AN UNAPPROVED OVERRIDE IS VISIBLY PENDING.
//
// #410: "an order that neither executes nor reports why is the failure mode this
// platform refuses everywhere else." A proposer who closed their browser has no
// other way to find what they left waiting.
func TestArmed_AnUnapprovedOverrideIsVisiblyPending(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	rec := a.do(t, http.MethodGet, "/v1/exceptions/pending-overrides", "carol@kanz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pending list: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var pending []struct {
		ProposalID  string `json:"proposal_id"`
		ExceptionID string `json:"exception_id"`
		Proposer    string `json:"proposer"`
		ChosenPrice string `json:"chosen_price"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d entries, want 1 — an override nobody approved is invisible", len(pending))
	}
	got := pending[0]
	if got.ProposalID != proposalID || got.ExceptionID != id || got.Proposer != "alice@kanz" {
		t.Errorf("pending entry = %+v, want the proposal just made", got)
	}
	// The price is there so an approver can see WHAT they are signing for without
	// a second round trip — an approver who cannot see the value is a rubber stamp.
	if got.ChosenPrice != "130" {
		t.Errorf("chosen_price = %q, want 130", got.ChosenPrice)
	}

	// AND IT LEAVES THE LIST ONCE DECIDED.
	if rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d", rec.Code)
	}
	rec = a.do(t, http.MethodGet, "/v1/exceptions/pending-overrides", "carol@kanz", "")
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("pending after approval = %s, want [] — a decided proposal still listed reads as work "+
			"outstanding", body)
	}
}

// AN EXPIRED PROPOSAL IS NOT APPROVABLE.
//
// Without a deadline a signature collected today can be applied against next
// quarter's book.
func TestArmed_AnExpiredProposalCannotBeApproved(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	*a.clock = now.Add(25 * time.Hour) // past DefaultTTL

	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approving an expired proposal: want 409 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(a.exception(t, id).Overrides) != 0 {
		t.Error("an expired proposal applied")
	}
	// AND IT IS NO LONGER ADVERTISED AS PENDING — which is now a statement about
	// its STATE rather than its absence (#563).
	//
	// This assertion used to require the list to be exactly `[]`. That was the
	// behaviour #563 identified as the defect: an expired proposal vanished, and
	// the proposer could not tell "it lapsed" from "I never proposed it". The
	// property this test exists to protect is unchanged and is now checked
	// directly — an expired proposal must never be offered as work — rather than
	// through an emptiness that also hid the record.
	p, ok := a.find(t, "carol@kanz", proposalID)
	if !ok {
		t.Fatal("the expired proposal vanished from the queue entirely — a proposer has no way to " +
			"learn their override lapsed unsigned (#563)")
	}
	if p.State != "lapsed" {
		t.Errorf("expired proposal listed with state %q, want lapsed — anything else sends an "+
			"approver to sign a proposal the line above just refused", p.State)
	}
}

// THE APPROVAL COVERS THE VALUE.
//
// Simulates the payload being altered after the signature was given — the
// proposal is rewritten in the store with a different price while keeping the
// digest that was signed. The approval must not carry over.
func TestArmed_APayloadAlteredAfterProposalIsRefused(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	prop, ok, err := a.proposals.Get(context.Background(), proposalID)
	if err != nil || !ok {
		t.Fatalf("get proposal: ok=%v err=%v", ok, err)
	}
	prop.ChosenPrice = prop.ChosenPrice.SetInt64(9999) // the digest still says 130

	// THE SWAP TAKES TWO STEPS NOW, and the reason is the point of #562. Put used
	// to overwrite an id already held, so this test rewrote the row in place; the
	// shared store refuses a duplicate in BOTH backends, exactly as Postgres's
	// primary key always did. Editing the value the store handed back does not
	// work either — reads are deep copies. So the tamper has to remove the row and
	// hold a new one, which is a fair simulation of the threat and no longer
	// depends on a store behaviour production never had.
	ctx := context.Background()
	if claimed, err := a.proposals.Claim(ctx, proposalID); err != nil || !claimed {
		t.Fatalf("clearing the proposal for the tamper: claimed=%v err=%v", claimed, err)
	}
	if err := a.proposals.Put(ctx, prop); err != nil {
		t.Fatal(err)
	}

	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approving an altered payload: want 409 got %d (%s)", rec.Code, rec.Body.String())
	}
	if n := len(a.exception(t, id).Overrides); n != 0 {
		t.Error("a price nobody approved was written to the audit trail")
	}
}

// ONE DECISION, ONE RECORD.
//
// Two approvers acting on the same pending override both pass every check —
// different people, matching digest, unexpired. Only one may apply.
func TestArmed_AProposalCanOnlyBeAppliedOnce(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	first := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
	second := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "carol@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)

	if first.Code != http.StatusOK {
		t.Fatalf("first approval: want 200 got %d", first.Code)
	}
	// 404, NOT 409, for a SEQUENTIAL second attempt: the proposal is gone, and
	// "already decided", "never existed" and "belongs to another tenant" are
	// deliberately indistinguishable on this surface. 409 is the genuine-race
	// answer — see TestArmed_ALostClaimRefusesRatherThanApplyingTwice.
	if second.Code != http.StatusNotFound {
		t.Errorf("second approval of the same proposal: want 404 got %d (%s)", second.Code, second.Body.String())
	}
	// THE ASSERTION THAT MATTERS whichever status it is.
	if n := len(a.exception(t, id).Overrides); n != 1 {
		t.Errorf("%d override records for ONE decision — the trail double-counts an approval", n)
	}
}

// losesTheClaim is a proposal store that reads back a valid pending proposal and
// then loses the claim — the state two concurrent approvers leave each other in.
type losesTheClaim struct{ store.ProposalStore }

func (l losesTheClaim) Claim(context.Context, string) (bool, error) { return false, nil }

// A LOST CLAIM REFUSES; IT DOES NOT APPLY.
//
// Two approvers acting at the same moment both pass every check — different
// people, matching digest, neither expired. Only the one that claims the
// proposal may apply it. Without the claim being the serialisation point, both
// would write, appending two override records for one decision.
//
// The sequential test above cannot reach this: it needs the read to succeed and
// the claim to fail, which is precisely the window a real race opens.
//
// SINCE #807 THE CLAIM IS INSIDE THE OVERRIDE, so losing it must also leave the
// override unwritten — the two are one act. That is what the zero-overrides
// assertion below now measures.
func TestArmed_ALostClaimRefusesRatherThanApplyingTwice(t *testing.T) {
	// The claim that decides is the one INSIDE the override (#807), so the store
	// that loses it is the one the queue applies against — not s.proposals, which
	// now only answers the read and the rejection path.
	a := newDualControlServerWith(t, true, func(p store.ProposalStore) store.ProposalStore {
		return losesTheClaim{p}
	})
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("an approver that lost the claim: want 409 got %d (%s)", rec.Code, rec.Body.String())
	}
	if n := len(a.exception(t, id).Overrides); n != 0 {
		t.Errorf("%d override records written by an approver that did not win the claim — two "+
			"concurrent approvals would both apply", n)
	}
	if got := testutil.ToFloat64(a.s.overrideMetrics.proposals.WithLabelValues("refused_race")); got != 1 {
		t.Errorf("refused_race = %v, want 1", got)
	}
}

// refusesTheOverride is an exception store whose Override applies nothing and
// says so — a store failure anywhere inside the override's transaction.
type refusesTheOverride struct{ store.ExceptionStore }

func (refusesTheOverride) Override(context.Context, string, pricing.Override, store.Claim) error {
	return errors.New("the override FACT could not be built")
}

// A FAILED OVERRIDE LEAVES THE APPROVAL STILL GIVABLE (#807).
//
// The handler used to claim the proposal and THEN apply the override, so any
// failure after the claim — a store error, or the process simply dying — spent
// the second signature and applied nothing. The approver was told to re-propose,
// because there was nothing left to approve.
//
// The claim now travels with the override, so a refused override consumes
// nothing: the proposal is still on the pending queue and the same approver can
// sign again once the cause is fixed. That re-approval is what this asserts,
// because "the proposal is still in the store" is a weaker claim than "the
// approval still works".
func TestArmed_AFailedOverrideLeavesTheApprovalGivable(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	working := a.s.exceptions
	a.s.exceptions = refusesTheOverride{working}
	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an override the store refused: want 400 got %d (%s)", rec.Code, rec.Body.String())
	}
	a.s.exceptions = working

	// STILL PENDING. Not "still in the store" — on the queue the proposer and the
	// approver actually read.
	list := a.do(t, http.MethodGet, "/v1/exceptions/pending-overrides", "carol@kanz", "")
	var pending []struct {
		ProposalID string `json:"proposal_id"`
		State      string `json:"state"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &pending); err != nil {
		t.Fatalf("decode pending: %v (%s)", err, list.Body.String())
	}
	if len(pending) != 1 || pending[0].ProposalID != proposalID {
		t.Fatalf("pending after a failed override = %+v, want the proposal that was never applied — "+
			"the second signature was spent on an override that did not happen (#807)", pending)
	}

	// AND THE APPROVAL STILL WORKS.
	rec = a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-approving after a failed override: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	ex := a.exception(t, id)
	if ex.Status != pricing.StatusOverridden || len(ex.Overrides) != 1 {
		t.Errorf("after the retry the exception is %s with %d override(s), want OVERRIDDEN with 1",
			ex.Status, len(ex.Overrides))
	}
}

// A REJECTION REMOVES IT AND APPLIES NOTHING.
func TestArmed_RejectionAppliesNothing(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"reject"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(a.exception(t, id).Overrides) != 0 {
		t.Error("a rejected override applied")
	}
	if body := strings.TrimSpace(a.do(t, http.MethodGet, "/v1/exceptions/pending-overrides", "carol@kanz", "").Body.String()); body != "[]" {
		t.Errorf("a rejected proposal is still pending: %s", body)
	}
}

// AN UNAUTHENTICATED APPROVER IS REFUSED. Dual control over an anonymous second
// signature is the #444 defect wearing the control's name.
func TestArmed_ApprovalRequiresAnAuthenticatedApprover(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("approval with no principal: want 401 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(a.exception(t, id).Overrides) != 0 {
		t.Error("an anonymous approval applied")
	}
}

// AN EMPTY DECISION IS REFUSED RATHER THAN ASSUMED. Both defaults are wrong:
// approve applies something nobody consented to, reject discards a decision.
func TestArmed_TheDecisionMustBeExplicit(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	for _, body := range []string{
		`{"proposal_id":"` + proposalID + `"}`,
		`{"proposal_id":"` + proposalID + `","decision":""}`,
		`{"proposal_id":"` + proposalID + `","decision":"APPROVE"}`,
		`{"proposal_id":"` + proposalID + `","decision":"yes"}`,
	} {
		rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("decision %s: want 400 got %d", body, rec.Code)
		}
	}
	if len(a.exception(t, id).Overrides) != 0 {
		t.Error("an ambiguous decision applied the override")
	}
}

// A PROPOSAL BELONGING TO ANOTHER EXCEPTION IS NOT FOUND HERE.
//
// Without this the exception id in the path is decorative and an approver could
// be shown one exception while signing for another.
func TestArmed_AProposalCannotBeApprovedThroughAnotherExceptionsPath(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)
	proposalID := a.propose(t, id, "alice@kanz", "130")

	rec := a.do(t, http.MethodPost, "/v1/exceptions/SOME-OTHER-EXCEPTION/override/approve", "bob@kanz",
		`{"proposal_id":"`+proposalID+`","decision":"approve"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("approving through the wrong exception path: want 404 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(a.exception(t, id).Overrides) != 0 {
		t.Error("the override applied through another exception's path")
	}
}

// ===== UNARMED: the posture this ships in =====

// UNARMED, THE OVERRIDE STILL APPLIES — and is recorded as single-signed.
//
// This is the deliberate default. Arming on a deploy would turn every existing
// caller's 200 into a 202 that completes only when a second person acts: a
// valuation outage delivered by a security improvement. What must NOT happen is
// that the resulting record be indistinguishable from a dual-signed one.
func TestUnarmed_AppliesImmediatelyAndRecordsThatOnePersonSignedIt(t *testing.T) {
	a := newDualControlServer(t, false)
	id := openExceptionID(t, a.s, a.exceptions)

	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override", "alice@kanz",
		`{"reason":"vendor confirmed","chosen_price":"130"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unarmed override: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}

	o := a.exception(t, id).Overrides[0]
	if o.Actor != "alice@kanz" {
		t.Errorf("actor = %q", o.Actor)
	}
	if o.Approver != "" || o.DualSigned() {
		t.Errorf("approver = %q / DualSigned = %v on a one-person override — a single signature must "+
			"not read as two", o.Approver, o.DualSigned())
	}
	if got := testutil.ToFloat64(a.s.overrideMetrics.signatures.WithLabelValues("single_signed")); got != 1 {
		t.Errorf("single_signed = %v, want 1 — the #410 gap is not being counted", got)
	}
	if got := testutil.ToFloat64(a.s.overrideMetrics.signatures.WithLabelValues("dual_signed")); got != 0 {
		t.Errorf("dual_signed = %v on an unarmed override, want 0", got)
	}
}

// THE ZEROES ARE PRESENT FROM THE FIRST SCRAPE.
//
// A counter that materialises only when it first fires makes "no self-approval
// has ever been attempted" and "this build has no such check" identical on a
// dashboard, and the second is the one worth knowing.
func TestDualControl_EverySeriesExistsBeforeItFires(t *testing.T) {
	a := newDualControlServer(t, true)
	families, err := a.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, f := range families {
		seen[f.GetName()] = len(f.GetMetric())
	}
	if n := seen["kanz_datamaster_overrides_total"]; n != 2 {
		t.Errorf("kanz_datamaster_overrides_total has %d series before any override, want 2 "+
			"(single_signed and dual_signed)", n)
	}
	if n := seen["kanz_datamaster_override_proposals_total"]; n != 7 {
		t.Errorf("kanz_datamaster_override_proposals_total has %d series before any proposal, want 7", n)
	}
}

// ARMED WITHOUT A PROPOSAL STORE MUST NOT SILENTLY FALL BACK TO ONE SIGNATURE.
//
// The composition root refuses to start in this state. This pins the second
// line: if the flag is somehow set without a store, the server must not treat
// "cannot record a proposal" as "apply it on one signature" — which would leave
// an operator believing dual control was on while one person kept overriding
// alone. Applying is the wrong direction to fail in; here it degrades to the
// documented unarmed behaviour, which is at least COUNTED as single-signed.
func TestArmedWithoutAStore_IsNotSilentlyDualSigned(t *testing.T) {
	feeds := testFeeds()
	golden := store.NewMemoryGoldenStore()
	exceptions := store.NewQueueStore(pricing.NewQueue(), nil)
	proj := projector.New(feeds, golden, exceptions, nil, projector.WithClock(func() time.Time { return now }))
	if err := proj.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := &Readiness{}
	r.Set(true)
	reg := prometheus.NewRegistry()
	s := New(r, nil, testTenant, golden, exceptions, feeds,
		WithClock(func() time.Time { return now }),
		WithDualControl(DualControl{Require: true, Registerer: reg})) // no Proposals
	a := armed{s: s, exceptions: exceptions, reg: reg}

	id := openExceptionID(t, s, exceptions)
	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override", "alice@kanz",
		`{"reason":"vendor confirmed","chosen_price":"130"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want the documented unarmed behaviour (200), got %d (%s)", rec.Code, rec.Body.String())
	}
	o := a.exception(t, id).Overrides[0]
	if o.DualSigned() {
		t.Fatal("an override applied with no second signature was recorded as DUAL SIGNED — the audit " +
			"trail would claim four-eyes on a control that was never armed")
	}
	if got := testutil.ToFloat64(s.overrideMetrics.signatures.WithLabelValues("single_signed")); got != 1 {
		t.Errorf("single_signed = %v, want 1", got)
	}
}

// THE STORE REFUSES A SELF-APPROVED RECORD TOO.
//
// The handler refuses it first. This is the audit record itself, and a store that
// will write actor == approver leaves the clause the whole control rests on
// defended in exactly one place — the place a future caller bypasses.
func TestOverrideRecord_AStoreWillNotWriteASelfApproval(t *testing.T) {
	a := newDualControlServer(t, false)
	id := openExceptionID(t, a.s, a.exceptions)

	for _, approver := range []string{"alice@kanz", "Alice@kanz", " alice@kanz "} {
		err := a.exceptions.Override(context.Background(), id, pricing.Override{
			Actor: "alice@kanz", Approver: approver, Reason: "r", ChosenPrice: bigRat(t, "130"), At: now,
		}, store.Claim{})
		if err == nil {
			t.Errorf("the store wrote an override where the approver (%q) is the actor — a self-approval "+
				"is recorded as dual control", approver)
		}
	}
}

func bigRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("bad rat %q", s)
	}
	return r
}
