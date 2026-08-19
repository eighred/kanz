package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

// MAKER-CHECKER ON THE PRICING OVERRIDE (#410).
//
// An institutional platform is expected to require two people for an act that
// moves capital or relaxes a control. This one required one, everywhere. #410
// names three acts — a pricing override, a mandate change, and an order above a
// notional — and this is the first, chosen because it is a single endpoint whose
// actor is already authenticated (#444) and whose trail is already append-only.
// The rule itself lives in internal/dualcontrol so the other two extend it
// rather than retyping it.
//
// # It ships UNARMED, and that is the deliberate half
//
// DATAMASTER_REQUIRE_DUAL_CONTROL defaults to false, so an override still
// applies immediately today. Arming it by default would turn every existing
// override caller into a 202 that never completes, on a deploy — a valuation
// outage delivered by a security improvement. Same shape as OMS_REQUIRE_MANDATE,
// OMS_REQUIRE_VERIFIED_ACCOUNT and RISK_REQUIRE_VALIDATED_ANALYTICS: build the
// control, count the gap, arm it with the list in hand.
//
// WHAT MAKES THIS DIFFERENT FROM SIMPLY NOT SHIPPING IT is that the gap is now
// counted and the audit record now distinguishes the two states. Every override
// records whether a second person signed it, so "one person did this" and "two
// people did this" are different rows rather than the same row read against a
// config value that has since changed.

// DualControl is what the composition root supplies to arm — or merely to
// observe — maker-checker on this surface.
type DualControl struct {
	// Proposals holds pending proposals. REQUIRED WHEN Require IS TRUE: arming
	// the control without somewhere to put a proposal would refuse every
	// override and accept none, which is an outage wearing a control's name.
	Proposals store.ProposalStore
	// Require arms the refusal. False counts and warns; true makes a second
	// signature mandatory.
	Require bool
	// TTL is how long a pending proposal stays approvable; zero means
	// dualcontrol.DefaultTTL.
	TTL time.Duration
	// Registerer receives the posture metrics. Nil skips them, which is the test
	// default and never the production one.
	Registerer prometheus.Registerer
}

// WithDualControl configures maker-checker on the override path.
func WithDualControl(dc DualControl) Option {
	return func(s *Server) {
		s.proposals = dc.Proposals
		s.requireDualControl = dc.Require
		s.dualTTL = dc.TTL
		if s.dualTTL <= 0 {
			s.dualTTL = dualcontrol.DefaultTTL
		}
		if dc.Registerer != nil {
			s.overrideMetrics = newOverrideMetrics(dc.Registerer)
		}
	}
}

// overrideMetrics is the override posture: how many signatures each applied
// override carried, and what became of each proposal.
type overrideMetrics struct {
	signatures *prometheus.CounterVec
	proposals  *prometheus.CounterVec
}

func newOverrideMetrics(reg prometheus.Registerer) *overrideMetrics {
	signatures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_datamaster_overrides_total",
		Help: "Pricing-oversight overrides applied, by how many people signed them. " +
			"single_signed is the #410 gap: one person relaxed a control on their own " +
			"authority. It is not an error while DATAMASTER_REQUIRE_DUAL_CONTROL is off " +
			"— it is the count that says how much of this trail a second person never saw.",
	}, []string{"signatures"})
	proposals := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_datamaster_override_proposals_total",
		Help: "Dual-control proposals by outcome: proposed, approved, rejected, " +
			"refused_self_approval, refused_expired, refused_payload_changed, refused_race. " +
			"refused_self_approval is the one an auditor asks for — it is the control firing.",
	}, []string{"outcome"})
	reg.MustRegister(signatures, proposals)
	// EVERY SERIES EXISTS FROM THE FIRST SCRAPE, including the zeroes. A counter
	// that appears only once it fires makes "no self-approval has ever been
	// attempted" and "this build does not have the check" identical on a
	// dashboard — and the second is the one worth knowing.
	for _, l := range []string{"single_signed", "dual_signed"} {
		signatures.WithLabelValues(l)
	}
	for _, l := range []string{"proposed", "approved", "rejected", "refused_self_approval",
		"refused_expired", "refused_payload_changed", "refused_race"} {
		proposals.WithLabelValues(l)
	}
	return &overrideMetrics{signatures: signatures, proposals: proposals}
}

// The two counters are nil-safe because a Server built without a Registerer is
// the test default, and a metric that panics when unobserved would make the
// observability wiring load-bearing for the control itself.
func (s *Server) countSignatures(label string) {
	if s.overrideMetrics != nil {
		s.overrideMetrics.signatures.WithLabelValues(label).Inc()
	}
}

func (s *Server) countProposal(outcome string) {
	if s.overrideMetrics != nil {
		s.overrideMetrics.proposals.WithLabelValues(outcome).Inc()
	}
}

// dualControlArmed reports whether a second signature is required.
//
// IT REQUIRES A PROPOSAL STORE, NOT JUST THE FLAG. Arming without one would take
// every override, fail to record it as pending, and return a 202 the approver
// can never act on — an act that neither takes effect nor reports why, which is
// the failure mode #410's "verified when" names outright. The composition root
// refuses this combination at startup; this is the second line, because a
// constructor that can be called from a test is not a guarantee.
func (s *Server) dualControlArmed() bool { return s.requireDualControl && s.proposals != nil }

// proposeOverride records a pending override and answers 202.
func (s *Server) proposeOverride(w http.ResponseWriter, r *http.Request, exceptionID, proposer, reason string, price *big.Rat) {
	id, err := mintProposalID()
	if err != nil {
		s.logger.Error("cannot mint a proposal id", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot record the proposal"})
		return
	}
	base, err := dualcontrol.Propose(id, dualcontrol.ActPricingOverride, exceptionID, proposer,
		store.PayloadDigest(exceptionID, reason, price), s.now(), s.dualTTL)
	if err != nil {
		// Propose refuses only malformed input, and every field here has already
		// been validated by the caller — so this is a defect in THIS code, not in
		// the request, and must not be reported as the operator's fault.
		s.logger.Error("built an unapprovable proposal", "exception_id", exceptionID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot record the proposal"})
		return
	}
	prop := store.OverrideProposal{Proposal: base, Reason: reason, ChosenPrice: price}
	if err := s.proposals.Put(r.Context(), prop); err != nil {
		s.logger.Error("cannot store the override proposal", "exception_id", exceptionID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot record the proposal"})
		return
	}
	s.countProposal("proposed")
	// 202, NOT 200. The override has NOT been applied, and a 200 would tell every
	// existing client it had been — the exact silent-drop this control is meant
	// to make impossible.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":       "PENDING_APPROVAL",
		"proposal_id":  prop.ID,
		"exception_id": exceptionID,
		"proposer":     prop.Proposer,
		"expires_at":   prop.ExpiresAt,
		"message": "recorded, NOT applied: this override takes effect when a different authenticated " +
			"person approves it at POST /v1/exceptions/{id}/override/approve",
	})
}

// handleApproveOverride is the second signature.
func (s *Server) handleApproveOverride(w http.ResponseWriter, r *http.Request) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	if s.proposals == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": notFoundBody})
		return
	}
	exceptionID := r.PathValue("id")
	approver, ok := s.authenticatedSubject(w, r)
	if !ok {
		return
	}
	var body struct {
		ProposalID string `json:"proposal_id"`
		// Decision is "approve" or "reject". THERE IS NO DEFAULT: an empty
		// decision is refused rather than assumed, because both possible
		// assumptions are wrong — defaulting to approve applies an override
		// nobody consented to, and defaulting to reject discards a decision
		// silently.
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if body.Decision != "approve" && body.Decision != "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `decision must be "approve" or "reject"`,
		})
		return
	}

	prop, found, err := s.proposals.Get(r.Context(), body.ProposalID)
	if err != nil {
		s.logger.Error("cannot read the override proposal", "proposal_id", body.ProposalID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proposal store unavailable"})
		return
	}
	// A proposal for a DIFFERENT exception is not found here, not applied there.
	// Without this the exception id in the path is decorative, and an approver
	// could be shown one exception while signing for another.
	if !found || prop.Subject != exceptionID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": notFoundBody})
		return
	}

	if body.Decision == "reject" {
		// A rejection needs no second-person check: refusing an act is always
		// safe, including by the proposer withdrawing their own.
		claimed, err := s.proposals.Claim(r.Context(), prop.ID)
		if err != nil {
			s.logger.Error("cannot claim the override proposal", "proposal_id", prop.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proposal store unavailable"})
			return
		}
		if !claimed {
			s.countProposal("refused_race")
			writeJSON(w, http.StatusConflict, map[string]string{"error": "this proposal has already been decided"})
			return
		}
		s.countProposal("rejected")
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "REJECTED", "proposal_id": prop.ID, "rejected_by": approver,
		})
		return
	}

	// THE RULE. Checked BEFORE the claim, so a refused approval leaves the
	// proposal pending for someone who may legitimately approve it — consuming it
	// on a self-approval attempt would let one person destroy a colleague's
	// pending decision by trying to approve their own.
	digest := store.PayloadDigest(prop.Subject, prop.Reason, prop.ChosenPrice)
	if _, err := prop.Approve(approver, digest, s.now()); err != nil {
		s.refuseApproval(w, prop, approver, err)
		return
	}

	claimed, err := s.proposals.Claim(r.Context(), prop.ID)
	if err != nil {
		s.logger.Error("cannot claim the override proposal", "proposal_id", prop.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proposal store unavailable"})
		return
	}
	if !claimed {
		// Someone else decided it between the read and here. Refusing is the only
		// safe answer: applying would append a second override for one decision.
		s.countProposal("refused_race")
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this proposal has already been decided"})
		return
	}

	// BOTH NAMES GO IN TOGETHER, built by the proposal itself, so the proposer
	// and the approver cannot be transposed or one of them dropped on the way
	// into the append-only trail.
	if err := s.exceptions.Override(r.Context(), prop.Subject, prop.ApplyOverride(approver, s.now())); err != nil {
		// The proposal is already claimed and the override did not apply. Say so
		// loudly: the proposer must re-propose, and an operator needs to know why
		// a decision they made vanished.
		s.logger.Error("dual-signed override was approved but did not apply — the proposal is consumed "+
			"and must be re-proposed", "exception_id", prop.Subject, "proposer", prop.Proposer,
			"approver", approver, "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.countProposal("approved")
	s.countSignatures("dual_signed")

	ex, ok, err := s.exceptions.Get(r.Context(), prop.Subject)
	if err != nil || !ok {
		s.logger.Error("override applied but read-back failed", "exception_id", prop.Subject, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "exception store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, ex)
}

// refuseApproval maps a dualcontrol refusal onto a status, a count and a message
// that names the rule.
//
// THIS PATH IS ALREADY ANSWERED, AND #558 IS NOT ABOUT IT. That issue reported
// "exactly the same silence on refuseApproval" as the OMS order path has; read
// against this code the premise does not hold — an override is approved over
// HTTP, so the refusal goes back on the same request with a status the caller
// branches on. The OMS's approval arrives on the BUS, where the 202 was answered
// at publish time and there is nothing left to reply to, which is why it needed
// the refusal recorded on the proposal and shown on its pending queue and this
// does not. Adding a second, asynchronous channel here would be a message with no
// reader — the thing #563 declined to build one function down.
func (s *Server) refuseApproval(w http.ResponseWriter, prop store.OverrideProposal, approver string, err error) {
	switch {
	case errors.Is(err, dualcontrol.ErrSelfApproval):
		// 403, not 400. The request is well-formed; the person is not permitted.
		// THE PROPOSAL STAYS PENDING — see the call site.
		s.countProposal("refused_self_approval")
		s.logger.Warn("self-approval refused on a pricing override — one person attempted both signatures",
			"exception_id", prop.Subject, "proposer", prop.Proposer, "approver", approver)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "the approver must be a different person from the proposer; this override was proposed by " +
				prop.Proposer,
		})
	case errors.Is(err, dualcontrol.ErrExpired):
		s.countProposal("refused_expired")
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this proposal expired before it was approved and must be proposed again",
		})
	case errors.Is(err, dualcontrol.ErrPayloadChanged):
		s.countProposal("refused_payload_changed")
		s.logger.Error("an override proposal's payload no longer matches its digest — the stored proposal "+
			"changed after it was proposed", "exception_id", prop.Subject, "proposal_id", prop.ID)
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the proposed values no longer match what was signed for",
		})
	default:
		s.logger.Error("malformed override proposal reached approval", "proposal_id", prop.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the proposal is not well-formed"})
	}
}

// handlePendingOverrides lists overrides awaiting a second signature.
//
// WITHOUT THIS THE CONTROL IS A DROP. #410's "verified when" is explicit: an
// unapproved action must be visibly PENDING rather than silently dropped, because
// an act that neither takes effect nor reports why is the failure mode this
// platform refuses everywhere else. A proposer who closes their browser has no
// other way to find what they left waiting.
func (s *Server) handlePendingOverrides(w http.ResponseWriter, r *http.Request) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	if s.proposals == nil {
		// An empty list, not a 404: "dual control is not armed here" is a real
		// answer and there genuinely is nothing pending.
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	now := s.now()
	pending, err := s.proposals.Pending(r.Context(), now)
	if err != nil {
		s.logger.Error("cannot list pending override proposals", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proposal store unavailable"})
		return
	}
	// A PROPOSAL NOBODY SIGNED IS LISTED, NOT ERASED (#563).
	//
	// Until this, an override proposal that reached expires_at simply stopped
	// appearing, and the proposer had to tell "I never proposed it", "somebody is
	// still considering it" and "it died unsigned" apart from one absence. The
	// exception stayed listed as unresolved, so the STATE was recoverable and the
	// event was not: nothing anywhere recorded that a resolution had been
	// proposed and had lapsed.
	//
	// #547 solved the same problem for held orders with a terminal FACT, and that
	// answer does not transfer. It worked because ORDER_REJECTED already had a
	// reader — the trader watching for any rejection. This service publishes one
	// subject, data.exception.overridden, and nothing consumes it but the audit
	// projector, so an expiry FACT here would be a new subject, grant and message
	// with nobody on the other end. The surface the proposer already uses is the
	// reader that exists.
	lapsed, err := s.proposals.Lapsed(r.Context(), now)
	if err != nil {
		s.logger.Error("cannot list lapsed override proposals", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proposal store unavailable"})
		return
	}
	out := make([]map[string]any, 0, len(pending)+len(lapsed))
	for _, p := range pending {
		out = append(out, proposalJSON(p, "pending"))
	}
	for _, p := range lapsed {
		out = append(out, proposalJSON(p, "lapsed"))
	}
	writeJSON(w, http.StatusOK, out)
}

// proposalJSON renders one proposal with the state that says whether it is still
// actionable.
//
// STATE IS ALWAYS PRESENT, INCLUDING ON PENDING ENTRIES. Adding the field only
// to lapsed ones would make a client that ignores unknown keys — which is every
// client that predates this — read a lapsed proposal as work waiting for them.
func proposalJSON(p store.OverrideProposal, state string) map[string]any {
	return map[string]any{
		"proposal_id":  p.ID,
		"exception_id": p.Subject,
		"act":          string(p.Act),
		"proposer":     p.Proposer,
		"reason":       p.Reason,
		"chosen_price": p.ChosenPrice.RatString(),
		"created_at":   p.CreatedAt,
		"expires_at":   p.ExpiresAt,
		"state":        state,
	}
}

// authenticatedSubject returns the caller's identity or writes the 401.
//
// ONE IMPLEMENTATION, shared by the override and approve paths. Dual control over
// a forgeable identity is theatre: one person could propose as alice and approve
// as bob without ever holding a second credential, and the trail would show
// four-eyes. That defect was real on this very surface and was fixed in #444 —
// which is why the check is a function rather than a paragraph repeated twice.
func (s *Server) authenticatedSubject(w http.ResponseWriter, r *http.Request) (string, bool) {
	principal, ok := auth.PrincipalFromHeaders(r.Header)
	if !ok || principal.Subject == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "no authenticated principal — this surface is reachable only through the api-gateway, " +
				"which is what makes the actor on an override record real",
		})
		return "", false
	}
	return principal.Subject, true
}

// applySingleSigned is the unarmed path: the override takes effect on one
// person's authority, and the fact that it did is recorded and counted.
func (s *Server) applySingleSigned(w http.ResponseWriter, r *http.Request, exceptionID, actor, reason string, price *big.Rat) {
	o := pricing.Override{Actor: actor, Reason: reason, ChosenPrice: price, At: s.now()}
	if err := s.exceptions.Override(r.Context(), exceptionID, o); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.countSignatures("single_signed")
	ex, ok, err := s.exceptions.Get(r.Context(), exceptionID)
	if err != nil || !ok {
		s.logger.Error("override recorded but read-back failed", "exception_id", exceptionID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "exception store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, ex)
}

// mintProposalID returns an unguessable proposal id.
//
// RANDOM, NOT SEQUENTIAL OR DERIVED. A predictable id lets a caller approve a
// proposal they were never shown — the tenant scope and the exception-id match
// both still hold, so the only thing standing between them and a colleague's
// pending decision is not knowing which one it is.
func mintProposalID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
