// Package session holds the BFF's server-side browser sessions and in-flight
// login transactions. The browser only ever holds an opaque, httpOnly session
// id; the token itself never leaves the server (the core BFF security property
// — a token in browser-reachable JS is an exfiltration target). The in-memory
// store is the single-replica default; a shared backend (Redis, the PARITY-05
// stance) plugs in behind the same Manager for horizontal scale.
package session

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// PendingTTL bounds how long a started login (its PKCE verifier) is redeemable
// before the user must restart — long enough to authenticate, short enough to
// bound the window an intercepted state value is useful.
const PendingTTL = 10 * time.Minute

// Session is one authenticated browser session's server-side state.
type Session struct {
	AccessToken  string
	RefreshToken string
	// Subject and Tenant are decoded from the id/access token for display and
	// logging; they are not an authorization decision (the gateway re-validates
	// the token on every proxied call).
	Subject string
	Tenant  string
	Expiry  time.Time
}

type entry[V any] struct {
	val V
	exp time.Time
}

type ttlStore[V any] struct {
	mu  sync.Mutex
	m   map[string]entry[V]
	now func() time.Time
}

func newTTLStore[V any](now func() time.Time) *ttlStore[V] {
	return &ttlStore[V]{m: make(map[string]entry[V]), now: now}
}

func (s *ttlStore[V]) put(key string, v V, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = entry[V]{val: v, exp: s.now().Add(ttl)}
}

func (s *ttlStore[V]) get(key string) (V, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok || s.now().After(e.exp) {
		if ok {
			delete(s.m, key)
		}
		var zero V
		return zero, false
	}
	return e.val, true
}

// take returns the value and removes it — one-time redemption.
func (s *ttlStore[V]) take(key string) (V, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if ok {
		delete(s.m, key)
	}
	if !ok || s.now().After(e.exp) {
		var zero V
		return zero, false
	}
	return e.val, true
}

func (s *ttlStore[V]) del(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

func (s *ttlStore[V]) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.m {
		if now.After(e.exp) {
			delete(s.m, k)
		}
	}
}

// Manager owns the authenticated sessions and the in-flight login
// transactions. Safe for concurrent use.
type Manager struct {
	sessions *ttlStore[Session]
	pending  *ttlStore[string] // state -> PKCE verifier
	ttl      time.Duration
	now      func() time.Time
}

// NewManager returns an in-memory Manager whose sessions live for ttl.
func NewManager(ttl time.Duration) *Manager { return newManager(ttl, time.Now) }

func newManager(ttl time.Duration, now func() time.Time) *Manager {
	return &Manager{
		sessions: newTTLStore[Session](now),
		pending:  newTTLStore[string](now),
		ttl:      ttl,
		now:      now,
	}
}

// Create stores an authenticated session and returns its opaque id. The
// session lives for min(ttl, until token expiry) so a session never outlives
// the token it holds.
func (m *Manager) Create(s Session) (string, error) {
	id, err := randomID()
	if err != nil {
		return "", err
	}
	ttl := m.ttl
	if !s.Expiry.IsZero() {
		if until := s.Expiry.Sub(m.now()); until < ttl {
			ttl = until
		}
	}
	if ttl <= 0 {
		return "", errors.New("session: token already expired")
	}
	m.sessions.put(id, s, ttl)
	return id, nil
}

// Get returns the session for id, or false when unknown/expired.
func (m *Manager) Get(id string) (Session, bool) { return m.sessions.get(id) }

// Delete ends a session (logout).
func (m *Manager) Delete(id string) { m.sessions.del(id) }

// PutPending records a started login's PKCE verifier keyed by its state value.
func (m *Manager) PutPending(state, verifier string) { m.pending.put(state, verifier, PendingTTL) }

// TakePending redeems and removes the verifier for state (one-time), so a
// replayed callback with the same state cannot exchange a second time.
func (m *Manager) TakePending(state string) (string, bool) { return m.pending.take(state) }

// Sweep drops expired sessions and pending logins. A caller runs it on a
// ticker so abandoned entries do not accumulate.
func (m *Manager) Sweep() {
	m.sessions.sweep()
	m.pending.sweep()
}

func randomID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
