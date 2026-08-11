// Package server is the web-BFF's HTTP surface (PS-02a): the browser login
// endpoints (OIDC authorization-code + PKCE against Eighred SSO) and a
// session-authenticated reverse proxy to the api-gateway /v1 edge. The browser
// holds only an opaque httpOnly session cookie; the BFF attaches the
// server-held bearer to each proxied call, which the gateway validates.
package server

import (
	"encoding/base64"
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

	"github.com/eighred/kanz/services/web-bff/internal/clientip"
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
	StaticDir     string
	OIDC          *oidc.Client
	Sessions      *session.Manager
	GatewayURL    string
	SecureCookies bool
	Logger        *slog.Logger
	Metrics       http.Handler
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
	secureCookies bool
	logger        *slog.Logger
	metrics       http.Handler
	mux           *http.ServeMux
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
		secureCookies: opts.SecureCookies,
		logger:        logger,
		metrics:       opts.Metrics,
		mux:           http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.readiness.Ready() {
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
	s.mux.HandleFunc("GET /auth/me", s.handleMe)
	// Everything under /api/ is proxied to the gateway as the session's caller.
	s.mux.HandleFunc("/api/", s.handleProxy)
	// The SPA last, on "/" — every route above is registered on a more specific
	// pattern, so ServeMux prefers them. Registered only when a build is
	// configured, so an API-only deployment 404s rather than serving nothing.
	if s.static != nil {
		s.mux.Handle("/", s.static)
	}
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
	s.sessions.PutPending(state, pkce.Verifier)
	authURL, err := s.oidc.AuthCodeURL(r.Context(), state, pkce.Challenge)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "cannot reach identity provider", err)
		return
	}
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
	verifier, ok := s.sessions.TakePending(state)
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
	sub, tenant := claimsOf(tok.IDToken)
	if sub == "" {
		sub, tenant = claimsOf(tok.AccessToken)
	}
	id, err := s.sessions.Create(session.Session{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Subject:      sub,
		Tenant:       tenant,
		Expiry:       tok.Expiry,
	})
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not start session", err)
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

	id, cerr := s.sessions.Create(session.Session{
		AccessToken: tok.Token,
		Subject:     tok.Subject,
		Tenant:      tok.Tenant,
		Expiry:      tok.Expires,
	})
	if cerr != nil {
		s.fail(w, http.StatusInternalServerError, "could not start session", cerr)
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
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(c.Value)
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"subject":    sess.Subject,
		"tenant":     sess.Tenant,
		"expires_at": sess.Expiry.Format(time.RFC3339),
	})
}

// handleProxy forwards /api/* to the gateway /v1/* as the session's caller.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
		return
	}
	out := r.Clone(r.Context())
	// Strip the /api prefix so /api/v1/ask reaches the gateway as /v1/ask.
	out.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
	// Attach the server-held bearer; never forward the browser's session cookie
	// or any client-supplied Authorization to the gateway.
	out.Header.Del("Cookie")
	out.Header.Set("Authorization", "Bearer "+sess.AccessToken)
	s.proxy.ServeHTTP(w, out)
}

// currentSession resolves the session from the request cookie.
func (s *Server) currentSession(r *http.Request) (session.Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session.Session{}, false
	}
	return s.sessions.Get(c.Value)
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

// claimsOf base64url-decodes a JWT payload WITHOUT verifying the signature —
// display/logging only. The gateway is the authority that verifies the token.
func claimsOf(jwt string) (subject, tenant string) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var c struct {
		Sub    string `json:"sub"`
		Tenant string `json:"tenant"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return "", ""
	}
	return c.Sub, c.Tenant
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
