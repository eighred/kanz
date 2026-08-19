package server

import (
	"net/http"

	"github.com/eighred/kanz/internal/revocation"
)

// revocationsHandler serves the per-subject revocation feed the api-gateway
// enforces (#532).
//
// # Why this is on the UNAUTHENTICATED half of this service
//
// For the same reason /jwks.json is: the gateway holds no credential to present
// here, and could not obtain one without a token — which is the thing it is
// asking this endpoint to help it judge. Requiring authentication would make the
// control depend on the control.
//
// What makes that acceptable is the same pair of properties that makes
// /jwks.json acceptable, and both must stay true:
//
//   - REACHABILITY IS THE BOUNDARY. identity has no Ingress; only the gateway and
//     the web-bff are admitted to :8087 by allow-ingress-to-identity
//     (infra/security/runtime/network-policies.yaml). This is not internet-facing
//     and must not become so.
//   - THE BODY NAMES NOBODY. Subjects are hashed (revocation.HashSubject), so the
//     feed answers "is THIS subject revoked" without publishing the roster of
//     everyone who has been. identity spends real effort — a decoy Argon2id
//     verify on every unknown subject — keeping /login from enumerating this
//     platform's traders and operators; serving their names here would hand out
//     for free what that decoy makes expensive.
//
// # Why it is registered unconditionally
//
// Unlike the provisioning routes, this is NOT gated on s.provisioning. A
// deployment with no provisioning surface cannot disable anybody, so its feed is
// empty — and an empty 200 is the truthful answer "checked, and nobody is
// revoked". A 404 would be read by the gateway as a feed it cannot fetch, which
// (correctly, and uselessly) keeps it out of service. "Nothing configured" and
// "checked, and fine" have to be distinguishable, and here they are: an empty
// list is the second, and no answer at all is the first.
//
// # Failure is an error, never an empty list
//
// A store failure answers 503 with no body to parse. Returning 200 with zero
// entries would tell the gateway that nobody on this platform has been revoked —
// the single most dangerous lie this endpoint could tell, and the one it would
// tell every 30 seconds without anybody noticing.
func (s *Server) revocationsHandler(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.Revocations(r.Context())
	if err != nil {
		s.logger.Error("identity: revocation feed unavailable — the gateway cannot learn which "+
			"accounts have been disabled, and fails closed once its cached feed passes its ceiling",
			"err", err)
		writeErr(w, http.StatusServiceUnavailable, "revocation feed unavailable")
		return
	}
	writeJSON(w, http.StatusOK, revocation.Feed{
		Kind:    revocation.FeedKind,
		AsOf:    s.now().UTC().Unix(),
		Entries: entries,
	})
}
