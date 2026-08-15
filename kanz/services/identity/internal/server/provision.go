package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/eighred/kanz/internal/identity"
)

// AUTHENTICATED PROVISIONING (#364).
//
// Until this route existed, the ONLY way to create an invitation was
// cmd/kanz-invite, which writes to the store directly and therefore requires the
// database credential. That is correct for the FIRST operator — "can touch the
// server's disk" is a stronger claim than "got there first" — and wrong for
// everything after it: the act is attributable to whoever holds a DSN rather
// than to a person, and the DSN is shared.
//
// # Why this service authenticates instead of trusting the gateway's headers
//
// Every other upstream on this platform trusts X-Kanz-Principal-*, and that is
// sound because a NetworkPolicy makes the gateway their only reachable caller.
//
// THIS SERVICE CANNOT BE BEHIND THAT POLICY. /login and /invites/redeem exist to
// be reached by people who hold no token at all; that is the entire point of the
// service. So a principal header arriving here is a string the caller typed, and
// a route that trusted it would let anyone who can reach the pod mint an
// invitation carrying any role they name — including kanz-operator, in any
// tenant.
//
// THE HEADERS ARE THEREFORE NOT READ ANYWHERE IN THIS SERVICE, and
// test/arch/identity_trusts_no_headers_test.go fails the build if that changes.
// The caller presents a bearer token and this service verifies its SIGNATURE
// with the key it already holds. That is not a second identity authority
// competing with the gateway: this service mints the tokens the gateway trusts,
// so checking its own signature is the same answer.

// Verifier authenticates a bearer token this service issued.
type Verifier interface {
	Verify(raw string, now time.Time) (*identity.Claims, error)
}

// Provisioner is the store half provisioning needs, kept separate from Store so
// a deployment that does not enable provisioning cannot accidentally satisfy it.
type Provisioner interface {
	CreateInvite(ctx context.Context, inv *identity.Invite) error
	InvitesFor(ctx context.Context, tenant string) ([]*identity.Invite, error)
}

// Provisioning configures the authenticated invite routes.
type Provisioning struct {
	Verifier Verifier
	Store    Provisioner
	// OperatorRole is the role a caller must hold. Required: defaulting it would
	// pick the authority that may create accounts, which is a deployment's
	// decision and not this package's.
	OperatorRole string
	// InviteTTL is how long a new invitation stays redeemable; zero uses the
	// domain default.
	InviteTTL time.Duration
}

// WithProvisioning enables POST/GET /invites.
//
// PROVISIONING IS OFF UNTIL WIRED, and the routes do not exist when it is off —
// they are not registered rather than registered-and-refusing. A 404 and a 403
// say different things to someone probing, and "this deployment has no
// provisioning surface" is the truthful one.
func WithProvisioning(p Provisioning) Option {
	return func(s *Server) { s.provisioning = &p }
}

type createInviteRequest struct {
	Subject    string   `json:"subject"`
	Tenant     string   `json:"tenant"`
	Roles      []string `json:"roles"`
	Portfolios []string `json:"portfolios"`
}

// createInvite issues a single-use invitation on an operator's authority.
func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.operator(w, r)
	if !ok {
		return
	}
	var req createInviteRequest
	if !decode(w, r, &req) {
		return
	}

	// THE TENANT IS THE OPERATOR'S OWN, AND A REQUEST NAMING ANOTHER IS REFUSED
	// RATHER THAN SILENTLY CORRECTED.
	//
	// An operator who could set an arbitrary tenant could mint themselves an
	// account in any fund on the platform — the single most valuable escalation
	// available here, since the tenant is what every RLS policy keys on. Empty
	// means "mine", which is the common case; naming someone else's is refused
	// with both tenants said out loud, so a client that believed it was
	// provisioning elsewhere learns that it was not.
	tenant := strings.TrimSpace(req.Tenant)
	if tenant == "" {
		tenant = claims.Tenant
	}
	if tenant != claims.Tenant {
		writeErr(w, http.StatusForbidden, "an invitation may only be created in the operator's own "+
			"tenant ("+claims.Tenant+"); this named "+tenant)
		return
	}
	if claims.Tenant == "" {
		// An operator token with no tenant cannot scope an invitation, and
		// defaulting to any value would put an account somewhere nobody chose.
		writeErr(w, http.StatusForbidden, "the operator's token carries no tenant, so an invitation "+
			"cannot be scoped to one")
		return
	}

	raw, hash, err := identity.NewInviteToken()
	if err != nil {
		s.logger.Error("cannot mint an invite token", "err", err)
		writeErr(w, http.StatusInternalServerError, "cannot create the invitation")
		return
	}
	// createdBy COMES FROM THE VERIFIED TOKEN, never from the body. It is the
	// same rule #444 applied to the pricing override: an audit field a caller can
	// set is not an audit field.
	inv, err := identity.NewInvite(uuid.NewString(), hash, strings.TrimSpace(req.Subject), tenant,
		req.Roles, req.Portfolios, claims.Subject, s.now().UTC(), s.provisioning.InviteTTL)
	if err != nil {
		// NewInvite refuses the malformed: no subject, no tenant, no roles. Those
		// are the caller's input, so they are a 400 and the reason is returned.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.provisioning.Store.CreateInvite(r.Context(), inv); err != nil {
		s.logger.Error("cannot store the invitation", "subject", inv.Subject, "err", err)
		writeErr(w, http.StatusInternalServerError, "cannot create the invitation")
		return
	}
	s.logger.Info("invitation created", "subject", inv.Subject, "tenant", inv.Tenant,
		"roles", inv.Roles, "created_by", claims.Subject, "expires_at", inv.ExpiresAt)

	// THE TOKEN IS RETURNED ONCE AND IS NOT RECOVERABLE. Only its SHA-256 is
	// stored, which is the same property that makes a leaked database useless for
	// impersonating an invitee — and the reason this response is the only chance
	// to capture it.
	writeJSON(w, http.StatusCreated, map[string]any{
		"invite_token": raw,
		"invite_id":    inv.ID,
		"subject":      inv.Subject,
		"tenant":       inv.Tenant,
		"roles":        inv.Roles,
		"portfolios":   inv.Portfolios,
		"expires_at":   inv.ExpiresAt,
		"created_by":   inv.CreatedBy,
		"note":         "the invite_token is shown once and is not recoverable; only its hash is stored",
	})
}

// listInvites shows the operator's tenant's outstanding invitations.
//
// IT NEVER RETURNS A TOKEN OR ITS HASH. The listing exists so an operator can
// see what is outstanding and who created it; returning the hash would let a
// reader of this route confirm a guessed token offline.
func (s *Server) listInvites(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.operator(w, r)
	if !ok {
		return
	}
	invites, err := s.provisioning.Store.InvitesFor(r.Context(), claims.Tenant)
	if err != nil {
		s.logger.Error("cannot list invitations", "tenant", claims.Tenant, "err", err)
		writeErr(w, http.StatusInternalServerError, "cannot list invitations")
		return
	}
	now := s.now().UTC()
	out := make([]map[string]any, 0, len(invites))
	for _, inv := range invites {
		out = append(out, map[string]any{
			"invite_id":  inv.ID,
			"subject":    inv.Subject,
			"tenant":     inv.Tenant,
			"roles":      inv.Roles,
			"portfolios": inv.Portfolios,
			"created_by": inv.CreatedBy,
			"expires_at": inv.ExpiresAt,
			// Redeemable now, rather than a raw redeemed_at: an operator asking
			// "can this still be used" should not have to compare two timestamps
			// and know which of expiry or redemption takes precedence.
			"redeemable": inv.Redeemable(now) == nil,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// operator authenticates the caller and requires the operator role.
//
// A FAILURE HERE IS 401 OR 403 AND NEVER A REASON. "expired", "signed by
// something else" and "no such issuer" told apart is a probing oracle; the
// service logs which it was and the caller is told only that it was rejected.
func (s *Server) operator(w http.ResponseWriter, r *http.Request) (*identity.Claims, bool) {
	raw, ok := bearer(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "a bearer token is required")
		return nil, false
	}
	claims, err := s.provisioning.Verifier.Verify(raw, s.now().UTC())
	if err != nil {
		s.logger.Warn("provisioning request rejected a token", "err", err, "remote", r.RemoteAddr)
		writeErr(w, http.StatusUnauthorized, "the token was rejected")
		return nil, false
	}
	if !claims.HasRole(s.provisioning.OperatorRole) {
		// WARN and named. Somebody holding a valid platform token attempted to
		// create an account, and that is worth seeing whether it was a
		// misconfiguration or an attempt.
		s.logger.Warn("provisioning refused: the caller does not hold the operator role",
			"subject", claims.Subject, "tenant", claims.Tenant, "roles", claims.Roles,
			"required", s.provisioning.OperatorRole)
		writeErr(w, http.StatusForbidden, "creating an account requires the "+s.provisioning.OperatorRole+" role")
		return nil, false
	}
	return claims, true
}

// bearer extracts the token from an Authorization header.
//
// THE SCHEME IS MATCHED CASE-INSENSITIVELY because RFC 7235 says it is
// case-insensitive, and a client sending "bearer" is not an attacker — but the
// TOKEN itself is taken verbatim, since it is base64url and its case is
// significant.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	return tok, tok != ""
}
