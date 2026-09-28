// Package server is the web-BFF's HTTP surface (PS-02a): the browser login
// endpoints (OIDC authorization-code + PKCE against Eighred SSO) and a
// session-authenticated reverse proxy to the api-gateway /v1 edge. The browser
// holds only an opaque httpOnly session cookie; the BFF attaches the
// server-held bearer to each proxied call, which the gateway validates.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"

	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/internal/clientip"
	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/internal/requestbody"
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
	"github.com/eighred/kanz/services/web-bff/internal/oidc"
	"github.com/eighred/kanz/services/web-bff/internal/session"
)

const sessionCookie = "kanz_session"

// Readiness gates traffic.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Options configures a Server.
type Options struct {
	// Identity is the credential login path (#364/#371) — REQUIRED. OIDC is the
	// optional alternative for a client bringing their own IdP.
	Identity *identityclient.Client
	// ClientIP resolves the caller's address behind the edge, for the identity
	// service's rate limiter. REQUIRED: without it every browser login arrives
	// from this process and the limiter keys them all together, so one user's
	// failures throttle everybody and an attacker hides in the same bucket.
	ClientIP *clientip.Resolver
	// StaticDir is the compiled SPA served on THIS origin. Empty ⇒ API only,
	// which is the local shape where Vite serves the SPA on its own port.
	StaticDir  string
	OIDC       *oidc.Client
	Sessions   *session.Manager
	GatewayURL string
	// SigningSecret is the gateway's API-01d request-signing secret. The
	// gateway runs its Signing middleware whenever its own secret is set --
	// which every deployment manifest does -- and rejects an unsigned request
	// BEFORE authentication runs. Empty sends no header, which is correct only
	// against a gateway with signing disabled (#777).
	SigningSecret           string
	SecureCookies           bool
	Logger                  *slog.Logger
	Metrics                 http.Handler
	PreflightEvidencePath   string
	PreflightEvidenceMaxAge time.Duration
}

// Server wires the login flow and the authenticated proxy.
type Server struct {
	readiness     *Readiness
	identity      *identityclient.Client
	clientIP      *clientip.Resolver
	static        *staticHandler
	oidc          *oidc.Client
	sessions      *session.Manager
	proxy         *httputil.ReverseProxy
	signingSecret []byte
	secureCookies bool
	logger        *slog.Logger
	metrics       http.Handler
	preflight     *preflightEvidence
	mux           *http.ServeMux
	browserOrigin http.CrossOriginProtection
}

// New builds the server. gatewayURL must be a valid absolute URL.
func New(readiness *Readiness, opts Options) (*Server, error) {
	gw, err := url.Parse(opts.GatewayURL)
	if err != nil {
		return nil, err
	}
	// REQUIRED, not defaulted. A nil Identity makes the only working login route
	// panic on the first attempt; a nil ClientIP makes every browser login look
	// like it came from this process, so the identity service's limiter keys them
	// all together — one user's failures throttle everybody, and an attacker's
	// attempts hide in the same bucket. Both are silent, and both are the kind of
	// thing a zero value provides happily.
	if opts.Sessions == nil {
		return nil, errors.New("web-bff: session authority required")
	}
	if opts.Identity == nil {
		return nil, errors.New("web-bff: identity client required — it is the only way anyone signs in")
	}
	if opts.ClientIP == nil {
		return nil, errors.New("web-bff: client-IP resolver required — without it every login is " +
			"attributed to this process and the identity service's rate limit becomes global")
	}
	static, err := newStaticHandler(opts.StaticDir)
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		readiness:     readiness,
		identity:      opts.Identity,
		clientIP:      opts.ClientIP,
		static:        static,
		oidc:          opts.OIDC,
		sessions:      opts.Sessions,
		proxy:         newProxy(gw),
		signingSecret: []byte(opts.SigningSecret),
		secureCookies: opts.SecureCookies,
		logger:        logger,
		metrics:       opts.Metrics,
		preflight:     newPreflightEvidence(opts.PreflightEvidencePath, opts.PreflightEvidenceMaxAge),
		mux:           http.NewServeMux(),
	}
	s.routes()
	// The browser-facing origin owns these headers, including proxied errors.
	// ReverseProxy otherwise appends upstream headers to our response policy.
	s.proxy.ModifyResponse = func(r *http.Response) error {
		discardUpstreamBrowserPolicy(r.Header)
		r.Header.Del("Cache-Control")
		r.Header.Del("Expires")
		return nil
	}
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setBrowserHeaders(w.Header(), s.secureCookies)
	if strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	if !s.browserRequestAllowed(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
		return
	}
	s.mux.ServeHTTP(w, r)
}

// Close releases the OS resources the Server owns — today, the static root's
// directory handle. Safe to call on a Server that serves no static build.
//
// It is separate from the HTTP server's Shutdown, which drains connections and
// knows nothing about what a handler holds open.
func (s *Server) Close() error { return s.static.Close() }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /auth/mfa", s.handleMFA)
	for _, path := range []string{"register/begin", "register/finish", "login/finish", "stepup/begin", "stepup/finish", "remove"} {
		s.mux.HandleFunc("POST /auth/mfa/"+path, s.handleMFA)
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if !s.readiness.Ready() || s.sessions.Ping(ctx) != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	if s.metrics != nil {
		s.mux.Handle("GET /metrics", s.metrics)
	}
	// POST is the CREDENTIAL login; GET is the optional OIDC redirect start.
	// Same path, different methods, because to a browser they are one idea.
	s.mux.HandleFunc("POST /auth/login", s.handleCredentialLogin)
	s.mux.HandleFunc("POST /auth/redeem", s.handleRedeem)
	// REGISTERED ONLY WHEN CONFIGURED. An unconfigured OIDC route would answer a
	// browser with a redirect to nowhere, which is how the TUI's /login came to
	// report "login required but failed" against an issuer that did not exist.
	if s.oidc != nil {
		s.mux.HandleFunc("GET /auth/login", s.handleLogin)
		s.mux.HandleFunc("GET /auth/callback", s.handleCallback)
	}
	s.mux.HandleFunc("POST /auth/logout", s.handleLogout)
	s.mux.HandleFunc("POST /auth/credential", s.handleCredentialRotation)
	s.mux.HandleFunc("POST /auth/mailbox", s.handleRecovery)
	s.mux.HandleFunc("GET /auth/mailbox", s.handleRecovery)
	s.mux.HandleFunc("POST /auth/mailbox/verify", s.handleRecovery)
	s.mux.HandleFunc("POST /auth/recovery", s.handleRecovery)
	s.mux.HandleFunc("POST /auth/recovery/consume", s.handleRecovery)
	s.mux.HandleFunc("GET /auth/me", s.handleMe)
	s.mux.HandleFunc("GET /auth/sessions", s.handleSessions)
	s.mux.HandleFunc("POST /auth/sessions/{id}/revoke", s.handleRevokeSession)
	// Identity provisioning is a separate authority from the gateway API. These
	// two exact routes keep its bearer in the BFF session while avoiding a broad
	// identity proxy that would expose future administrative endpoints by
	// accident.
	s.mux.HandleFunc("GET /api/identity/invites", s.handleInvites)
	s.mux.HandleFunc("POST /api/identity/invites", s.handleInvites)
	s.mux.HandleFunc("POST /api/identity/invites/{id}/revoke", s.handleInvites)
	s.mux.HandleFunc("POST /api/identity/invites/{id}/reissue", s.handleInvites)
	s.mux.HandleFunc("GET /api/identity/users", s.handleIdentityAccess)
	s.mux.HandleFunc("PUT /api/identity/users/{subject}/access", s.handleIdentityAccess)
	s.mux.HandleFunc("POST /api/identity/users/{subject}/disable", s.handleIdentityAccess)
	s.mux.HandleFunc("POST /api/identity/users/{subject}/enable", s.handleIdentityAccess)
	s.mux.HandleFunc("GET /auth/permissions", s.handleIdentityAccess)
	s.mux.HandleFunc("GET /api/preflight", s.handlePreflight)
	// Everything under /api/ is proxied to the gateway as the session's caller.
	s.mux.HandleFunc("/api/", s.handleProxy)
	// The SPA last, on "/" — every route above is registered on a more specific
	// pattern, so ServeMux prefers them. Registered only when a build is
	// configured, so an API-only deployment 404s rather than serving nothing.
	if s.static != nil {
		s.mux.Handle("/", s.static)
	}
}

func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentSession(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.preflight.read(time.Now().UTC()))
}

// handleLogin starts the auth-code + PKCE flow: mint state + verifier, stash the
// verifier server-side keyed by state, and redirect the browser to SSO.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	pkce, err := oidc.NewPKCE()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "login init failed", err)
		return
	}
	state, err := oidc.NewState()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "login init failed", err)
		return
	}
	if err = s.sessions.PutPending(r.Context(), state, pkce.Verifier); err != nil {
		s.sessionError(w, err)
		return
	}
	authURL, err := s.oidc.AuthCodeURL(r.Context(), state, pkce.Challenge)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "cannot reach identity provider", err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "kanz_login", Value: state, Path: "/auth/callback", MaxAge: int(session.PendingTTL.Seconds()), HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleCallback completes the flow: verify state, redeem the code with the
// stashed verifier, create a session, set the cookie, and land the browser.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.fail(w, http.StatusUnauthorized, "sign-in was denied: "+e, nil)
		return
	}
	code, state := q.Get("code"), q.Get("state")
	if code == "" || state == "" {
		s.fail(w, http.StatusBadRequest, "missing code or state", nil)
		return
	}
	binding, e := r.Cookie("kanz_login")
	if e != nil || subtle.ConstantTimeCompare([]byte(binding.Value), []byte(state)) != 1 {
		s.fail(w, 400, "invalid or expired login state", nil)
		return
	}
	verifier, ok, takeErr := s.sessions.TakePending(r.Context(), state)
	if takeErr != nil {
		s.sessionError(w, takeErr)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "kanz_login", Value: "", Path: "/auth/callback", MaxAge: -1, HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode})
	if !ok {
		// Unknown/expired/replayed state — the anti-forgery check (a forged
		// callback has no matching pending login).
		s.fail(w, http.StatusBadRequest, "invalid or expired login state", nil)
		return
	}
	tok, err := s.oidc.Exchange(r.Context(), code, verifier)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "token exchange failed", err)
		return
	}
	principal, err := s.oidc.Identity(r.Context(), tok.IDToken)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "sign-in identity could not be verified", nil)
		return
	}
	tok.Expiry = s.sessions.Expiry(tok.Expiry)
	id, err := s.sessions.Replace(r.Context(), sessionID(r), session.Session{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Subject:      principal.Subject,
		Tenant:       principal.Tenant,
		Expiry:       tok.Expiry,
		Authority:    "oidc:" + s.oidc.Issuer(),
	}, false)
	if err != nil {
		s.sessionError(w, err)
		return
	}
	s.setSessionCookie(w, id, tok.Expiry)
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleCredentialLogin exchanges a credential for a browser session (#371).
//
// THE TOKEN NEVER REACHES THE BROWSER. It is held in the server-side session and
// attached by handleProxy; the response carries only who you are and until when.
// That is the property this whole BFF exists for — a token in browser-reachable
// JS is an exfiltration target, and a kanz token carries kanz-trader.
func (s *Server) handleCredentialLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subject    string `json:"subject"`
		Credential string `json:"credential"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	tok, err := s.identity.Login(r.Context(), req.Subject, req.Credential, s.clientIP.Resolve(r))
	s.completeLogin(w, r, tok, err)
}

// handleRedeem turns an invite into an account AND a session in one step, so an
// invitee is signed in by the act of accepting rather than being asked for the
// credential they set one second earlier.
func (s *Server) handleRedeem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token      string `json:"token"`
		Credential string `json:"credential"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	tok, err := s.identity.Redeem(r.Context(), req.Token, req.Credential, s.clientIP.Resolve(r))

	// THE ONE REFUSAL ON THIS SURFACE THAT SAYS WHY, and only here.
	//
	// A 400 from redemption is about the credential the invitee has just
	// invented — under the minimum length — so it discloses nothing about the
	// estate. Collapsed into the opaque 401 that every other refusal gets, it
	// would reach them as "that invitation is not valid", and they would abandon
	// a perfectly good single-use invitation and ask an operator for another.
	//
	// LOGIN DELIBERATELY DOES NOT DO THIS. There, every refusal is one answer,
	// because the difference between "no such account" and "wrong password" is
	// what turns a sign-in form into a list of the fund's staff — and identity's
	// login path never checks credential POLICY, so a 400 there would only ever
	// mean a malformed body, which is nothing a browser needs spelled out.
	var invalid *identityclient.ErrInvalidInput
	if errors.As(err, &invalid) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": invalid.Message})
		return
	}
	s.completeLogin(w, r, tok, err)
}

// completeLogin turns a minted token into a session + cookie, or an error into
// the one answer the browser is allowed to see.
func (s *Server) completeLogin(w http.ResponseWriter, r *http.Request, tok *identityclient.Token, err error) {
	if err == nil && tok != nil && tok.MFA != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"mfa": tok.MFA})
		return
	}

	var invalid *identityclient.ErrInvalidInput
	switch {
	case errors.As(err, &invalid):
		// COLLAPSED INTO THE ONE ANSWER, deliberately, and written out rather than
		// falling through to a neighbouring case — this switch has no expression,
		// so `fallthrough` would land in whichever body happens to come next.
		//
		// Redemption intercepts this before it reaches here and relays the reason,
		// because there it concerns a credential the invitee just chose. On the
		// LOGIN path it can only mean a malformed body — nothing a browser needs
		// spelled out — and answering it differently from any other refusal would
		// give a caller a second distinguishable response to probe with. It must
		// not become a 502 either: the identity service answered, and reporting it
		// as unavailable would send an operator to look at a healthy service.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "those details are not valid"})
		return
	case errors.Is(err, identityclient.ErrThrottled):
		// 429, NOT 401: the caller may hold a correct credential and must be told
		// to wait rather than that it was wrong.
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts, try again shortly"})
		return
	case errors.Is(err, identityclient.ErrRejected):
		// ONE ANSWER. Unknown subject, wrong credential, disabled account and an
		// invalid invite are indistinguishable here because the identity service
		// already collapses them — re-separating them at this layer would undo
		// that and hand back a user-enumeration oracle.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "those details are not valid"})
		return
	case err != nil:
		s.fail(w, http.StatusBadGateway, "the identity service is unavailable", err)
		return
	}

	strict := strings.HasPrefix(r.URL.Path, "/auth/mfa/") && r.URL.Path != "/auth/mfa/login/finish"
	tok.Expires = s.sessions.Expiry(tok.Expires)
	id, cerr := s.sessions.Replace(r.Context(), sessionID(r), session.Session{
		AccessToken: tok.Token,
		Subject:     tok.Subject,
		Tenant:      tok.Tenant,
		Expiry:      tok.Expires,
	}, strict)
	if cerr != nil {
		s.sessionError(w, cerr)
		return
	}
	s.setSessionCookie(w, id, tok.Expires)
	// JSON rather than a redirect: the caller is a fetch() from the SPA, and a
	// 302 would be followed transparently and land HTML in a JSON parser.
	writeJSON(w, http.StatusOK, map[string]any{
		"subject":    tok.Subject,
		"tenant":     tok.Tenant,
		"expires_at": tok.Expires.Format(time.RFC3339),
	})
}

// decodeJSON reads a small JSON body, answering 400 on anything unreadable.
//
// The body is BOUNDED: these two routes are unauthenticated, so an unbounded
// read is a memory-exhaustion surface reachable by anyone who can open a socket
// to the edge.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
		return false
	}
	return true
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.Delete(r.Context(), sessionID(r)); err != nil {
		s.sessionError(w, err)
		return
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"subject":    sess.Subject,
		"tenant":     sess.Tenant,
		"expires_at": sess.Expiry.Format(time.RFC3339),
	})
}

// handleInvites forwards the authenticated operator's invitation request to
// identity. The browser's Authorization header is ignored; the only bearer
// identity sees is the one held in the server-side session.
func (s *Server) handleInvites(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(w, r)
	if !ok {
		return
	}
	var body []byte
	if r.Method == http.MethodPost {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
		if err != nil || len(body) > 64<<10 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
			return
		}
	}
	path := "/invites"
	if id := r.PathValue("id"); id != "" {
		action := "revoke"
		if strings.HasSuffix(r.Pattern, "/reissue") {
			action = "reissue"
		}
		path += "/" + url.PathEscape(id) + "/" + action
	}
	resp, err := s.identity.Administration(r.Context(), r.Method, path, sess.AccessToken, body)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "the identity service is unavailable", err)
		return
	}
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// handleProxy forwards /api/* to the gateway /v1/* as the session's caller.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(w, r)
	if !ok {
		return
	}
	body, ok := s.readProxyBody(w, r)
	if !ok {
		return
	}
	out := r.Clone(r.Context())
	// Strip the /api prefix so /api/v1/ask reaches the gateway as /v1/ask.
	out.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
	// Keep encoded identifier characters as data at the gateway (#1193).
	// A RawPath retaining /api no longer matches Path, so net/url discards it.
	out.URL.RawPath = strings.TrimPrefix(r.URL.EscapedPath(), "/api")
	// Attach the server-held bearer; never forward the browser's session cookie
	// or any client-supplied Authorization to the gateway.
	out.Header.Del("Cookie")
	out.Header.Set("Authorization", "Bearer "+sess.AccessToken)
	// SIGN IT, or the gateway refuses before it ever authenticates (#777). The
	// signature covers the body, which readProxyBody bounded before allocating.
	// Put the bytes back because r.Clone shares the consumed ReadCloser.
	//
	// The PATH SIGNED IS THE GATEWAY'S, not this server's: the middleware hashes
	// the path it receives, so signing "/api/v1/..." produces a valid signature
	// for a request nobody makes.
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	if len(s.signingSecret) > 0 {
		gatewaysig.SignRequest(out, s.signingSecret, body)
	}
	s.proxy.ServeHTTP(w, out)
}

// readProxyBody enforces the public request-body contract before the singleton
// BFF buffers or signs bytes. Content-Length rejects known oversize requests
// without touching their streams; MaxBytesReader covers chunked bodies and
// callers that understate the length.
func (s *Server) readProxyBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.ContentLength > requestbody.MaxBytes {
		_ = r.Body.Close()
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return nil, false
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestbody.MaxBytes))
	_ = r.Body.Close()
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return nil, false
		}
		s.fail(w, http.StatusBadRequest, "read body failed", err)
		return nil, false
	}
	return body, true
}

// THE REPRODUCTION IS GONE (#781). It used to live here, and its own comment
// stated the reason: "a reproduction rather than a shared call because the
// gateway's verifier lives inside that service; the two are one fact stated
// twice, and #777 exists because the third caller in a row stated only half of
// it." The premise was true and is what changed — internal/gatewaysig now holds
// the canonicalization AND the gateway's verifier is built from it, so a shared
// call is no longer half a fact.

// currentSession resolves the session from the request cookie.
func (s *Server) currentSession(w http.ResponseWriter, r *http.Request) (session.Session, bool) {
	v, ok, err := s.sessions.Get(r.Context(), sessionID(r))
	if err != nil {
		s.sessionError(w, err)
		return session.Session{}, false
	}
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "not authenticated"})
	}
	return v, ok
}
func sessionID(r *http.Request) string {
	c, e := r.Cookie(sessionCookie)
	if e != nil {
		return ""
	}
	return c.Value
}
func (s *Server) sessionError(w http.ResponseWriter, e error) {
	status, msg := 503, "Session service unavailable. Try again shortly."
	switch {
	case errors.Is(e, session.ErrCapacity):
		status, msg = 429, "Session capacity reached. Sign out another session or try again later."
	case errors.Is(e, session.ErrNotFound):
		status, msg = 404, "Session not found. Refresh the session list."
	case errors.Is(e, session.ErrMissing):
		status, msg = 401, "Session no longer active. Sign in again."
	case errors.Is(e, session.ErrInvalid):
		status, msg = 400, "Session could not be established."
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, id string, expiry time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) fail(w http.ResponseWriter, status int, msg string, err error) {
	if err != nil {
		s.logger.Warn("bff request failed", "msg", msg, "err", err)
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

// newProxy builds a reverse proxy to the gateway. The default director joins the
// target path with the (already prefix-stripped) request path and preserves the
// query, which is exactly the transcoding the gateway expects.
func newProxy(target *url.URL) *httputil.ReverseProxy {
	p := httputil.NewSingleHostReverseProxy(target)
	// Route errors (gateway down) return 502 to the browser rather than a panic.
	p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream unavailable"})
	}
	return p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
