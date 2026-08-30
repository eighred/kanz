package server

// AN ALREADY-DECIDED EXCEPTION IS A 409, ON BOTH DOORS (#816).
//
// The store refuses a second override; these tests are about what the operator's
// client is TOLD, which is the half a store test cannot reach. A 400 would put
// "your request was malformed" and "your first attempt already landed" behind one
// status code, and a client retrying after a timeout has to tell those apart —
// the first means fix the payload, the second means stop retrying and read the
// trail.
//
// Both doors are covered because both can arrive twice. The unarmed door has no
// protection at all and is the one production is on today
// (DATAMASTER_REQUIRE_DUAL_CONTROL defaults to false). The armed door was
// idempotent per PROPOSAL — a claim is single-use — but nothing stopped a second
// proposal being raised and approved against an exception already decided.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// THE UNARMED DOOR: a repeated single-signed override answers 409 and writes
// nothing.
func TestUnarmed_ASecondOverrideIs409AndWritesNothing(t *testing.T) {
	s, exceptions := newServer(t)
	id := openExceptionID(t, s, exceptions)

	first := postAsPrincipal("/v1/exceptions/"+id+"/override", testTenant, "alice@kanz",
		`{"reason":"corp action confirmed","chosen_price":"130"}`)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, first)
	if rec.Code != http.StatusOK {
		t.Fatalf("the first override: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}

	// A DIFFERENT PERSON AND A DIFFERENT PRICE. This is not a duplicate request
	// being suppressed; it is a second decision on a break that has been decided,
	// and the caller must be told rather than handed a 200 for a price the
	// append-only trail does not hold.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, postAsPrincipal("/v1/exceptions/"+id+"/override", testTenant, "bob@kanz",
		`{"reason":"re-reviewed","chosen_price":"160"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("a second override on a decided exception: want 409 got %d (%s).\n"+
			"200 would append a second authorisation to the audit trail for one act; 400 would "+
			"tell a retrying client its request was malformed.", rec.Code, rec.Body.String())
	}

	ex, ok, err := exceptions.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("read-back: ok=%v err=%v", ok, err)
	}
	if len(ex.Overrides) != 1 {
		t.Fatalf("%d override records for ONE decision: %+v", len(ex.Overrides), ex.Overrides)
	}
	if ex.Overrides[0].Actor != "alice@kanz" {
		t.Errorf("the refused override displaced the trail: actor = %q, want alice@kanz",
			ex.Overrides[0].Actor)
	}
	if ex.Status != pricing.StatusOverridden {
		t.Errorf("status = %s after a refused second override, want OVERRIDDEN", ex.Status)
	}
}

// THE ARMED DOOR: a proposal raised against an exception someone already decided
// is refused at approval, counted, and NOT consumed.
//
// Not consumed is the load-bearing half. The claim rides the override's
// transaction (#807), so a refusal that spent it would leave a second signature
// gone and no override applied — the failure #807 removed, coming back through
// this fix. The proposal stays on the pending queue and lapses there visibly
// (#563) instead.
func TestArmed_AnApprovalForADecidedExceptionIs409AndKeepsTheProposal(t *testing.T) {
	a := newDualControlServer(t, true)
	id := openExceptionID(t, a.s, a.exceptions)

	// Decide it the only way an armed server can: propose, then approve.
	firstProposal := a.propose(t, id, "alice@kanz", "130")
	rec := a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "bob@kanz",
		`{"proposal_id":"`+firstProposal+`","decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the first approval: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}

	// A SECOND PROPOSAL FOR THE SAME EXCEPTION. Nothing refuses this at propose
	// time and nothing should: proposing does not touch the exception, and the
	// answer belongs where the act is applied.
	second := a.propose(t, id, "carol@kanz", "160")

	rec = a.do(t, http.MethodPost, "/v1/exceptions/"+id+"/override/approve", "dave@kanz",
		`{"proposal_id":"`+second+`","decision":"approve"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approving an override for an already-decided exception: want 409 got %d (%s)",
			rec.Code, rec.Body.String())
	}

	if _, found, err := a.proposals.Get(context.Background(), second); err != nil || !found {
		t.Fatalf("the refusal consumed the proposal (found=%v err=%v). The second signature is "+
			"spent on an override that did not happen, which is exactly what #807 removed.",
			found, err)
	}
	ex := a.exception(t, id)
	if len(ex.Overrides) != 1 {
		t.Fatalf("%d override records for ONE decision: %+v", len(ex.Overrides), ex.Overrides)
	}
	if got := testutil.ToFloat64(
		a.s.overrideMetrics.proposals.WithLabelValues("refused_already_overridden")); got != 1 {
		t.Errorf("refused_already_overridden = %v, want 1 — a duplicate-audit refusal that is not "+
			"counted is a control nobody can see firing", got)
	}
}
