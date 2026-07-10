package deviceauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ssoStub is a minimal stand-in for the Eighred SSO device-flow surface: the
// OIDC discovery document plus the device_authorization and token endpoints.
// The token endpoint returns the next scripted response on each poll.
type ssoStub struct {
	srv *httptest.Server

	mu            sync.Mutex
	tokenReplies  []tokenReply // consumed front-to-back on each POST /token
	tokenRequests []map[string]string
	deviceForm    map[string]string
	interval      int  // advertised poll interval (seconds); 0 ⇒ omit
	expiresIn     int  // advertised device_code lifetime; 0 ⇒ omit
	issuerOK      bool // when false, discovery advertises a mismatched issuer
}

type tokenReply struct {
	status int
	body   map[string]any
}

func newSSOStub(t *testing.T) *ssoStub {
	t.Helper()
	s := &ssoStub{interval: 5, expiresIn: 600, issuerOK: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := s.srv.URL
		if !s.issuerOK {
			iss = "https://someone-else.example"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                        iss,
			"token_endpoint":                s.srv.URL + "/token",
			"device_authorization_endpoint": s.srv.URL + "/device_authorization",
		})
	})
	mux.HandleFunc("/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.deviceForm = formMap(r)
		s.mu.Unlock()
		resp := map[string]any{
			"device_code":               "DEV-123",
			"user_code":                 "WXYZ-1234",
			"verification_uri":          s.srv.URL + "/device",
			"verification_uri_complete": s.srv.URL + "/device?user_code=WXYZ-1234",
		}
		if s.interval != 0 {
			resp["interval"] = s.interval
		}
		if s.expiresIn != 0 {
			resp["expires_in"] = s.expiresIn
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.tokenRequests = append(s.tokenRequests, formMap(r))
		var reply tokenReply
		if len(s.tokenReplies) > 0 {
			reply = s.tokenReplies[0]
			s.tokenReplies = s.tokenReplies[1:]
		} else {
			reply = tokenReply{http.StatusBadRequest, map[string]any{"error": "authorization_pending"}}
		}
		s.mu.Unlock()
		writeJSON(w, reply.status, reply.body)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func formMap(r *http.Request) map[string]string {
	m := make(map[string]string, len(r.PostForm))
	for k := range r.PostForm {
		m[k] = r.PostForm.Get(k)
	}
	return m
}

func pending() tokenReply {
	return tokenReply{http.StatusBadRequest, map[string]any{"error": "authorization_pending"}}
}

func approved() tokenReply {
	return tokenReply{http.StatusOK, map[string]any{
		"access_token":  "ACCESS-TOK",
		"refresh_token": "REFRESH-TOK",
		"id_token":      "ID-TOK",
		"token_type":    "Bearer",
		"expires_in":    3600,
	}}
}

// recordingWaiter fires immediately and records the durations it was asked to
// wait, so a test can assert the poll cadence (interval + slow_down back-off)
// without spending real time.
type recordingWaiter struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (rw *recordingWaiter) after(d time.Duration) <-chan time.Time {
	rw.mu.Lock()
	rw.waits = append(rw.waits, d)
	rw.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch
}

func newClient(t *testing.T, s *ssoStub, w *recordingWaiter) *Client {
	t.Helper()
	cfg := Config{
		Issuer:   s.srv.URL,
		ClientID: "kanz-cli",
		Scope:    "openid profile",
	}
	if w != nil {
		cfg.after = w.after
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestAuthorize_HappyPath(t *testing.T) {
	s := newSSOStub(t)
	s.tokenReplies = []tokenReply{pending(), pending(), approved()}
	c := newClient(t, s, &recordingWaiter{})

	var prompt Prompt
	shown := 0
	tok, err := c.Authorize(context.Background(), func(p Prompt) { prompt = p; shown++ })
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if shown != 1 {
		t.Fatalf("show called %d times, want 1", shown)
	}
	if prompt.UserCode != "WXYZ-1234" || prompt.VerificationURI == "" {
		t.Fatalf("bad prompt: %+v", prompt)
	}
	if tok.AccessToken != "ACCESS-TOK" || tok.RefreshToken != "REFRESH-TOK" || tok.IDToken != "ID-TOK" {
		t.Fatalf("bad token: %+v", tok)
	}
	if tok.TokenType != "Bearer" {
		t.Fatalf("token_type = %q", tok.TokenType)
	}
	if tok.Expiry.IsZero() {
		t.Fatal("expiry not set")
	}
	// The device request carried the client_id and scope.
	if s.deviceForm["client_id"] != "kanz-cli" || s.deviceForm["scope"] != "openid profile" {
		t.Fatalf("device form = %v", s.deviceForm)
	}
	// Every token poll carried the device_code grant + device_code + client_id.
	if len(s.tokenRequests) != 3 {
		t.Fatalf("token polled %d times, want 3", len(s.tokenRequests))
	}
	last := s.tokenRequests[len(s.tokenRequests)-1]
	if last["grant_type"] != deviceGrantType || last["device_code"] != "DEV-123" || last["client_id"] != "kanz-cli" {
		t.Fatalf("token form = %v", last)
	}
}

func TestAuthorize_SlowDownBacksOff(t *testing.T) {
	s := newSSOStub(t)
	s.interval = 5
	s.tokenReplies = []tokenReply{
		{http.StatusBadRequest, map[string]any{"error": "slow_down"}},
		approved(),
	}
	w := &recordingWaiter{}
	c := newClient(t, s, w)

	if _, err := c.Authorize(context.Background(), nil); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	// First wait at the advertised 5s; after slow_down the second wait is +5s.
	if len(w.waits) != 2 {
		t.Fatalf("waits = %v, want 2", w.waits)
	}
	if w.waits[0] != 5*time.Second || w.waits[1] != 10*time.Second {
		t.Fatalf("cadence = %v, want [5s 10s]", w.waits)
	}
}

func TestAuthorize_DefaultIntervalWhenUnspecified(t *testing.T) {
	s := newSSOStub(t)
	s.interval = 0 // omit interval from the device response
	s.tokenReplies = []tokenReply{approved()}
	w := &recordingWaiter{}
	c := newClient(t, s, w)

	if _, err := c.Authorize(context.Background(), nil); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if len(w.waits) != 1 || w.waits[0] != 5*time.Second {
		t.Fatalf("waits = %v, want [5s]", w.waits)
	}
}

func TestAuthorize_Denied(t *testing.T) {
	s := newSSOStub(t)
	s.tokenReplies = []tokenReply{pending(), {http.StatusBadRequest, map[string]any{"error": "access_denied"}}}
	c := newClient(t, s, &recordingWaiter{})

	_, err := c.Authorize(context.Background(), nil)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("err = %v, want ErrAccessDenied", err)
	}
}

func TestAuthorize_ServerExpired(t *testing.T) {
	s := newSSOStub(t)
	s.tokenReplies = []tokenReply{{http.StatusBadRequest, map[string]any{"error": "expired_token"}}}
	c := newClient(t, s, &recordingWaiter{})

	_, err := c.Authorize(context.Background(), nil)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestAuthorize_ClientSideExpiryDeadline(t *testing.T) {
	s := newSSOStub(t)
	s.expiresIn = 30
	// The server keeps saying pending; the client must stop once its own clock
	// passes the device_code deadline rather than poll forever.
	s.tokenReplies = []tokenReply{pending(), pending(), pending()}

	var mu sync.Mutex
	now := time.Now()
	w := &recordingWaiter{}
	cfg := Config{
		Issuer:   s.srv.URL,
		ClientID: "kanz-cli",
		after:    w.after,
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(20 * time.Second) // each clock read advances 20s
			return now
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Authorize(context.Background(), nil); !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestAuthorize_UnexpectedErrorIsTerminal(t *testing.T) {
	s := newSSOStub(t)
	s.tokenReplies = []tokenReply{{http.StatusBadRequest, map[string]any{"error": "invalid_client", "error_description": "unknown client"}}}
	c := newClient(t, s, &recordingWaiter{})

	_, err := c.Authorize(context.Background(), nil)
	if err == nil || errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want a plain terminal error", err)
	}
}

func TestAuthorize_ServerFaultSurfaces(t *testing.T) {
	s := newSSOStub(t)
	s.tokenReplies = []tokenReply{{http.StatusInternalServerError, map[string]any{}}}
	c := newClient(t, s, &recordingWaiter{})

	if _, err := c.Authorize(context.Background(), nil); err == nil {
		t.Fatal("expected an error on 5xx from the token endpoint")
	}
}

func TestAuthorize_DiscoveryIssuerMismatch(t *testing.T) {
	s := newSSOStub(t)
	s.issuerOK = false
	c := newClient(t, s, &recordingWaiter{})

	if _, err := c.Authorize(context.Background(), nil); err == nil {
		t.Fatal("expected an error on issuer mismatch")
	}
}

func TestAuthorize_ContextCancel(t *testing.T) {
	s := newSSOStub(t)
	// Never approves; a blocking waiter forces the poll to sit on ctx.Done.
	blocking := func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	cfg := Config{Issuer: s.srv.URL, ClientID: "kanz-cli", after: blocking}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := c.Authorize(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := New(Config{ClientID: "x"}); err == nil {
		t.Fatal("expected error for missing issuer")
	}
	if _, err := New(Config{Issuer: "https://iss"}); err == nil {
		t.Fatal("expected error for missing client_id")
	}
}

// sanity: the stub's discovery doc is valid JSON the resolver accepts.
func TestResolveEndpoints_Caches(t *testing.T) {
	s := newSSOStub(t)
	c := newClient(t, s, &recordingWaiter{})
	if err := c.resolveEndpoints(context.Background()); err != nil {
		t.Fatalf("resolveEndpoints: %v", err)
	}
	if c.tokenEndpoint == "" || c.deviceEndpoint == "" {
		t.Fatal("endpoints not resolved")
	}
	// Tear down the server, then re-resolve: a cached client must not touch the
	// network, so the second call succeeds with the endpoints intact.
	s.srv.Close()
	if err := c.resolveEndpoints(context.Background()); err != nil {
		t.Fatalf("resolveEndpoints (cached, server down): %v", err)
	}
	if c.tokenEndpoint == "" || c.deviceEndpoint == "" {
		t.Fatal("cached endpoints lost")
	}
}
