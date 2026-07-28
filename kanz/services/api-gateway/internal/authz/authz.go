// Package authz is the gateway's capability model (SEC-M2).
//
// The gateway is the platform's sole identity authority, and it checked ONE role across all
// of /v1: `GET /v1/portfolios/{id}/exposure` and `POST /v1/orders` were guarded identically.
// The token handed to an analyst to look at the fund's exposure would submit an order to a
// live exchange.
//
// SEC-M1 closed the door to strangers. This is about what the people INSIDE may do.
//
// The model is deliberately small — two capabilities, one policy map, one mux. A capability
// system nobody can hold in their head gets bypassed by the first engineer in a hurry.
package authz

import (
	"encoding/json"
	"net/http"

	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
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
	for _, role := range roles {
		for _, c := range g[role] {
			if c == cap {
				return true
			}
		}
	}
	return false
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
	mux    *http.ServeMux
	grants Grants
	routes []Route
}

// NewMux returns a Mux enforcing grants. A nil Grants allows NOTHING — which is what makes
// it safe to construct one in a test, and fatal to construct one in production by mistake.
func NewMux(grants Grants) *Mux {
	return &Mux{mux: http.NewServeMux(), grants: grants}
}

// Handle registers pattern, reachable only by a principal whose roles carry cap.
func (m *Mux) Handle(cap Capability, pattern string, h http.HandlerFunc) {
	m.routes = append(m.routes, Route{Pattern: pattern, Capability: cap})
	m.mux.HandleFunc(pattern, m.require(cap, h))
}

// Routes returns every registered route and its capability, in registration order.
func (m *Mux) Routes() []Route { return m.routes }

// ServeHTTP makes the Mux the gateway's /v1 handler.
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

// require is the enforcement point.
func (m *Mux) require(cap Capability, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := middleware.PrincipalFromContext(r.Context())
		if p == nil {
			// This router runs INSIDE middleware.Auth, so no principal means the chain was
			// composed wrong. REFUSE. A capability check that treats "nobody" as "allowed"
			// is worse than no check at all, because it looks like one.
			forbidden(w)
			return
		}
		if !m.grants.Allows(p.Roles, cap) {
			forbidden(w)
			return
		}
		next(w, r)
	}
}

// forbidden says the caller is known and not permitted — deliberately WITHOUT naming the
// capability they lack. 403, not 401: their credentials are fine; their authority is not.
func forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "insufficient capability"})
}
