// Package server is the web-BFF's HTTP surface (PS-02a): the browser login
// endpoints (OIDC authorization-code + PKCE against Eighred SSO) and a
// session-authenticated reverse proxy to the api-gateway /v1 edge. The browser
// holds only an opaque httpOnly session cookie; the BFF attaches the
// server-held bearer to each proxied call, which the gateway validates.
package server

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

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
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		readiness:     readiness,
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
	s.mux.HandleFunc("GET /auth/login", s.handleLogin)
	s.mux.HandleFunc("GET /auth/callback", s.handleCallback)
	s.mux.HandleFunc("POST /auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /auth/me", s.handleMe)
	// Everything under /api/ is proxied to the gateway as the session's caller.
	s.mux.HandleFunc("/api/", s.handleProxy)
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
