// Package api is act two of #410: changing a mandate takes TWO PEOPLE, not two
// invocations (#562).
//
// # What was wrong, precisely
//
// cmd/kanz-mandate already took two steps, and its own doc said what that could
// and could not buy:
//
//	IT DOES NOT AUTHENTICATE THE TWO HUMANS. Both invocations run on one
//	operator's machine under one SVID, and the proposal file is a CARRIER, not a
//	signature — one person can run both steps.
//
// So a unilateral mandate change was DETECTABLE — the FACT records four eyes and
// the digest covers the exact mandate and reason — and it was not PREVENTABLE.
// That matters more here than for the other two acts because a mandate is the
// control every order is checked against: relax it, then place the order it would
// have refused, and both acts read as correct in the trail.
//
// This package is the surface that makes it preventable. Two SEPARATELY
// AUTHENTICATED REQUESTS through the api-gateway, which authenticates each caller
// and injects the principal — the shape act one (the pricing override) and act
// three (the held order) already have.
//
// # Why the proposal lives here and not in a file
//
// Two requests cannot pass a file between them, so the proposal rests in
// services/compliance/internal/store, which is built on the shared
// internal/dualcontrol/proposalstore promoted for exactly this act.
//
// # Why this is a SEPARATE LISTENER from /metrics
//
// The whole authorization story for these routes is X-Kanz-Principal-*, and that
// is sound only while the api-gateway is this service's only reachable caller.
// allow-observability-scrape admits whatever port serves /metrics from the entire
// kanz-observability namespace, so putting these routes on that port would let a
// pod there choose its own principal and both PROPOSE and APPROVE a mandate
// change — one pod holding both signatures, which is precisely what this package
// exists to prevent. optimization (#409) and accounting (#447) split their
// listeners for weaker reasons than this one.
//
// # The rule itself is not restated here
//
// internal/dualcontrol owns "the approver is not the proposer", "the signature
// covers the payload" and "a proposal expires". internal/compliance's Publisher
// takes a dualcontrol.Approval and has no argument for a lone actor, so no code
// path in this package can publish a unilateral mandate — the refusal is in the
// signature rather than in a check somebody has to remember.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/encoding/protojson"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/dualcontrol/proposalstore"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/compliance/internal/store"
)

// maxBodyBytes bounds a proposal body. A mandate is a rule list, not a data set.
const maxBodyBytes = 1 << 20 // 1 MiB

// Publisher is the mandate publish surface — satisfied by *comp.Publisher.
//
// IT TAKES A dualcontrol.Approval AND THAT IS THE SEAM'S WHOLE VALUE. A test
// double for this interface cannot be handed two names by one person either: the
// Approval type's fields are unexported, so nothing outside internal/dualcontrol
// can forge one, and the zero value is refused by Covers. Faking the bus does not
// fake the control.
type Publisher interface {
	Publish(ctx context.Context, m, previous *compliancepb.Mandate, approval dualcontrol.Approval, reason string) error
}

// Server serves the mandate-change routes.
type Server struct {
	proposals store.ProposalStore
	publisher Publisher
	ttl       time.Duration
	now       func() time.Time
	logger    *slog.Logger
	mux       *http.ServeMux
}

// Option configures a Server.
type Option func(*Server)

// WithClock replaces time.Now. Tests only; production reads the wall clock,
// because a proposal's expiry has to be comparable to the one an approver in
// another timezone is looking at.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithTTL sets how long a pending proposal stays approvable. Zero or negative
// leaves dualcontrol.DefaultTTL.
func WithTTL(ttl time.Duration) Option {
	return func(s *Server) {
		if ttl > 0 {
			s.ttl = ttl
		}
	}
}

// New returns the mandate-change API.
//
// BOTH DEPENDENCIES ARE REQUIRED PARAMETERS RATHER THAN OPTIONS, and that is not
// style. A Server with no store would take every proposal, record it nowhere and
// answer 202 to a proposer whose change no approver can ever find — an act that
// neither takes effect nor reports why. A Server with no publisher would take the
// second signature and publish nothing. Both are the failure this control exists
// to end, arriving through the constructor. A deployment that has neither does
// not build a crippled Server — it does not MOUNT this listener at all, which is
// what the composition root does when no bus is configured, and what makes the
// absence a 404 rather than a route that answers 500 forever.
func New(proposals store.ProposalStore, publisher Publisher, logger *slog.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		proposals: proposals,
		publisher: publisher,
		ttl:       dualcontrol.DefaultTTL,
		now:       func() time.Time { return time.Now().UTC() },
		logger:    logger,
		mux:       http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Routes registers the three routes. They are 1:1 with the gateway's, so the
// gateway path IS the upstream path and nothing rewrites.
func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/portfolios/{id}/mandate", s.handlePropose)
	s.mux.HandleFunc("POST /v1/portfolios/{id}/mandate/approve", s.handleApprove)
	s.mux.HandleFunc("GET /v1/mandates/pending-changes", s.handlePendingChanges)
}

// proposeRequest is what a proposer sends.
//
// THE MANDATE IS RAW JSON AND IS DECODED WITH protojson, not with encoding/json.
// A hand-rolled struct here would be a second definition of what a mandate is,
// and the field it silently dropped would be a constraint the approver never saw
// and the published mandate never carried.
type proposeRequest struct {
	Mandate json.RawMessage `json:"mandate"`
	Reason  string          `json:"reason"`
}

// handlePropose records a pending mandate change and answers 202. IT PUBLISHES
// NOTHING, which is the strongest statement the surface can make: the route that
// reaches the broker is the other one, and it cannot be called by this caller.
func (s *Server) handlePropose(w http.ResponseWriter, r *http.Request) {
	proposer, tenant, ok := s.principal(w, r)
	if !ok {
		return
	}
	portfolioID := r.PathValue("id")
	if portfolioID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no portfolio in the path"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable or oversized body"})
		return
	}
	var req proposeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "reason is required: an unexplained change to what governs a portfolio is not " +
				"auditable, and the reason is inside the digest the approver signs",
		})
		return
	}
	var m compliancepb.Mandate
	if err := protojson.Unmarshal(req.Mandate, &m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "mandate is not a compliance.v1.Mandate: " + err.Error(),
		})
		return
	}

	// THE PATH AND THE PRINCIPAL WIN OVER THE BODY, exactly as they do on the
	// order-approval route. A body-supplied tenant is one fund's operator filing a
	// mandate for another's portfolio; a body-supplied portfolio means the approver
	// is shown one portfolio in the URL and signs for another. Defaulting the empty
	// case is a convenience; DISAGREEING is refused rather than overwritten, so a
	// caller that meant something else is told, instead of having it silently
	// changed under them.
	if m.GetTenantId() == "" {
		m.TenantId = tenant
	}
	if m.GetTenantId() != tenant {
		// 403, not 404: the caller is authenticated and named a tenant that is not
		// theirs. There is no oracle here — the answer does not depend on whether
		// that tenant or that portfolio exists.
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "this mandate names another tenant; a token may only change what governs its own book",
		})
		return
	}
	if m.GetPortfolioId() == "" {
		m.PortfolioId = portfolioID
	}
	if m.GetPortfolioId() != portfolioID {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "the mandate names portfolio " + m.GetPortfolioId() + " but the path names " +
				portfolioID + " — an approver reads the path and must not be signing for another portfolio",
		})
		return
	}
	if err := comp.ValidateMandate(&m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	digest, err := comp.MandateDigest(&m, req.Reason)
	if err != nil {
		s.logger.Error("compliance: cannot digest a mandate", "portfolio_id", portfolioID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot record the proposal"})
		return
	}
	id, err := mintProposalID()
	if err != nil {
		s.logger.Error("compliance: cannot mint a proposal id", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot record the proposal"})
		return
	}
	base, err := dualcontrol.Propose(id, dualcontrol.ActMandateChange,
		comp.MandateConfigKey(m.GetTenantId(), m.GetPortfolioId()), proposer, digest, s.now(), s.ttl)
	if err != nil {
		// Propose refuses only malformed input, and every field above has already
		// been checked — so this is a defect HERE, not in the request, and must not
		// be reported as the operator's fault.
		s.logger.Error("compliance: built an unapprovable mandate proposal",
			"portfolio_id", portfolioID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot record the proposal"})
		return
	}
	prop := store.MandateProposal{Proposal: base, Reason: req.Reason, Mandate: &m}
	if err := s.proposals.Put(r.Context(), prop); err != nil {
		if errors.Is(err, proposalstore.ErrExists) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "that proposal id is already held"})
			return
		}
		s.logger.Error("compliance: cannot store a mandate proposal",
			"portfolio_id", portfolioID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot record the proposal"})
		return
	}

	// 202, NOT 200. Nothing has been published and no replica has armed. A 200
	// would tell a client the mandate is in force, which is the silent-drop this
	// control exists to make impossible.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":       "PENDING_APPROVAL",
		"proposal_id":  prop.ID,
		"portfolio_id": m.GetPortfolioId(),
		"mandate_id":   m.GetMandateId(),
		"version":      m.GetVersion(),
		// THE RULE COUNT IS REPORTED RATHER THAN REFUSED. An empty ruleset is legal
		// and means "governed by a mandate that constrains nothing", which is
		// different from having no mandate — the approver has to be able to see that
		// is what they are signing.
		"rule_count": len(m.GetRules()),
		"proposer":   prop.Proposer,
		"digest":     prop.Digest,
		"expires_at": prop.ExpiresAt,
		"message": "recorded, NOT published: this mandate takes effect when a DIFFERENT authenticated " +
			"person approves it at POST /v1/portfolios/" + m.GetPortfolioId() + "/mandate/approve",
	})
}

// approveRequest is the second signature.
type approveRequest struct {
	ProposalID string `json:"proposal_id"`
	// Decision is "approve" or "reject". THERE IS NO DEFAULT: an empty decision
	// is refused rather than assumed, because both possible assumptions are
	// wrong — defaulting to approve publishes a mandate nobody consented to, and
	// defaulting to reject discards a decision silently. Same stance as the
	// override path's.
	Decision string `json:"decision"`
}

// handleApprove is the second signature, and the only route on this service that
// reaches the broker.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	approver, tenant, ok := s.principal(w, r)
	if !ok {
		return
	}
	portfolioID := r.PathValue("id")

	var req approveRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `decision must be "approve" or "reject"`,
		})
		return
	}

	prop, found, err := s.proposals.Get(r.Context(), req.ProposalID)
	if err != nil {
		s.logger.Error("compliance: cannot read a mandate proposal",
			"proposal_id", req.ProposalID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proposal store unavailable"})
		return
	}
	// THE SUBJECT CARRIES BOTH THE TENANT AND THE PORTFOLIO, so this one comparison
	// is the tenant gate AND the "an approver must not be shown one portfolio while
	// signing for another" gate. Getting either wrong is the same 404 as an unknown
	// id: distinguishing them would make this route an oracle for other tenants'
	// pending mandate changes.
	if !found || prop.Subject != comp.MandateConfigKey(tenant, portfolioID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": notFoundBody})
		return
	}

	if req.Decision == "reject" {
		// A rejection needs no second-person check: refusing an act is always safe,
		// including by the proposer withdrawing their own.
		claimed, cerr := s.proposals.Claim(r.Context(), prop.ID)
		if cerr != nil {
			s.logger.Error("compliance: cannot claim a mandate proposal", "proposal_id", prop.ID, "err", cerr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proposal store unavailable"})
			return
		}
		if !claimed {
			writeJSON(w, http.StatusConflict, map[string]string{"error": alreadyDecidedBody})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "REJECTED", "proposal_id": prop.ID, "rejected_by": approver,
		})
		return
	}

	// THE DIGEST IS RE-DERIVED FROM WHAT THE STORE HOLDS, never taken from the
	// proposal record and never supplied by the approver's client. The record's
	// digest is what was signed for; this is what is about to be published. They
	// are compared by Approve, so a stored mandate that changed under the proposal
	// is refused rather than published.
	digest, err := comp.MandateDigest(prop.Mandate, prop.Reason)
	if err != nil {
		s.logger.Error("compliance: cannot digest a stored mandate", "proposal_id", prop.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the proposal is not well-formed"})
		return
	}
	// THE RULE, CHECKED BEFORE THE CLAIM. A refused approval must leave the
	// proposal PENDING for somebody who may legitimately sign it — consuming it on
	// a self-approval attempt lets one person destroy a colleague's pending
	// decision by attempting their own.
	approval, err := prop.Approve(approver, digest, s.now())
	if err != nil {
		s.refuseApproval(w, prop, approver, err)
		return
	}

	claimed, err := s.proposals.Claim(r.Context(), prop.ID)
	if err != nil {
		s.logger.Error("compliance: cannot claim a mandate proposal", "proposal_id", prop.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proposal store unavailable"})
		return
	}
	if !claimed {
		// Somebody else decided it between the read and here. Refusing is the only
		// safe answer: publishing would put two ConfigChanged FACTs for one decision
		// onto a compacted stream where the last one wins.
		writeJSON(w, http.StatusConflict, map[string]string{"error": alreadyDecidedBody})
		return
	}

	// previous IS NIL, and that is honest rather than lazy. This process holds a
	// mandate registry, but it is the POST-TRADE MONITOR's armed view and it is
	// filled by a replay that may still be in flight — a previous_value read from
	// it could disagree with the compacted stream, and an INVENTED previous is
	// worse than an absent one. The mandate's own version carries the ordering.
	if err := s.publisher.Publish(r.Context(), prop.Mandate, nil, approval, prop.Reason); err != nil {
		// THE PROPOSAL IS ALREADY CLAIMED AND NOTHING WAS PUBLISHED. Say so loudly:
		// the proposer must re-propose, and an operator needs to know why a decision
		// two people made did not take effect. A quiet 500 here is a mandate change
		// that everyone believes happened.
		s.logger.Error("compliance: a dual-signed mandate change was approved but NOT published — "+
			"the proposal is consumed and must be re-proposed",
			"proposal_id", prop.ID, "portfolio_id", prop.Mandate.GetPortfolioId(),
			"proposer", approval.Proposer(), "approver", approval.Approver(), "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "the approval was valid and the mandate was NOT published; it must be proposed again",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "PUBLISHED",
		"proposal_id":  prop.ID,
		"portfolio_id": prop.Mandate.GetPortfolioId(),
		"mandate_id":   prop.Mandate.GetMandateId(),
		"version":      prop.Mandate.GetVersion(),
		"proposed_by":  approval.Proposer(),
		"approved_by":  approval.Approver(),
		"reason":       prop.Reason,
		"message": "every OMS and compliance replica arms with this mandate — including one that " +
			"boots tomorrow",
	})
}

// refuseApproval maps a dualcontrol refusal onto a status and a message that
// NAMES THE RULE.
//
// THE REFUSAL IS SYNCHRONOUS, like the override path's and unlike the OMS's. A
// mandate change is approved over HTTP, so the answer goes back on the same
// request and there is no window in which a refused approval sits somewhere
// waiting to be discovered — which is why this act adds no "refused" state to the
// queue. internal/dualcontrol's own doc records why the three acts differ here
// and why making them the same would be a channel with no reader.
func (s *Server) refuseApproval(w http.ResponseWriter, prop store.MandateProposal, approver string, err error) {
	switch {
	case errors.Is(err, dualcontrol.ErrSelfApproval):
		// 403, not 400. The request is well-formed; the person is not permitted.
		// THE PROPOSAL STAYS PENDING — see the call site.
		//
		// LOGGED AT WARN AND NAMED. This is the control firing, and it is the line
		// an auditor asks for: one person attempted both signatures on the constraint
		// every order in a portfolio is checked against.
		s.logger.Warn("compliance: self-approval REFUSED on a mandate change — one person attempted "+
			"both signatures on the control every order is checked against",
			"proposal_id", prop.ID, "portfolio_id", prop.Mandate.GetPortfolioId(),
			"proposer", prop.Proposer, "approver", approver)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "the approver must be a different person from the proposer; this mandate change " +
				"was proposed by " + prop.Proposer,
		})
	case errors.Is(err, dualcontrol.ErrExpired):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this proposal expired before it was approved and must be proposed again",
		})
	case errors.Is(err, dualcontrol.ErrPayloadChanged):
		s.logger.Error("compliance: a mandate proposal's payload no longer matches its digest — the "+
			"stored mandate changed after it was proposed",
			"proposal_id", prop.ID, "portfolio_id", prop.Mandate.GetPortfolioId())
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the proposed mandate no longer matches what was signed for",
		})
	default:
		s.logger.Error("compliance: malformed mandate proposal reached approval",
			"proposal_id", prop.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the proposal is not well-formed"})
	}
}

// handlePendingChanges lists mandate changes awaiting a second signature.
//
// WITHOUT THIS THE CONTROL IS A DROP. #410's "verified when" is explicit: an
// unapproved act must be visibly PENDING rather than silently dropped. An
// approver has no other way to learn a proposal id — the propose reply goes to
// the PROPOSER — so this is the only surface a signature can be given from, the
// same relationship GET /v1/orders/pending-approvals has with the order-release
// route.
func (s *Server) handlePendingChanges(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := s.principal(w, r)
	if !ok {
		return
	}
	scope := store.TenantScope(tenant)
	now := s.now()

	pending, err := s.proposals.Pending(r.Context(), scope, now)
	if err != nil {
		s.logger.Error("compliance: cannot list pending mandate changes", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proposal store unavailable"})
		return
	}
	// A PROPOSAL NOBODY SIGNED IS LISTED, NOT ERASED (#563, learned from act one
	// rather than repeated here). Without it the proposer cannot tell "I never
	// proposed it", "somebody is still considering it" and "it died unsigned"
	// apart from one absence — and for a mandate the third case means the
	// portfolio is still governed by the old constraint, which is exactly the
	// thing somebody needs to know.
	lapsed, err := s.proposals.Lapsed(r.Context(), scope, now)
	if err != nil {
		s.logger.Error("compliance: cannot list lapsed mandate changes", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proposal store unavailable"})
		return
	}

	out := make([]map[string]any, 0, len(pending)+len(lapsed))
	// THE SPELLING IS dualcontrol'S, NOT THIS FILE'S (#558). Two queues already
	// render this concept and they diverged on casing the day they both existed;
	// a third inventing a third spelling is the whole reason the vocabulary lives
	// beside the rule. There is deliberately no "refused" here — a refusal on this
	// surface goes back on the same request.
	for _, p := range pending {
		out = append(out, proposalJSON(p, dualcontrol.StatePending))
	}
	for _, p := range lapsed {
		out = append(out, proposalJSON(p, dualcontrol.StateLapsed))
	}
	writeJSON(w, http.StatusOK, out)
}

// proposalJSON renders one proposal for the queue.
//
// IT CARRIES THE DIGEST AND NOT THE MANDATE. An approver needs to see WHAT they
// are signing, and the mandate is fetched by proposal id rather than splashed
// across a list — but the digest is what binds their signature, so it is the one
// field a client can compare against what it displays.
//
// STATE IS ALWAYS PRESENT, INCLUDING ON PENDING ENTRIES. Adding it only to lapsed
// ones would make a client that ignores unknown keys read a lapsed proposal as
// work waiting for them.
func proposalJSON(p store.MandateProposal, state string) map[string]any {
	return map[string]any{
		"proposal_id":  p.ID,
		"act":          string(p.Act),
		"portfolio_id": p.Mandate.GetPortfolioId(),
		"mandate_id":   p.Mandate.GetMandateId(),
		"version":      p.Mandate.GetVersion(),
		"rule_count":   len(p.Mandate.GetRules()),
		"proposer":     p.Proposer,
		"reason":       p.Reason,
		"digest":       p.Digest,
		"created_at":   p.CreatedAt,
		"expires_at":   p.ExpiresAt,
		"state":        state,
	}
}

// principal returns the caller's authenticated subject and tenant, or writes the
// refusal.
//
// ONE IMPLEMENTATION, SHARED BY ALL THREE ROUTES. Dual control over a forgeable
// identity is theatre: one person could propose as alice and approve as bob
// without ever holding a second credential, and the trail would show four eyes.
// That defect was real on the override surface and was fixed in #444, which is
// why this is a function rather than a paragraph repeated three times.
//
// THE HEADERS ARE TRUSTED BECAUSE OF THE NETWORK POLICY, not because they are
// headers. The api-gateway is this listener's only permitted caller — see the
// package doc on why these routes are not on the scraped port.
// A CALLER WITH NO TENANT IS REFUSED HERE TOO, AND BY THE SAME LINE.
// auth.PrincipalFromHeaders requires BOTH the subject and the tenant, which is
// the property this surface needs: neither gateway authenticator demands the
// tenant claim, so a perfectly valid token can carry none — and the tenant is
// what scopes every read and every write below. A proposal filed under an empty
// key lists on no queue and is approvable by nobody, which is a silent drop
// rather than a refusal.
//
// SO THERE IS NO SECOND CHECK BELOW, deliberately. A local p.Tenant == "" branch
// would be unreachable and would read as a control somebody could later "tidy
// up" the real one against; and reading the headers here directly to tell 401
// from 403 apart would be a second implementation of the trusted-header seam
// #258 consolidated into pkg/auth. The status is 401 rather than the 403 the
// gateway's order path answers, because from here the two cases are genuinely
// indistinguishable — pkg/auth is the one place that could tell them apart.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (subject, tenant string, ok bool) {
	p, found := auth.PrincipalFromHeaders(r.Header)
	if !found {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "no authenticated principal carrying both a subject and a tenant — this surface " +
				"is reachable only through the api-gateway, which is what makes the two names on a " +
				"mandate change real",
		})
		return "", "", false
	}
	return p.Subject, p.Tenant, true
}

// notFoundBody is the SINGLE body returned for "no such proposal", "not yours"
// and "already decided". One constant, so the three cannot drift apart into an
// oracle by a later edit to any branch.
const notFoundBody = "no such pending mandate change"

// alreadyDecidedBody is the losing side of a race. It is distinct from
// notFoundBody on purpose: the caller reached a proposal they were entitled to
// and lost it to a real second decision, which is a retryable-by-nobody outcome
// they should be told about rather than a permission answer.
const alreadyDecidedBody = "this proposal has already been decided"

// mintProposalID returns an unguessable proposal id.
//
// RANDOM, NOT DERIVED FROM THE MANDATE. cmd/kanz-mandate builds its id from the
// config key and the version, which is fine when the proposal travels in a file
// the approver was handed. On a shared surface a predictable id lets a caller
// approve a proposal they were never shown: the tenant scope and the portfolio
// match both still hold, so not knowing WHICH one is the last thing between them
// and a colleague's pending decision.
func mintProposalID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
