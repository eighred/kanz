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
