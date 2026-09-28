package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
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
//
// UserBySubject and SetStatus are DEPROVISIONING (#525), and they are on this
// interface rather than on Store because they ride the same authority: deciding
// that an account may exist and deciding that it may stop are the same operator
// power, and splitting them across two gates would let a deployment enable one
// without the other.
type Provisioner interface {
	RevokeInvite(ctx context.Context, actor identity.Administration, id string, revision int64, now time.Time) (*identity.Invite, error)
	ReissueInvite(ctx context.Context, actor identity.Administration, id string, revision int64, newID, tokenHash string, policy identity.InviteDomainPolicy, now time.Time, ttl time.Duration) (*identity.Invite, error)
	RotateCredential(ctx context.Context, actor identity.Administration, previous, replacement identity.Hash, now time.Time) error
	CreateInviteAs(ctx context.Context, actor identity.Administration, inv *identity.Invite) error
	InvitesFor(ctx context.Context, tenant string) ([]*identity.Invite, error)
	UserBySubject(ctx context.Context, subject string) (*identity.User, error)
	SetStatus(ctx context.Context, actor identity.Administration, subject string, status identity.Status, now time.Time) error
	UsersFor(ctx context.Context, actor identity.Administration, after string) ([]*identity.Access, error)
	SetAccess(ctx context.Context, actor identity.Administration, subject string, revision int64, roles, portfolios []string, now time.Time) (*identity.Access, error)
}

// Provisioning configures the authenticated invite and account-status routes.
type Provisioning struct {
	Verifier Verifier
	Store    Provisioner
	// AdminRole is the role a caller must hold. Required: defaulting it would
	// pick the authority that may create accounts, which is a deployment's
	// decision and not this package's.
	AdminRole string
	// InviteTTL is how long a new invitation stays redeemable; zero uses the
	// domain default.
	InviteTTL     time.Duration
	InviteDomains identity.InviteDomainPolicy

	// Audit is the additional operational status projection. REQUIRED so its
	// absence cannot silently remove existing operational visibility. The store
	// commits canonical DecisionLog evidence with each access/status mutation;
	// this recorder is not the durability boundary.
	Audit auth.DecisionRecorder
}

// WithProvisioning enables invitations, account status/access administration,
// the tenant directory and current identity permissions.
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

	if err := s.provisioning.InviteDomains.Check(req.Subject); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
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
	if err := s.provisioning.Store.CreateInviteAs(r.Context(), identity.Administration{Subject: claims.Subject, Tenant: claims.Tenant, IssuedAt: claims.IssuedAt, SessionEpoch: claims.SessionEpoch}, inv); err != nil {
		if errors.Is(err, identity.ErrAdminAuthority) {
			writeErr(w, http.StatusUnauthorized, "the token was rejected")
			return
		}
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
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, issuedInvitation{InvitationSummary: inv.Summary(s.now().UTC()), Token: raw, Note: "the invite_token is shown once and is not recoverable; only its hash is stored"})
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
	out := make([]identity.InvitationSummary, 0, len(invites))
	for _, inv := range invites {
		out = append(out, inv.Summary(now))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// operator authenticates the caller, requires the operator role, and requires
// that the caller's ACCOUNT is still active (#525).
//
// A FAILURE HERE IS 401 OR 403 AND NEVER A REASON. "expired", "signed by
// something else", "no such issuer" and "disabled since it was issued" told apart
// is a probing oracle; the service logs which it was and the caller is told only
// that it was rejected. The ONE exception is 503, and it is deliberate: a status
// that could not be checked is not a status that was checked.
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
	if !claims.HasRole(s.provisioning.AdminRole) || identity.ValidateAdminRoles(claims.Roles) != nil {
		// WARN and named. Somebody holding a valid platform token attempted to
		// create an account, and that is worth seeing whether it was a
		// misconfiguration or an attempt.
		s.logger.Warn("provisioning refused: the caller does not hold the operator role",
			"subject", claims.Subject, "tenant", claims.Tenant, "roles", claims.Roles,
			"required", s.provisioning.AdminRole)
		writeErr(w, http.StatusForbidden, "creating an account requires the "+s.provisioning.AdminRole+" role")
		return nil, false
	}

	// THE TOKEN IS NOT THE ACCOUNT (#525).
	//
	// Verify checks a signature, an expiry, an issuer and an audience — all
	// properties frozen at the moment the token was minted. Nothing above this
	// line reads the account, so a DISABLED OPERATOR'S OUTSTANDING TOKEN STILL
	// CREATES AND DISABLES ACCOUNTS for the rest of its TTL (8h by default). That
	// is the single highest-privilege residual token on this platform, and it
	// belonged to somebody the estate had already decided to lock out.
	//
	// IT IS CHECKED HERE AND NOT AT THE GATEWAY because here it is cheap and there
	// it is not: this process already holds the identity pool, and this is a
	// low-traffic authenticated route, so the cost is one indexed primary-key read
	// per operator action. The gateway holds no database client at all — giving it
	// one is a connection-budget and NetworkPolicy change, and the right answer
	// there is a revocation FACT on the bus rather than a per-request lookup. The
	// residual token on EVERY OTHER route therefore remains open; this closes it
	// only on the routes that provision.
	u, err := s.provisioning.Store.UserBySubject(r.Context(), claims.Subject)
	switch {
	case err != nil && !errors.Is(err, identity.ErrUserNotFound):
		// FAIL CLOSED, AND SAY SO AS ITS OWN OUTCOME. "The status was checked and
		// the account is disabled" and "the status could not be checked" are
		// different facts, and answering the same 401 to both would let a store
		// outage read in the logs as a wave of disabled operators — or, worse,
		// invite somebody to make the unreachable case permissive. 503 says the
		// service could not answer, which is the truth and is retryable.
		s.logger.Error("provisioning refused: the caller's account status could not be checked",
			"subject", claims.Subject, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "the operator's account status could not be checked")
		return nil, false
	case err != nil || !(identity.Administration{Subject: claims.Subject, Tenant: claims.Tenant, IssuedAt: claims.IssuedAt, SessionEpoch: claims.SessionEpoch}).Allows(u):
		// THE SAME 401 AS A REJECTED TOKEN, AND NO REASON — the stance this
		// function's doc states. A disabled operator learning "disabled" rather
		// than "rejected" learns that their subject is still a known account; a
		// validly-signed token naming no account at all (deleted, or minted by
		// something that should not have) is the same refusal for the same reason.
		// The log tells them apart.
		reason := "the account authority no longer matches the token"
		if err != nil {
			reason = "the token names no account in this store"
		}
		s.logger.Warn("provisioning refused: a valid token whose account may not act",
			"subject", claims.Subject, "tenant", claims.Tenant, "reason", reason)
		writeErr(w, http.StatusUnauthorized, "the token was rejected")
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
