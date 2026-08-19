// Package authz is the gateway's capability model (SEC-M2).
//
// The gateway is the platform's sole identity authority, and it checked ONE role across all
// of /v1: `GET /v1/portfolios/{id}/exposure` and `POST /v1/orders` were guarded identically.
// The token handed to an analyst to look at the fund's exposure would submit an order to a
// live exchange.
//
// SEC-M1 closed the door to strangers. This is about what the people INSIDE may do.
//
// The model is deliberately small — one policy map, one mux, and a capability set that has
// to argue its way in one at a time. A capability system nobody can hold in their head gets
// bypassed by the first engineer in a hurry, so every constant below carries the case for why
// it is a DIFFERENT authority in BOTH directions from every other one.
package authz

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

const (
	// decider identifies this gateway as the deciding authority in the DecisionLog
	// ("{type}:{id}" per the schema). AUDIT-01 selects authz records by it.
	decider = "authz:api-gateway"
	// resourceRoute is the resource type for a capability check: the thing being
	// authorized is a ROUTE, not a portfolio or an order.
	resourceRoute = "route"
)

// Capability is what a route DOES, not what it is called. The HTTP verb is not the authority
// on effect: `POST /v1/portfolios/{id}/scenario` computes a what-if and moves no capital,
// while `POST /v1/orders` reaches a live exchange.
type Capability string

const (
	// Read: query the fund's state. Exposure, positions, orders, executions, prices.
	Read Capability = "read"
	// Trade: MOVE CAPITAL. Submit or cancel an order at a live exchange.
	Trade Capability = "trade"
	// Operate: RUN THE PLATFORM. Provision, cordon, drain and relabel nodes; write the
	// exchange API credentials the venue adapters sign with (OPS-M2b).
	//
	// A THIRD capability, and only a third: the package comment above is right that a
	// model nobody can hold in their head gets routed around, so this is not the start of
	// a taxonomy. It exists because operating the estate is a DIFFERENT authority from
	// reading it or trading on it, in both directions. An operator who can drain a node
	// has no business submitting orders; a trader who moves capital all day has no
	// business rotating the credentials those orders are signed with.
	//
	// It is deliberately NOT implied by Trade. Writing a venue key is not "more trading" —
	// a wrong key posts fills to another fund's ledger while the exchange debits the right
	// account, which no amount of trade authority is a licence to do.
	Operate Capability = "operate"
	// Fund: MOVE THE FUND'S OWN CAPITAL. Post a subscription, a redemption or a fee to the
	// book of record; move cash into or out of an exchange account (#415).
	//
	// A FOURTH, AND THE COMMENT ON Operate IS RIGHT THAT THIS MUST NOT BECOME A TAXONOMY.
	// It earns its place on the same test that one did: is this a DIFFERENT authority, in
	// BOTH directions, from every capability already here?
	//
	// It is, and the separation is the oldest one in fund operations. THE PERSON WHO CAN
	// MOVE MONEY IS NEVER THE PERSON WHO TRADES IT. A trader who moves capital all day has
	// no business deciding how much capital the fund holds — that is the investor's money
	// arriving and leaving. A funder has no business submitting an order. Collapsing the
	// two gives one credential the power to both bring cash in and spend it, which is the
	// single control every auditor of a fund asks about first.
	//
	// NOT Trade, AND NOT IMPLIED BY IT. Funding moves no market risk: it reaches no
	// exchange order book and takes no position. Reusing Trade would grant every strategy
	// operator the authority to book a redemption against the IBOR.
	//
	// NOT Operate EITHER. Operating the estate is draining nodes and rotating credentials;
	// it touches no fund capital, and an SRE holding it should not be able to move the
	// fund's cash.
	//
	// NOT Read, obviously: reading the ledger is Read, and this writes it.
	Fund Capability = "fund"
	// Approve: GIVE THE SECOND SIGNATURE. Sign off an act another person proposed — today a
	// pricing/compliance override (#410 act one), and by design the order and mandate
	// approvals that follow (#539).
	//
	// A FIFTH, AND THE BAR SET ON Operate AND Fund APPLIES UNCHANGED: is this a DIFFERENT
	// authority, in BOTH directions, from every capability already here? It is, and the
	// argument is stronger than for any of the four, because separation is not a
	// consequence of this capability — it is the ENTIRE CONTENT of it. A second signature
	// from someone who already held the authority to act alone is not a control; it is the
	// same authority spelled twice, and it reads as four-eyes in exactly the record an
	// auditor would check.
	//
	// NOT Trade, AND THIS IS THE ONE THAT MATTERS. Collapsing Approve into Trade hands
	// every trader the second signature on every other trader's proposal. The workflow
	// still refuses SELF-approval — datamaster compares authenticated subjects — so the
	// trail would show two distinct people and the control would be satisfied by any two
	// traders on the same desk. That is the failure mode maker-checker exists to prevent,
	// arriving through the capability layer rather than the identity one.
	//
	// NOT Fund, AND NOT Operate. Both move something real — the fund's cash, the estate's
	// nodes and venue credentials. Approve moves nothing on its own: it releases an act
	// somebody else already composed and bound to a digest. Granting it to a funder or an
	// SRE because they are "senior" is how it would drift into a seniority badge, which is
	// the one thing it must never be.
	//
	// NOT Read, obviously: reading what is pending is a report, and this decides it.
	//
	// WHY A CAPABILITY AND NOT JUST internal/dualcontrol. dualcontrol answers "is this
	// approval valid" — the approver differs from the proposer, the signature covers the
	// payload, the proposal has not expired. It cannot answer "may this person be an
	// approver at all", because it never sees a role. That question is authorization, it
	// belongs at the gateway with the other four, and without it every authenticated
	// subject in the tenant is a candidate signatory.
	Approve Capability = "approve"
	// Mandate: CHANGE THE CONSTRAINT ITSELF. Propose or sign a change to the mandate
	// a portfolio is governed by — what it may hold, in what concentration, at what
	// leverage (#562, #410 act two).
	//
	// A SIXTH, AND IT IS THE HARDEST ONE TO JUSTIFY, so the argument is written out
	// rather than assumed. The bar Operate, Fund and Approve each cleared is: is this
	// a DIFFERENT authority, in BOTH directions, from every capability already here?
	//
	// THE ONE IT HAS TO BE DIFFERENT FROM IS Approve, and #539's own text predicted
	// this would be "a third route on the same capability". That is the reading this
	// constant rejects, on the escalation #562 describes:
	//
	//	relax the constraint that would have refused an order, then clear the order
	//	it would have refused — with both acts individually looking correct in the
	//	trail.
	//
	// Under ONE capability the second signature on the mandate change and the second
	// signature on the held order come from the SAME POOL of people, so one signatory
	// can give both: sign away the limit, then sign the trade the limit existed to
	// stop. Each act still shows two names, each is still refused as a self-approval,
	// and the chain is invisible because nothing compares the two records. Separating
	// them means the escalation needs a signatory from each pool — the collusion set
	// is strictly wider, and that is the entire content of a maker-checker control.
	//
	// It is also the older distinction of the two. Approving an override or an order
	// is TRANSACTION authority: a judgement about one act, against the policy in
	// force. Changing a mandate is POLICY authority: a judgement about what the fund
	// may do at all, which is an investment-committee decision and not a desk one. A
	// firm that lets its order approvers rewrite the mandate has no mandate.
	//
	// NOT Trade, obviously, and for the sharpest version of the reason: a trader who
	// could change the mandate would not need to break the pre-trade gate, only to
	// widen it.
	//
	// NOT Operate AND NOT Fund. Both move something real — nodes, credentials, the
	// fund's cash — and neither has any business deciding what the fund may hold.
	// NOT Read: reading the mandate in force is a report; this rewrites it.
	//
	// THE COST IS ONE MORE ROLE A DEPLOYMENT MUST NAME, and it is paid the way every
	// optional capability here is: API_GATEWAY_MANDATE_ROLE unset leaves the three
	// routes UNREGISTERED, so the deployment answers 404 — "there is no mandate
	// surface here", which is true — rather than the 403 that #535 found reads as a
	// working control while being a total outage of the capability.
	Mandate Capability = "mandate"
)

// Grants is the role → capabilities policy: what a principal's roles entitle them to do.
//
// It is DENY-BY-DEFAULT and there is no fallback. A role nobody has mapped carries no
// capability — it does not decay into "well, it authenticated, so let it read". The baseline
// role that admits a caller to the gateway at all (SEC-M1's API_GATEWAY_REQUIRED_ROLE) is
// just another key here, and it must never be mapped to Trade: every authenticated caller
// carries it, so that one line of config would undo this whole model.
type Grants map[string][]Capability

// Allows reports whether any of the principal's roles carries cap.
func (g Grants) Allows(roles []string, cap Capability) bool {
	_, ok := g.grantingRole(roles, cap)
	return ok
}

// grantingRole names the role that carries cap, which is the audit-facing half of
// the same question Allows answers. It is one lookup rather than two so a future
// change cannot make the recorded reason disagree with the enforced decision —
// "allowed, because role X" and "allowed" must never be computed separately.
func (g Grants) grantingRole(roles []string, cap Capability) (string, bool) {
	for _, role := range roles {
		for _, c := range g[role] {
			if c == cap {
				return role, true
			}
		}
	}
	return "", false
}

// Route is one registered endpoint and the capability it demands. Exposed so the arch test
// can assert the property over WHATEVER is registered, rather than over a list someone has
// to remember to extend.
type Route struct {
	Pattern    string
	Capability Capability
}

// Mux is an http.ServeMux whose every route DECLARES the capability required to reach it.
//
// This is the "by construction" in SEC-M2: Handle takes the capability as a required
// parameter, so a route cannot be registered without one — not because a reviewer would
// catch it, but because the code would not compile. Enforcement is wrapped around the
// handler at registration, so it is not a middleware that a future composition root can
// leave out of a chain.
type Mux struct {
	mux      *http.ServeMux
	grants   Grants
	routes   []Route
	recorder auth.DecisionRecorder
}

// NewMux returns a Mux enforcing grants and recording every decision it makes.
//
// A nil Grants allows NOTHING — which is what makes it safe to construct one in a test,
// and fatal to construct one in production by mistake.
//
// THE RECORDER IS A REQUIRED PARAMETER FOR THE SAME REASON THE CAPABILITY IS (#352). This
// package's stance is that enforcement belongs at registration, "not because a reviewer
// would catch it, but because the code would not compile". An authorization decision that
// nobody records is exactly the defect that stance exists to prevent, one level up: the
// gateway is the platform's sole identity authority, and until now it recorded its allows
// and denies NOWHERE — not even to a log. A functional option would have made forgetting it
// the default.
//
// A nil recorder means "record nothing" and is for tests only. The composition root is
// guarded by test/arch/gateway_decision_recorder_test.go, which fails if main.go passes one.
func NewMux(grants Grants, recorder auth.DecisionRecorder) *Mux {
	return &Mux{mux: http.NewServeMux(), grants: grants, recorder: recorder}
}

// Handle registers pattern, reachable only by a principal whose roles carry cap.
func (m *Mux) Handle(cap Capability, pattern string, h http.HandlerFunc) {
	m.routes = append(m.routes, Route{Pattern: pattern, Capability: cap})
	m.mux.HandleFunc(pattern, m.require(cap, pattern, h))
}

// Routes returns every registered route and its capability, in registration order.
func (m *Mux) Routes() []Route { return m.routes }

// ServeHTTP makes the Mux the gateway's /v1 handler.
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

// require is the enforcement point, and therefore the recording point (AUTH-01d).
//
// BOTH VERDICTS ARE RECORDED, and that is not a preference. pkg/auth/audit.go states the
// contract — a DecisionLog "for every allow AND deny" — and services/audit takes the same
// line for the same reason ("audit completeness over economy", DefaultSubjects = [">"]). A
// trail holding only refusals cannot answer who DID read the fund's positions, which is the
// question an investigation actually asks.
//
// The volume that raises is real and is handled where it belongs rather than by pre-emptive
// narrowing: BusRecorder drains off a bounded queue and SHEDS on overflow, counting every
// drop. So load degrades to a counted, alertable loss instead of backpressure on the request
// path. If that counter ever moves in production, the answer is a deliberate, documented
// narrowing — not a guess made today against traffic nobody has measured.
func (m *Mux) require(cap Capability, pattern string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := middleware.PrincipalFromContext(r.Context())
		if p == nil {
			// This router runs INSIDE middleware.Auth, so no principal means the chain was
			// composed wrong. REFUSE. A capability check that treats "nobody" as "allowed"
			// is worse than no check at all, because it looks like one.
			//
			// Recorded too: this is a misconfiguration that presents as a 403 the caller
			// cannot distinguish from an ordinary refusal, so the audit trail is the only
			// place it becomes visible.
			m.record(r, nil, cap, pattern, auth.Decision{
				Reason: "no principal on the request context — the middleware chain is composed wrong",
			})
			forbidden(w)
			return
		}
		role, allowed := m.grants.grantingRole(p.Roles, cap)
		m.record(r, p, cap, pattern, decisionFor(cap, role, allowed, p.Roles))
		if !allowed {
			forbidden(w)
			return
		}
		next(w, r)
	}
}

// decisionFor renders the audit-facing reason. It NAMES THE GRANTING ROLE on an allow,
// because "allowed" alone cannot be reconstructed later: the grants map changes, and the
// question a year from now is which role carried the capability at the time.
func decisionFor(cap Capability, role string, allowed bool, roles []string) auth.Decision {
	if allowed {
		return auth.Decision{Allow: true, Reason: fmt.Sprintf("role %q carries %s", role, cap)}
	}
	return auth.Decision{Reason: fmt.Sprintf("none of the caller's roles %v carries %s", roles, cap)}
}

// record maps the capability decision onto a DecisionLog and hands it to the recorder.
//
// It uses auth.BuildDecisionLog rather than assembling attributes here, so the gateway's
// capability model and the rest of the estate's authorization records land in the SAME
// shape — AUDIT-01 selects on those attribute keys, and a second mapping would be a second
// answer to what "decision" and "principal.subject" mean.
//
// The resource is the ROUTE PATTERN, not the concrete path: it is what the capability was
// actually checked against, and it does not carry ids from the URL into the audit trail.
func (m *Mux) record(r *http.Request, p *middleware.Principal, cap Capability, pattern string, d auth.Decision) {
	if m.recorder == nil {
		return
	}
	var principal *auth.Principal
	tenant := ""
	if p != nil {
		principal = &auth.Principal{Subject: p.Subject, Tenant: p.Tenant, Roles: p.Roles, Portfolios: p.Portfolios}
		tenant = p.Tenant
	}
	entry := auth.BuildDecisionLog(decider, auth.Request{
		Principal: principal,
		Action:    auth.Action(cap),
		Resource:  auth.Resource{Type: resourceRoute, ID: pattern, Tenant: tenant},
	}, d)
	// The error is discarded because DecisionRecorder implementations are non-blocking and
	// best-effort by contract (they report their own losses via a counter). Failing the
	// caller's request because an audit sink is unhappy would make the gateway less
	// available than the thing it is auditing.
	_ = m.recorder.Record(r.Context(), entry)
}

// forbidden says the caller is known and not permitted — deliberately WITHOUT naming the
// capability they lack. 403, not 401: their credentials are fine; their authority is not.
func forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "insufficient capability"})
}
