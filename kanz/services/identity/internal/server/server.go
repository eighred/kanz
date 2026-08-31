// Package server is the identity service's HTTP surface (#364).
//
// FOUR ROUTES, AND THE SPLIT BETWEEN THEM IS THE SECURITY MODEL.
//
//	POST /login          unauthenticated — a person exchanges a credential for a token
//	POST /invites/redeem unauthenticated — an invitee exchanges a link for an account
//	GET  /jwks.json      unauthenticated — the gateway fetches the public key
//	GET  /revocations    unauthenticated — the gateway fetches who has been disabled
//
// All four are unauthenticated BY NECESSITY: you cannot require a token from
// someone who is trying to obtain one, and the gateway cannot present a
// credential to fetch the key it would need in order to verify credentials —
// nor to fetch the list it needs in order to decide whether a credential it has
// just verified is still honoured. The two the gateway reads are what make it
// able to judge anybody; requiring authentication on either would make the
// control depend on the control. See revocations.go for what keeps that
// acceptable, and what must stay true for it to remain so.
//
// WHY THIS IS A SERVICE AND NOT A HANDLER INSIDE THE GATEWAY. The gateway is the
// internet-facing process and the sole identity authority — it would have been
// the obvious host. It must not be, because it must NOT HOLD THE SIGNING KEY:
// slice 3 made issuance asymmetric precisely so the verifier cannot forge, and
// putting the private key in the verifier would hand that property straight back.
// So the key and the credential store live here, behind the gateway, and the
// gateway holds only the public half it fetches from /jwks.json.
//
// THE FOUR ABOVE ARE THE ONES THAT MUST WORK FOR SOMEONE HOLDING NOTHING.
// Everything else this service serves — creating invites (#364), disabling and
// re-enabling accounts (#525) — is an operator action behind a verified bearer
// token, registered only when a deployment wires provisioning. See provision.go
// and status.go for why this service authenticates instead of trusting the
// gateway's principal headers.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/revocation"
)

// Store is the persistence this server needs — the subset of
// identity.Postgres it actually calls, so a test can substitute one.
type Store interface {
	UserBySubject(ctx context.Context, subject string) (*identity.User, error)
	UpdateCredential(ctx context.Context, subject string, cred identity.Hash, now time.Time) error
	Redeem(ctx context.Context, rawToken string, cred identity.Hash, now time.Time) (*identity.User, error)
	// Revocations feeds the api-gateway's per-subject revocation check (#532).
	// It is on the BASE interface and not on Provisioning because the feed must
	// be served whether or not this deployment can disable accounts — see
	// revocations.go for why an empty feed and an absent one are different
	// answers.
	Revocations(ctx context.Context) ([]revocation.Entry, error)
}

// Minter issues a bearer token for an account.
type Minter interface {
	Mint(u *identity.User) (string, time.Time, error)
}

// Limiter decides whether an unauthenticated attempt may proceed.
//
// IT IS A REQUIRED DEPENDENCY, not an option, because nothing upstream provides
// one. The gateway's Quota middleware sits AFTER its auth middleware, so an
// anonymous request never reaches it. A login endpoint with no limiter is an
// offline password-guessing oracle that answers as fast as Argon2id allows.
//
// THE GATEWAY NOW HAS A PRE-AUTH LIMITER OF ITS OWN (middleware.PreAuth, #835)
// AND IT DOES NOT COVER THIS SERVICE, which is worth saying because the sentence
// this replaces — "middleware.RateLimit is wired nowhere" — no longer parses.
// identity cannot sit behind the gateway at all: /login and /invites/redeem
// exist to be reached by people holding no token. Nothing about the gateway's
// chain reaches a request that never goes through the gateway.
type Limiter interface {
	// Allow reports whether an attempt keyed by k may proceed.
	Allow(k string) bool
}

// Server serves the credential surface.
type Server struct {
	store   Store
	minter  Minter
	limiter Limiter
	logger  *slog.Logger
	jwks    func() any
	now     func() time.Time

	// issuer is this service's own base URL, published in the discovery document
	// so a gateway configured with nothing but an issuer can find the key.
	issuer string

	// provisioning, when non-nil, enables the AUTHENTICATED invite routes. Nil
	// means this deployment has no provisioning surface at all and the routes are
	// not registered — see provision.go.
	provisioning *Provisioning

	// decoyHash is verified against when a subject does not exist, so a caller
	// cannot tell "no such account" from "wrong password" BY TIMING. Without it
	// the unknown-subject path returns in microseconds while the known-subject
	// path spends Argon2id's full cost, and that difference enumerates the
	// platform's users — which on this system is a fund's traders and operators.
	decoyHash identity.Hash
}

// New builds the server. Every dependency is required: a nil limiter in
// particular would turn the login route into an unbounded guessing oracle, and
// defaulting it to "allow everything" is the kind of convenience that is
// indistinguishable from working.
// Option customizes the server.
type Option func(*Server)

func New(store Store, minter Minter, limiter Limiter, jwks func() any, issuer string, logger *slog.Logger, opts ...Option) (*Server, error) {
	switch {
	case store == nil:
		return nil, errors.New("identity/server: store required")
	case minter == nil:
		return nil, errors.New("identity/server: minter required")
	case limiter == nil:
		return nil, errors.New("identity/server: limiter required — the gateway's quota middleware " +
			"runs after authentication and never sees an anonymous request, so this is the only " +
			"bound on credential guessing")
	case jwks == nil:
		return nil, errors.New("identity/server: jwks source required")
	case strings.TrimSpace(issuer) == "":
		return nil, errors.New("identity/server: issuer required — it is published in the discovery " +
			"document, and OIDC verifiers refuse a document whose issuer does not match the URL they " +
			"discovered it from")
	}
	if logger == nil {
		logger = slog.Default()
	}
	// A hash of a value nobody can present. Its only job is to cost the same as
	// a real verification.
	decoy, err := identity.HashCredential("decoy-credential-that-matches-nothing")
	if err != nil {
		return nil, err
	}
	s := &Server{
		store: store, minter: minter, limiter: limiter, logger: logger,
		jwks: jwks, issuer: strings.TrimRight(issuer, "/"), now: time.Now, decoyHash: decoy,
	}
	for _, o := range opts {
		o(s)
	}
	// PROVISIONING IS REFUSED AT CONSTRUCTION IF IT IS HALF-WIRED, rather than at
	// the first request. Every field is load-bearing: no verifier means the route
	// authenticates nobody, and an empty operator role means HasRole("") decides
	// who may create accounts — which is the one question this route exists to
	// answer and the one nobody would notice being answered wrongly.
	if p := s.provisioning; p != nil {
		switch {
		case p.Verifier == nil:
			return nil, errors.New("identity/server: provisioning needs a verifier — this service is " +
				"reachable without a token, so a provisioning route that does not verify one is open")
		case p.Store == nil:
			return nil, errors.New("identity/server: provisioning needs a store")
		case strings.TrimSpace(p.OperatorRole) == "":
			return nil, errors.New("identity/server: provisioning needs the operator role named — an " +
				"empty role matches nothing, so every authenticated caller would be refused, and a " +
				"role check nobody can pass is indistinguishable from a broken deployment")
		case p.Audit == nil:
			return nil, errors.New("identity/server: provisioning needs an audit recorder — these " +
				"routes create and disable accounts, and an account disabled by nobody-in-particular " +
				"is the unattributable hand-run UPDATE they exist to replace; pass " +
				"auth.NewSlogRecorder(logger) if this deployment has no bus")
		}
	}
	return s, nil
}

// Routes registers the surface on a mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /invites/redeem", s.redeem)
	mux.HandleFunc("GET /jwks.json", s.jwksHandler)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.discoveryHandler)
	// UNCONDITIONAL, unlike the provisioning routes below — see revocations.go.
	// The gateway refuses to become ready until this answers, so a deployment
	// that registered it only when provisioning was wired would take the
	// platform's sole ingress out of service by omission.
	mux.HandleFunc("GET /revocations", s.revocationsHandler)
	// AUTHENTICATED provisioning (#364). Registered only when wired, so a
	// deployment without it answers 404 rather than 403 — "there is no
	// provisioning surface here" is the truthful answer to someone probing.
	if s.provisioning != nil {
		mux.HandleFunc("POST /invites", s.createInvite)
		mux.HandleFunc("GET /invites", s.listInvites)
		// DEPROVISIONING (#525), on the SAME gate and for the same reason: an
		// unconfigured deployment must answer 404 to "can I disable an account
		// here", not 403, because it truthfully cannot.
		mux.HandleFunc("POST /users/{subject}/disable", s.disableUser)
		mux.HandleFunc("POST /users/{subject}/enable", s.enableUser)
	}
}

type loginRequest struct {
	Subject    string `json:"subject"`
	Credential string `json:"credential"`
}

type redeemRequest struct {
	Token      string `json:"token"`
	Credential string `json:"credential"`
}

type tokenResponse struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires_at"`
	Subject string    `json:"subject"`
	Tenant  string    `json:"tenant"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decode(w, r, &req) {
		return
	}
	if !s.allowAttempt(r, req.Subject) {
		// 429 rather than 401: the caller may hold a correct credential and must
		// be told to wait rather than that it was wrong.
		writeErr(w, http.StatusTooManyRequests, "too many attempts")
		return
	}

	u, err := s.store.UserBySubject(r.Context(), req.Subject)
	if err != nil {
		// UNKNOWN SUBJECT STILL PAYS THE ARGON2 COST. Returning here directly
		// would make the unknown case orders of magnitude faster than the known
		// one, and that timing difference is a user-enumeration oracle.
		_ = identity.Verify(s.decoyHash, req.Credential)
		s.deny(w, req.Subject, "unknown subject")
		return
	}
	if err := identity.Verify(u.Credential, req.Credential); err != nil {
		s.deny(w, req.Subject, "credential mismatch")
		return
	}
	if !u.Active() {
		// Checked AFTER the credential, so a disabled account is not detectable
		// without knowing its password.
		s.deny(w, req.Subject, "account disabled")
		return
	}

	// The cost parameters may have been raised since this credential was stored;
	// a successful login is the only moment the plaintext is available to
	// re-derive with. Failure here must not fail the login — the user is
	// authenticated either way, and refusing them because an upgrade write
	// failed would turn a hardening step into an outage.
	if identity.NeedsRehash(u.Credential) {
		if fresh, herr := identity.HashCredential(req.Credential); herr == nil {
			if uerr := s.store.UpdateCredential(r.Context(), u.Subject, fresh, s.now()); uerr != nil {
				s.logger.Warn("credential rehash failed; the account still uses older parameters",
					"subject", u.Subject, "err", uerr)
			}
		}
	}

	s.issue(w, u)
}

func (s *Server) redeem(w http.ResponseWriter, r *http.Request) {
	var req redeemRequest
	if !decode(w, r, &req) {
		return
	}
	if !s.allowAttempt(r, "") {
		writeErr(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	// THE POLICY IS ENFORCED HERE, SERVER-SIDE, because the browser form that
	// also checks it is a courtesy and not a control — anyone can POST straight
	// at this route. Until this existed the ONLY rule was non-empty, so a
	// one-character password was accepted for an account carrying kanz-trader.
	//
	// The reason IS returned, unlike every other refusal on this handler: it
	// concerns the credential the caller has just invented, so it reveals nothing
	// about the estate, and withholding it would leave someone retrying a rule
	// they cannot see — with a single-use invitation.
	if err := identity.ValidateCredential(req.Credential); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	cred, err := identity.HashCredential(req.Credential)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "a credential is required")
		return
	}
	u, err := s.store.Redeem(r.Context(), req.Token, cred, s.now())
	if err != nil {
		// ONE ANSWER FOR EVERY REFUSAL. Unknown, expired and already-redeemed are
		// indistinguishable to someone holding only a link: telling them an
		// invite exists confirms an account was offered to somebody.
		s.logger.Info("invite redemption refused", "err", err)
		writeErr(w, http.StatusUnauthorized, "that invitation is not valid")
		return
	}
	s.issue(w, u)
}

// issue mints and returns a token. The response deliberately carries the subject
// and tenant so a client need not decode the token to know who it is — the REPL
// currently base64-decodes the payload WITHOUT verifying to answer /whoami.
func (s *Server) issue(w http.ResponseWriter, u *identity.User) {
	tok, expiry, err := s.minter.Mint(u)
	if err != nil {
		s.logger.Error("minting failed for an authenticated caller", "subject", u.Subject, "err", err)
		writeErr(w, http.StatusInternalServerError, "could not issue a token")
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		Token: tok, Expires: expiry, Subject: u.Subject, Tenant: u.Tenant,
	})
}

// deny answers every failed login identically.
//
// The REASON is logged and never returned. An operator needs to tell "nobody by
// that name" from "wrong password"; the caller must not, because the difference
// is precisely what turns a login form into a list of the fund's staff.
func (s *Server) deny(w http.ResponseWriter, subject, reason string) {
	s.logger.Info("login refused", "subject", subject, "reason", reason)
	writeErr(w, http.StatusUnauthorized, "invalid credentials")
}

func (s *Server) jwksHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.jwks())
}

// discoveryHandler publishes the minimum OIDC discovery document.
//
// WITHOUT IT, WIRING THE GATEWAY IS A TRAP. pkg/auth discovers the key set from
// {issuer}/.well-known/openid-configuration unless an explicit JWKS URI is
// configured, so an operator who sets only the issuer — the ordinary thing, and
// what every other IdP needs — would get a 404 here and a gateway that cannot
// verify a single token this service issues. Serving it means the standard
// configuration works, rather than working only for someone who knew about the
// second setting.
//
// It carries the two fields a verifier needs and no more. This is not a
// full-featured OIDC provider and must not advertise endpoints it does not have
// — an authorization_endpoint that 404s is worse than an absent one.
func (s *Server) discoveryHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":   s.issuer,
		"jwks_uri": s.issuer + "/jwks.json",
	})
}

// ClientIPHeader carries the ORIGINAL caller's address, set by the web-bff.
//
// It is this repo's own name rather than a vendor one (CF-Connecting-IP): the
// BFF reaches this service over the private network with no edge in between, so
// reusing the vendor name would invite someone to trust it here too.
//
// TRUSTING IT IS SOUND FOR THE SAME REASON THE GATEWAY'S PRINCIPAL HEADERS ARE:
// a NetworkPolicy makes the BFF the only caller that can reach this service. If
// that ever stops holding, this header becomes forgeable and the per-source
// bound below stops existing — which is why the subject bound is checked
// independently rather than being folded into one composite key.
const ClientIPHeader = "X-Kanz-Client-IP"

// allowAttempt bounds an unauthenticated attempt on BOTH axes, and both must
// permit it.
//
// SUBJECT ALONE IS NOT ENOUGH. Keying only on the account lets one source spray
// a single guess across thousands of accounts — credential stuffing, which is
// how leaked password lists are actually used — without ever exhausting a
// bucket. ADDRESS ALONE IS NOT ENOUGH EITHER: every login arrives through the
// BFF, so one attacker's guesses would spend the budget shared by every
// legitimate operator behind it.
//
// Checked in that order deliberately: a refusal on the subject axis does not
// debit the source axis, so an attacker cannot exhaust an address bucket that
// legitimate callers share by hammering one account.
func (s *Server) allowAttempt(r *http.Request, subject string) bool {
	if subject != "" && !s.limiter.Allow("s:"+subject) {
		return false
	}
	return s.limiter.Allow("a:" + clientIP(r))
}

// clientIP is the address an attempt is attributed to.
//
// The BFF's forwarded header wins when present. Without this the header the BFF
// takes care to send would be silently ignored, every attempt would be
// attributed to the BFF itself, and the per-source bound would be one bucket for
// the entire estate — "limited" and "not limited" looking identical from here.
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(ClientIPHeader)); v != "" {
		return v
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// maxBody bounds a credential request. A login body is small; anything larger is
// not a login.
const maxBody = 8 << 10

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
