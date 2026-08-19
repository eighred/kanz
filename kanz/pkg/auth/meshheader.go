package auth

import (
	"encoding/json"
	"net/http"
	"strings"
)

// The mesh identity headers — the SVCWIRE-01c trusted-header contract, and the
// entire authorization story for every service behind the gateway.
//
// The api-gateway is the sole identity authority: it verifies the token and
// injects these on every forwarded request. An upstream may trust them ONLY
// because a NetworkPolicy makes the gateway its only reachable caller (#232
// tracks where that is not yet enforced). EXPOSE ONE OF THOSE SERVICES DIRECTLY
// AND ANY CALLER NAMES ANY TENANT AND IS SERVED THAT TENANT'S BOOK.
//
// WHY HERE AND NOT WITH THE INJECTOR. The injector is
// services/api-gateway/internal/proxy, and Go's internal rule makes it
// unimportable by the services that must read what it writes. So five packages
// each grew their own copy of the constant AND their own enforcement (#258): a
// change to how this header is validated — a canonicalisation rule, a spoofing
// check, the day SPIFFE mTLS lands on those hops — had to be made five times, by
// someone who knew all five existed. pkg/auth is where Principal already lives
// and is already imported by the gateway, copilot, lineage and audit.
//
// ADD NOTHING HERE WITHOUT AN ACCESSOR BESIDE IT. The constant travelling alone
// is what let a service adopt the name and reinvent the check; test/arch's
// TestPrincipalHeadersLiveOnlyInPkgAuth is what keeps that from happening again.
const (
	HeaderPrincipalSubject = "X-Kanz-Principal-Subject"
	HeaderPrincipalTenant  = "X-Kanz-Principal-Tenant"
	HeaderPrincipalRoles   = "X-Kanz-Principal-Roles" // comma-separated
)

// SetPrincipalHeaders writes the verified principal onto an OUTBOUND request —
// the one act that mints trusted identity on this mesh. Two callers today: the
// api-gateway, forwarding the principal it authenticated, and copilot,
// forwarding the end user's principal to lineage so PII governance is applied to
// the human and not to copilot's SVID.
//
// Roles are omitted when empty rather than sent as "", so an upstream splitting
// the header never sees a single empty role.
//
// Takes the three fields rather than a *Principal because the gateway's edge
// principal (middleware.Principal, which also carries the portfolio allow-list)
// is a different type from auth.Principal and both must inject identically.
func SetPrincipalHeaders(h http.Header, subject, tenant string, roles []string) {
	h.Set(HeaderPrincipalSubject, subject)
	h.Set(HeaderPrincipalTenant, tenant)
	if len(roles) > 0 {
		h.Set(HeaderPrincipalRoles, strings.Join(roles, ","))
	}
}

// PrincipalFromHeaders is the READING half of SetPrincipalHeaders — the seam
// that turns the gateway's injected headers back into the auth.Principal every
// authorization decision keys on (#268).
//
// It existed nowhere for as long as the writing half did. auth.WithPrincipal was
// called in exactly one non-test place, INSIDE the gateway process, so a proxied
// request reached lineage's governance check and copilot's tool authorizer with a
// nil principal — no error, no missing import, no compile failure. Roles were
// forwarded on the wire on every request and read by nobody.
//
// ok IS FALSE UNLESS BOTH SUBJECT AND TENANT ARE PRESENT, and a false ok yields a
// nil principal rather than a zero-value one. A principal with no tenant is not a
// weaker principal, it is a principal PolicyAuthorizer denies outright ("principal
// has no tenant") while governance.CheckAccess would still serve it every
// non-PII dataset — the two are not the same answer, and handing a caller a
// non-nil Principal{} is how that divergence gets exercised. Prefer
// RequirePrincipal, which cannot be installed without the refusal.
//
// Roles round-trip through SetPrincipalHeaders exactly: joined on ",", split on
// ",", empties dropped, and nil in ⇒ nil out (the writer omits the header rather
// than sending ""). A ROLE NAME CONTAINING A COMMA DOES NOT SURVIVE THIS
// ENCODING — it arrives as two roles, and if either half names a real policy role
// that is an escalation. No IdP-issued role on this platform contains one; the
// day one might, the encoding has to change, not this reader.
//
// WHAT IT CANNOT RECONSTRUCT. The wire carries subject, tenant and roles only, so
// both Claims and Portfolios are always empty — including the ABAC portfolio
// allow-list PolicyAuthorizer reads through PortfolioInScope and copilot's tool
// gate relies on (services/copilot/internal/tools/tools.go's Registry.authorize).
// On a READ path an absent allow-list means "every portfolio within the caller's
// own tenant", so copilot's portfolio sub-scope is inert by construction, and
// portfolio.go argues why making it deny-on-empty would refuse every governed
// tool call rather than tighten anything.
//
// Putting a fourth header on the mesh would forward an empty list and look like a
// fix. #225 repaired the gateway's OIDC bridge, which used to drop the claim
// before it ever reached the wire, so the list is now real INSIDE the gateway —
// and the OMS reads it off the command envelope, not off these headers. Nothing
// downstream of this seam has a use for it that an empty header would satisfy.
// Tenant isolation and RBAC ARE enforced upstream; portfolio sub-scope within a
// tenant is not.
// IssuedAt IS ALSO ABSENT ON THIS SEAM, and there is no fifth header for it
// either. It exists so the GATEWAY can date a token against a revocation mark
// (#532); an upstream reads no feed and makes no such decision, and the gateway
// has already refused a revoked caller before these headers are written. It
// arrives here as the zero value, which the revocation check reads as
// "undatable" — so if this ever does move upstream, the absence fails closed.
func PrincipalFromHeaders(h http.Header) (*Principal, bool) {
	subject := h.Get(HeaderPrincipalSubject)
	tenant := h.Get(HeaderPrincipalTenant)
	if subject == "" || tenant == "" {
		return nil, false
	}
	return &Principal{Subject: subject, Tenant: tenant, Roles: splitRoles(h.Get(HeaderPrincipalRoles))}, true
}

// splitRoles is the inverse of the strings.Join in SetPrincipalHeaders. Returns
// nil rather than []string{""} for an absent or blank header, so HasRole and the
// policy lookup never see an empty role name.
func splitRoles(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	roles := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			roles = append(roles, p)
		}
	}
	if len(roles) == 0 {
		return nil
	}
	return roles
}

// RequirePrincipal is the UPSTREAM half of the trusted-header seam: middleware
// that reconstructs the gateway-authenticated principal onto the request context
// and REFUSES the request when there is none (#268).
//
// WHY 401 AND NOT "attach nothing, let the handler decide". Letting it through
// is what the platform did, and the two services that read
// auth.PrincipalFromContext both proceed on nil in the widening direction:
// lineage's governance.CheckAccess short-circuits every non-PII dataset to
// "public dataset — allow" before it ever looks at the principal, and
// lineage's catalog listing never asks for one at all. So an unauthenticated
// caller and an authorized steward got the same 200, which is exactly the state
// CLAUDE.md forbids — "nothing configured" and "checked, and fine" looking the
// same. It also failed in the OTHER direction at the same time: a real steward
// proxied through the gateway was denied their own PII lineage, because the
// principal that would have granted it never arrived.
//
// DEFAULT-DENY WITH FOUR NAMED EXEMPTIONS. Every path is refused except the
// infrastructure probes below, which are called by the kubelet and by Prometheus
// and carry no principal by construction. A route registered on a wrapped mux is
// therefore protected the day it is added, not the day someone remembers — which
// is the whole reason this attaches to the mux rather than to each handler.
//
// WHAT BREAKS IF THIS IS WRONG. Any legitimate non-gateway HTTP caller of a
// wrapped service now gets a 401 naming the cause. There are none today: the
// gateway proxies copilot's /v1/ask, copilot forwards the end user's principal to
// lineage, and nothing else calls either over HTTP. The refusal is loud, so a
// caller added later fails visibly on its first request rather than silently
// authorizing as nobody.
//
// THIS IS NOT AUTHENTICATION. It trusts the headers, and that trust holds only
// while a NetworkPolicy makes the gateway the sole reachable caller (#232, still
// unenforced in three namespaces). Direct exposure of a wrapped service lets any
// caller name any subject, tenant and role set — the header block above says the
// same thing and it is no less true on the reading side.
func RequirePrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unauthenticatedPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		p, ok := PrincipalFromHeaders(r.Header)
		if !ok {
			writeErrorJSON(w, http.StatusUnauthorized,
				"missing authenticated principal: this surface is reachable only through the "+
					"api-gateway, which injects the caller's subject, tenant and roles")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// unauthenticatedPaths are the only routes RequirePrincipal lets through without
// a principal: the liveness/readiness probes the kubelet calls and the metrics
// endpoint Prometheus scrapes, neither of which goes through the gateway. Exact
// paths, not prefixes — "/metrics" is exempt, "/metrics/../v1/anything" is not.
//
// Adding to this map exempts a route from the platform's only upstream identity
// check. The three probe names are the ones every service in this module
// registers; /startupz is deliberately absent because nothing registers it.
var unauthenticatedPaths = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
	"/livez":   true,
	"/metrics": true,
}

// CallerTenant returns the tenant the gateway authenticated for this inbound
// request, or "" when the request carries no principal.
//
// "" IS NOT A TENANT. It means nobody established who is asking — either the
// request did not come through the gateway, or it did and was not authenticated.
// Prefer RequireCallerTenant or RequireCallerTenantIs, which cannot be used
// without handling that case; reach for this only where the refusal is written
// somewhere else.
func CallerTenant(r *http.Request) string {
	return r.Header.Get(HeaderPrincipalTenant)
}

// RequireCallerTenant is the MULTI-TENANT SCOPING policy: the service holds more
// than one tenant's data and uses the caller's tenant as the scope of the read.
// It returns the tenant, or writes 401 and returns false when the request is
// unscoped.
//
// Used by audit (audit_log is deliberately NOT RLS'd — it is the cross-tenant
// compliance record, so this handler is the boundary and the database will not
// save you) and by tv-sync's Broker-API.
//
// An empty scope must never reach the store: for audit, an empty tenant filter
// does not mean "no records", it means EVERY tenant's records.
//
// 401 rather than 404 here, and that is not an inconsistency with
// RequireCallerTenantIs below — see the note on that function.
func RequireCallerTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenant := CallerTenant(r)
	if tenant == "" {
		writeErrorJSON(w, http.StatusUnauthorized,
			"missing tenant scope: this surface is reachable only through the api-gateway, "+
				"which injects the authenticated principal")
		return "", false
	}
	return tenant, true
}

// RequireCallerTenantIs is the SINGLE-TENANT INSTANCE policy: under the #97
// ruling the instance serves exactly one tenant (its RLS pool is pinned to it),
// so a caller from any other tenant has no business here whatever the database
// holds. Returns false and writes the refusal when the caller is not that
// tenant.
//
// Used by wealth and datamaster, both of which had NO tenant check at all before
// #222 — every route went from r.PathValue straight to the store, datamaster's
// exception-override WRITE included.
//
// Fails CLOSED on an absent header and on an unset instanceTenant: both mean
// nobody established who is asking, and neither is permission.
//
// 404, not 403, and notFoundMessage MUST be the same body a genuine miss
// returns on that route. Distinguishing "not yours" from "not there" turns id
// enumeration into a cross-tenant directory.
//
// WHY THIS DIFFERS FROM RequireCallerTenant AND SHOULD. The no-oracle 404 is
// only available to a surface that answers per-id from a single tenant's store.
// A scoping surface has already filtered by the caller's own tenant before any
// id is looked up, so its 404 leaks nothing — and its 401 on an unscoped request
// leaks nothing either, because no id has been named yet. Two policies, one
// header, one accessor: pick by which shape the service is, not by preference.
func RequireCallerTenantIs(w http.ResponseWriter, r *http.Request, instanceTenant, notFoundMessage string) bool {
	caller := CallerTenant(r)
	if caller == "" || instanceTenant == "" || caller != instanceTenant {
		writeErrorJSON(w, http.StatusNotFound, notFoundMessage)
		return false
	}
	return true
}

// writeErrorJSON emits the {"error": …} body every one of these surfaces already
// wrote, byte for byte, so consolidating the check changed no response.
func writeErrorJSON(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
