package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/compliance/internal/api"
	"github.com/eighred/kanz/services/compliance/internal/store"
)

const (
	tenantA   = "acme"
	tenantB   = "beta-capital"
	portfolio = "PF-GROWTH"

	proposerAkif = "operator:akif"
	// approverDana is a DIFFERENT person. Everything below that says "two people"
	// means these two subjects.
	approverDana = "operator:dana"
	reason       = "Q3 mandate, approved by the IC"
)

// recordingBus is the broker. It records the events the publisher emits so a test
// can read what a consumer WOULD arm from.
//
// THIS DOUBLE DOES NOT WEAKEN THE CONTROL, which is worth stating because
// fakeBus's habit of accepting what a real broker rejects is a known trap here.
// The publisher under test is the REAL internal/compliance.Publisher, and its
// first act is approval.Covers — which re-derives every check from the evidence
// and refuses the zero value. Nothing this double does can make an unapproved
// mandate publishable.
type recordingBus struct {
	mu     sync.Mutex
	events []bus.Event
	fail   error
}

func (b *recordingBus) Publish(_ context.Context, e bus.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.events = append(b.events, e)
	return nil
}

func (b *recordingBus) published() []bus.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bus.Event(nil), b.events...)
}

type rig struct {
	srv   *api.Server
	bus   *recordingBus
	store store.ProposalStore
	now   time.Time
}

func newRig(t *testing.T, opts ...api.Option) *rig {
	t.Helper()
	r := &rig{
		bus:   &recordingBus{},
		store: store.NewMemoryProposals(),
		now:   time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
	}
	all := append([]api.Option{api.WithClock(func() time.Time { return r.now })}, opts...)
	r.srv = api.New(r.store, comp.NewPublisher(r.bus),
		slog.New(slog.NewTextHandler(io.Discard, nil)), all...)
	return r
}

func mandateJSON(t *testing.T, version int64, rules int) string {
	t.Helper()
	m := map[string]any{
		"mandate_id":   "M-2026Q3",
		"portfolio_id": portfolio,
		"version":      version,
		"effective_at": "2026-09-01T00:00:00Z",
	}
	if rules > 0 {
		limits := make([]any, 0, rules)
		for i := 0; i < rules; i++ {
			limits = append(limits, map[string]any{"rule_id": "R" + string(rune('1'+i))})
		}
		m["rules"] = limits
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal mandate: %v", err)
	}
	return string(b)
}

// do issues one REQUEST as one authenticated principal. Every case below goes
// through this, because the property under test is that the two signatures are
// two separately authenticated REQUESTS rather than two calls in one process.
func (r *rig) do(t *testing.T, method, path, subject, tenant, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if subject != "" || tenant != "" {
		auth.SetPrincipalHeaders(req.Header, subject, tenant, nil)
	}
	rr := httptest.NewRecorder()
	r.srv.ServeHTTP(rr, req)
	return rr
}

func (r *rig) propose(t *testing.T, subject, tenant string, version int64) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"mandate":` + mandateJSON(t, version, 2) + `,"reason":"` + reason + `"}`
	return r.do(t, http.MethodPost, "/v1/portfolios/"+portfolio+"/mandate", subject, tenant, body)
}

func (r *rig) approve(t *testing.T, subject, tenant, proposalID, decision string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"proposal_id":"` + proposalID + `","decision":"` + decision + `"}`
	return r.do(t, http.MethodPost, "/v1/portfolios/"+portfolio+"/mandate/approve", subject, tenant, body)
}

func decodeMap(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return out
}

func proposalIDOf(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	id, _ := decodeMap(t, rr)["proposal_id"].(string)
	if id == "" {
		t.Fatalf("no proposal_id in %q", rr.Body.String())
	}
	return id
}

// THE ASSERTION #562 WAS FILED FOR.
//
// One authenticated principal proposes, a DIFFERENT authenticated principal
// approves, and the FACT that arms every OMS and compliance replica carries both
// identities. Two REQUESTS, two credentials — not two invocations one person can
// run on their own machine.
func TestAMandateChangeTakesTwoAuthenticatedPeople(t *testing.T) {
	r := newRig(t)

	proposed := r.propose(t, proposerAkif, tenantA, 3)
	if proposed.Code != http.StatusAccepted {
		t.Fatalf("propose = %d, want 202 (%s)", proposed.Code, proposed.Body.String())
	}
	// PROPOSING PUBLISHES NOTHING. If it did, the second signature would be
	// decoration on a change that had already taken effect.
	if got := r.bus.published(); len(got) != 0 {
		t.Fatalf("proposing published %d event(s) — the mandate is already in force and the "+
			"approver's signature decides nothing", len(got))
	}
	id := proposalIDOf(t, proposed)

	approved := r.approve(t, approverDana, tenantA, id, "approve")
	if approved.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200 (%s)", approved.Code, approved.Body.String())
	}

	events := r.bus.published()
	if len(events) != 1 {
		t.Fatalf("published %d event(s), want exactly 1", len(events))
	}
	cc, ok := events[0].Payload.(*lifecyclepb.ConfigChanged)
	if !ok {
		t.Fatalf("payload is %T, want *lifecyclepb.ConfigChanged", events[0].Payload)
	}
	if cc.GetChangedBy() != proposerAkif || cc.GetApprovedBy() != approverDana {
		t.Errorf("the FACT records changed_by=%q approved_by=%q, want %q and %q.\n\n"+
			"The audit value of dual control is entirely in the trail naming TWO people; a "+
			"FACT that names one is the state #410 opened with",
			cc.GetChangedBy(), cc.GetApprovedBy(), proposerAkif, approverDana)
	}
	if cc.GetReason() != reason {
		t.Errorf("reason = %q, want %q — it is inside the digest, so the recorded "+
			"justification must be the one that was signed for", cc.GetReason(), reason)
	}
	if events[0].TenantID != tenantA {
		t.Errorf("the FACT is stamped tenant %q, want %q", events[0].TenantID, tenantA)
	}
	if events[0].Subject != comp.SubjectMandateFor(tenantA, portfolio) {
		t.Errorf("subject = %q, want the portfolio's own compacted subject %q — a mandate on the "+
			"wrong subject arms nobody", events[0].Subject, comp.SubjectMandateFor(tenantA, portfolio))
	}

	// AND THE PROPOSAL IS GONE from the queue. A decided proposal that keeps
	// listing is work an approver is shown after somebody did it.
	queue := r.do(t, http.MethodGet, "/v1/mandates/pending-changes", approverDana, tenantA, "")
	if queue.Code != http.StatusOK {
		t.Fatalf("queue = %d, want 200", queue.Code)
	}
	if strings.Contains(queue.Body.String(), id) {
		t.Errorf("the published proposal is still on the queue: %s", queue.Body.String())
	}
}

// SELF-APPROVAL IS REFUSED ACROSS TWO REQUESTS, INCLUDING THE RESPELLING.
//
// This is the whole point of the change and the case the CLI could never make:
// there, the two steps ran under one SVID and nothing compared credentials. Here
// the proposer's own token is presented to the approve route and is refused —
// and refused again when the same person is spelled differently, because a
// case-sensitive comparison would call them two people and the trail would read
// as four eyes.
func TestApprove_TheProposerCannotSignTheirOwnMandateChange(t *testing.T) {
	r := newRig(t)
	id := proposalIDOf(t, r.propose(t, proposerAkif, tenantA, 4))

	for _, spelling := range []string{
		proposerAkif,       // the same token
		"operator:Akif",    // one capital letter
		"  operator:akif ", // surrounding space
		"OPERATOR:AKIF",    // shouting
	} {
		rr := r.approve(t, spelling, tenantA, id, "approve")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%q approved what %q proposed: status = %d, want 403.\n\n"+
				"One person now holds both signatures on the control every order in this "+
				"portfolio is checked against, and the FACT names two actors — which reads as "+
				"satisfied in exactly the record an auditor would check",
				spelling, proposerAkif, rr.Code)
		}
		if got := r.bus.published(); len(got) != 0 {
			t.Fatalf("%q published a mandate: %d event(s)", spelling, len(got))
		}
	}

	// THE REFUSED ATTEMPT MUST NOT CONSUME THE PROPOSAL. Otherwise one person can
	// destroy a colleague's pending decision by attempting their own approval and
	// being refused.
	if rr := r.approve(t, approverDana, tenantA, id, "approve"); rr.Code != http.StatusOK {
		t.Fatalf("a legitimate approver was refused after the self-approval attempts: %d (%s)",
			rr.Code, rr.Body.String())
	}
}

// A PROPOSAL BELONGS TO ONE TENANT, AND THE ANSWER IS 404 RATHER THAN 403.
//
// A distinct status would make this route an oracle: iterate proposal ids and the
// status code alone enumerates another fund's pending mandate changes.
func TestApprove_AnotherTenantCannotSeeOrSignIt(t *testing.T) {
	r := newRig(t)
	id := proposalIDOf(t, r.propose(t, proposerAkif, tenantA, 5))

	rr := r.approve(t, approverDana, tenantB, id, "approve")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("tenant %q reached tenant %q's proposal: status = %d, want 404 (%s)",
			tenantB, tenantA, rr.Code, rr.Body.String())
	}
	if got := r.bus.published(); len(got) != 0 {
		t.Fatal("another tenant published a mandate for a portfolio it does not own")
	}

	queue := r.do(t, http.MethodGet, "/v1/mandates/pending-changes", approverDana, tenantB, "")
	if strings.Contains(queue.Body.String(), id) {
		t.Errorf("tenant %q's queue names tenant %q's pending mandate change: %s",
			tenantB, tenantA, queue.Body.String())
	}
}

// A MANDATE NAMING ANOTHER TENANT IS REFUSED AT PROPOSE TIME. Without this an
// operator files a mandate for a portfolio in a book they have no claim on, and
// the approver in THEIR tenant would be the one asked to sign it.
func TestPropose_AMandateForAnotherTenantIsRefused(t *testing.T) {
	r := newRig(t)
	body := `{"mandate":{"mandate_id":"M1","tenant_id":"` + tenantB + `","portfolio_id":"` +
		portfolio + `","version":1,"effective_at":"2026-09-01T00:00:00Z"},"reason":"` + reason + `"}`

	rr := r.do(t, http.MethodPost, "/v1/portfolios/"+portfolio+"/mandate", proposerAkif, tenantA, body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rr.Code, rr.Body.String())
	}
}

// THE PATH NAMES THE PORTFOLIO THE APPROVER READS. A body that names a different
// one is refused rather than silently overwritten: an approver shown one
// portfolio and signing for another is the forged-context defect one layer up
// from a forged actor.
func TestPropose_ABodyThatNamesAnotherPortfolioIsRefused(t *testing.T) {
	r := newRig(t)
	body := `{"mandate":{"mandate_id":"M1","portfolio_id":"PF-OTHER","version":1,` +
		`"effective_at":"2026-09-01T00:00:00Z"},"reason":"` + reason + `"}`

	rr := r.do(t, http.MethodPost, "/v1/portfolios/"+portfolio+"/mandate", proposerAkif, tenantA, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
}

// AN UNAUTHENTICATED CALLER REACHES NEITHER HALF. This is the property the
// NetworkPolicy and the split listener exist to make meaningful: with no
// principal there is nobody to be the first or the second signature.
func TestEveryRouteRefusesAnUnauthenticatedCaller(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/portfolios/" + portfolio + "/mandate",
			`{"mandate":` + mandateJSON(t, 1, 1) + `,"reason":"x"}`},
		{http.MethodPost, "/v1/portfolios/" + portfolio + "/mandate/approve",
			`{"proposal_id":"p","decision":"approve"}`},
		{http.MethodGet, "/v1/mandates/pending-changes", ""},
	} {
		rr := r.do(t, c.method, c.path, "", "", c.body)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d to an anonymous caller, want 401", c.method, c.path, rr.Code)
		}
	}
}

// A TOKEN WITH NO TENANT MAY NOT PROPOSE, AND NOTHING IS FILED UNDER "".
//
// Neither gateway authenticator requires the tenant claim, so this arrives with a
// perfectly valid token. The tenant scopes every read and every write on this
// surface, so a proposal filed under an empty key would list on no queue and be
// approvable by nobody — a silent drop rather than a refusal, which is the one
// outcome this control cannot have.
//
// THE STATUS IS 401 AND NOT THE 403 THE GATEWAY'S ORDER PATH ANSWERS, because
// auth.PrincipalFromHeaders refuses a missing subject and a missing tenant with
// one answer and this service cannot tell them apart without reimplementing the
// seam #258 consolidated. Asserted rather than glossed: the refusal is the
// property, and 401 is what pkg/auth can honestly say.
func TestPropose_ATokenWithNoTenantIsRefused(t *testing.T) {
	r := newRig(t)
	rr := r.do(t, http.MethodPost, "/v1/portfolios/"+portfolio+"/mandate", proposerAkif, "",
		`{"mandate":`+mandateJSON(t, 1, 1)+`,"reason":"`+reason+`"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", rr.Code, rr.Body.String())
	}
	if got, err := r.store.Pending(context.Background(), store.TenantScope(""), r.now); err != nil {
		t.Fatalf("Pending: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("a tenant-less caller filed %d proposal(s) under an empty key — nothing lists "+
			"them and nobody can sign them", len(got))
	}
}

// AN EXPIRED PROPOSAL CANNOT BE SIGNED. Without a deadline an approval collected
// today applies against next quarter's book.
func TestApprove_AnExpiredProposalIsRefused(t *testing.T) {
	r := newRig(t, api.WithTTL(time.Hour))
	id := proposalIDOf(t, r.propose(t, proposerAkif, tenantA, 6))

	r.now = r.now.Add(2 * time.Hour)
	rr := r.approve(t, approverDana, tenantA, id, "approve")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rr.Code, rr.Body.String())
	}
	if got := r.bus.published(); len(got) != 0 {
		t.Fatal("an expired proposal was published")
	}

	// AND IT IS DISCOVERABLE RATHER THAN SILENT (#563). "Nobody signed it" and "it
	// never existed" must not be the same absence: for a mandate the difference is
	// whether the portfolio is still governed by the old constraint.
	queue := r.do(t, http.MethodGet, "/v1/mandates/pending-changes", proposerAkif, tenantA, "")
	if !strings.Contains(queue.Body.String(), dualcontrol.StateLapsed) {
		t.Errorf("the lapsed proposal is in no queue at all: %s\n\n"+
			"The proposer is left inferring the outcome from an absence", queue.Body.String())
	}
}

// A STORED MANDATE THAT NO LONGER MATCHES ITS DIGEST IS REFUSED. The approval
// covers the VALUE; if the two ever disagree the safe answer is to refuse, never
// to publish what the store now holds.
func TestApprove_APayloadThatChangedUnderTheProposalIsRefused(t *testing.T) {
	r := newRig(t)
	id := proposalIDOf(t, r.propose(t, proposerAkif, tenantA, 7))

	// Reach past the API and corrupt the record the way a bad migration or a
	// second writer would: same proposal, different digest.
	prop, ok, err := r.store.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if _, err := r.store.Claim(context.Background(), id); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	prop.Digest = dualcontrol.Digest("something", "else")
	if err := r.store.Put(context.Background(), prop); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rr := r.approve(t, approverDana, tenantA, id, "approve")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rr.Code, rr.Body.String())
	}
	if got := r.bus.published(); len(got) != 0 {
		t.Fatal("a mandate whose digest no longer covers it was published")
	}
}

// A REJECTION TAKES THE PROPOSAL OFF THE QUEUE AND PUBLISHES NOTHING, and it
// needs no second-person check: refusing an act is always safe, including by the
// proposer withdrawing their own.
func TestApprove_ARejectionWithdrawsTheProposal(t *testing.T) {
	r := newRig(t)
	id := proposalIDOf(t, r.propose(t, proposerAkif, tenantA, 8))

	if rr := r.approve(t, proposerAkif, tenantA, id, "reject"); rr.Code != http.StatusOK {
		t.Fatalf("reject = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if got := r.bus.published(); len(got) != 0 {
		t.Fatal("a rejected mandate change was published")
	}
	if rr := r.approve(t, approverDana, tenantA, id, "approve"); rr.Code != http.StatusNotFound {
		t.Fatalf("a rejected proposal was still approvable: %d", rr.Code)
	}
}

// AN EMPTY DECISION IS REFUSED RATHER THAN ASSUMED. Both assumptions are wrong:
// defaulting to approve publishes a mandate nobody consented to, defaulting to
// reject discards a decision silently.
func TestApprove_AnEmptyDecisionIsRefused(t *testing.T) {
	r := newRig(t)
	id := proposalIDOf(t, r.propose(t, proposerAkif, tenantA, 9))

	rr := r.do(t, http.MethodPost, "/v1/portfolios/"+portfolio+"/mandate/approve",
		approverDana, tenantA, `{"proposal_id":"`+id+`"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
	if got := r.bus.published(); len(got) != 0 {
		t.Fatal("an empty decision published a mandate")
	}
}

// ONE DECISION IS APPLIED ONCE. Two approvers acting on one pending change both
// pass every check — different people, matching digest, neither expired — and the
// claim is what elects one. Two FACTs for one decision on a COMPACTED stream is
// worse than it sounds: the last one wins and nothing records that there were two.
func TestApprove_ASecondApproverIsToldItIsAlreadyDecided(t *testing.T) {
	r := newRig(t)
	id := proposalIDOf(t, r.propose(t, proposerAkif, tenantA, 10))

	if rr := r.approve(t, approverDana, tenantA, id, "approve"); rr.Code != http.StatusOK {
		t.Fatalf("first approve = %d (%s)", rr.Code, rr.Body.String())
	}
	if rr := r.approve(t, "operator:carol", tenantA, id, "approve"); rr.Code != http.StatusNotFound {
		t.Fatalf("second approve = %d, want 404 — the proposal was claimed and is gone", rr.Code)
	}
	if got := r.bus.published(); len(got) != 1 {
		t.Fatalf("published %d event(s) for one decision, want 1", len(got))
	}
}

// A PUBLISH FAILURE AFTER A VALID SECOND SIGNATURE IS NOT A 200. The proposal is
// already claimed and nothing reached the broker, so the caller must be told the
// change did NOT take effect — a quiet success here is a mandate everybody
// believes is in force and no replica ever armed with.
func TestApprove_APublishFailureIsNotReportedAsSuccess(t *testing.T) {
	r := newRig(t)
	id := proposalIDOf(t, r.propose(t, proposerAkif, tenantA, 11))
	r.bus.fail = errBrokerDown

	rr := r.approve(t, approverDana, tenantA, id, "approve")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "NOT published") {
		t.Errorf("the reply does not say the mandate was not published: %s", rr.Body.String())
	}
}

var errBrokerDown = &brokerDownError{}

type brokerDownError struct{}

func (*brokerDownError) Error() string { return "broker unreachable" }

// AN INCOMPLETE MANDATE IS REFUSED BEFORE IT CAN BE PROPOSED. The MANDATE stream
// is compacted, so a malformed mandate is the LAST message on the portfolio's
// subject and every consumer that boots arms itself with it — forever.
func TestPropose_AnIncompleteMandateIsRefused(t *testing.T) {
	for _, c := range []struct{ name, mandate string }{
		{"no mandate_id", `{"portfolio_id":"` + portfolio + `","version":1,"effective_at":"2026-09-01T00:00:00Z"}`},
		{"no version", `{"mandate_id":"M1","portfolio_id":"` + portfolio + `","effective_at":"2026-09-01T00:00:00Z"}`},
		{"no effective_at", `{"mandate_id":"M1","portfolio_id":"` + portfolio + `","version":1}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			rr := r.do(t, http.MethodPost, "/v1/portfolios/"+portfolio+"/mandate",
				proposerAkif, tenantA, `{"mandate":`+c.mandate+`,"reason":"`+reason+`"}`)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

// A PROPOSAL WITH NO REASON IS REFUSED. The reason is inside the digest, so it is
// part of what the approver signs — and an unexplained change to what governs a
// portfolio is not auditable.
func TestPropose_AReasonIsRequired(t *testing.T) {
	r := newRig(t)
	rr := r.do(t, http.MethodPost, "/v1/portfolios/"+portfolio+"/mandate",
		proposerAkif, tenantA, `{"mandate":`+mandateJSON(t, 1, 1)+`}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
}

// THE QUEUE IS THE ONLY WAY AN APPROVER LEARNS A PROPOSAL ID. The propose reply
// goes to the PROPOSER, so without this the signature route is reachable and
// unusable — the state the order-approval route was in until its queue landed.
func TestPendingChanges_NamesTheProposerAndWhatIsBeingSigned(t *testing.T) {
	r := newRig(t)
	id := proposalIDOf(t, r.propose(t, proposerAkif, tenantA, 12))

	rr := r.do(t, http.MethodGet, "/v1/mandates/pending-changes", approverDana, tenantA, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var rows []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	if len(rows) != 1 {
		t.Fatalf("queue = %v, want one row", rows)
	}
	row := rows[0]
	for field, want := range map[string]any{
		"proposal_id":  id,
		"proposer":     proposerAkif,
		"portfolio_id": portfolio,
		"reason":       reason,
		"state":        dualcontrol.StatePending,
		"act":          string(dualcontrol.ActMandateChange),
	} {
		if row[field] != want {
			t.Errorf("%s = %v, want %v — an approver who cannot see WHO proposed it and WHAT it "+
				"changes is signing something unread, which is the failure the second signature "+
				"exists to prevent", field, row[field], want)
		}
	}
	if row["digest"] == "" || row["digest"] == nil {
		t.Error("the queue carries no digest — a client cannot check that what it displays is " +
			"what the signature will cover")
	}
}

// AN EMPTY QUEUE IS AN ANSWER: nothing awaits a signature. It is a 200 with an
// empty list rather than a 404, because "there is nothing pending" and "there is
// no such surface" are different facts and only one of them is true here.
func TestPendingChanges_AnEmptyQueueIsTwoHundred(t *testing.T) {
	r := newRig(t)
	rr := r.do(t, http.MethodGet, "/v1/mandates/pending-changes", approverDana, tenantA, "")
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("status = %d body = %q, want 200 []", rr.Code, rr.Body.String())
	}
}

// THE DIGEST COVERS THE MANDATE THE APPROVER SIGNS, and comp.MandateDigest is the
// one definition of it. This pins that the propose route publishes the SAME value
// both sides compute — a digest computed two ways is a digest that eventually
// differs, at which point every approval fails and the obvious fix is to stop
// checking it.
func TestPropose_TheAdvertisedDigestIsTheOneTheApprovalCovers(t *testing.T) {
	r := newRig(t)
	rr := r.propose(t, proposerAkif, tenantA, 13)
	advertised, _ := decodeMap(t, rr)["digest"].(string)

	want, err := comp.MandateDigest(&compliancepb.Mandate{
		MandateId: "M-2026Q3", TenantId: tenantA, PortfolioId: portfolio, Version: 13,
		EffectiveAt: timestamppb.New(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)),
		Rules: []*compliancepb.Rule{
			{RuleId: "R1"}, {RuleId: "R2"},
		},
	}, reason)
	if err != nil {
		t.Fatalf("MandateDigest: %v", err)
	}
	if advertised != want {
		t.Fatalf("the propose reply advertises digest %q, the canonical digest is %q — the two "+
			"sides of the signature are computing over different values", advertised, want)
	}
}
