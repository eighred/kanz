// Package session owns bounded server-side browser authority. Shared mode has
// no local read cache: a committed revoke applies at the next replica lookup.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const PendingTTL = 10 * time.Minute
const MaxPayload = 64 << 10

var ErrUnavailable = errors.New("session authority unavailable")
var ErrCapacity = errors.New("session capacity reached")
var ErrNotFound = errors.New("session not found")
var ErrMissing = errors.New("session no longer active")
var ErrInvalid = errors.New("invalid session")

type Limits struct{ Sessions, Pending, PerSubject int }

func DefaultLimits() Limits { return Limits{10000, 1000, 20} }
func (l Limits) Validate() error {
	if l.Sessions < 1 || l.Sessions > 100000 || l.Pending < 1 || l.Pending > 10000 || l.PerSubject < 1 || l.PerSubject > 100 || l.PerSubject > l.Sessions {
		return ErrInvalid
	}
	return nil
}

type Session struct {
	AccessToken  string
	RefreshToken string
	Subject      string
	Tenant       string
	Authority    string
	Expiry       time.Time
}
type Summary struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Current   bool      `json:"current"`
}
type stored struct {
	Session Session
	Created time.Time
}
type backend interface {
	put(context.Context, string, Session, string, bool) error
	get(context.Context, string) (Session, bool, error)
	del(context.Context, string) error
	list(context.Context, string) ([]Summary, error)
	revoke(context.Context, string, string) error
	pendingPut(context.Context, string, string) error
	pendingTake(context.Context, string) (string, bool, error)
	ping(context.Context) error
	sweep(context.Context) error
}
type Manager struct {
	store backend
	ttl   time.Duration
	now   func() time.Time
}

func NewManager(ttl time.Duration) *Manager { return newManager(ttl, time.Now) }
func newManager(ttl time.Duration, now func() time.Time) *Manager {
	m, err := NewBounded(ttl, DefaultLimits())
	if err != nil {
		return nil
	}
	m.now = now
	m.store.(*memory).now = now
	return m
}
func NewBounded(ttl time.Duration, l Limits) (*Manager, error) {
	if ttl <= 0 || ttl > 24*time.Hour || l.Validate() != nil {
		return nil, ErrInvalid
	}
	return &Manager{store: newMemory(l), ttl: ttl, now: time.Now}, nil
}
func (m *Manager) Create(ctx context.Context, s Session) (string, error) {
	return m.Replace(ctx, "", s, false)
}

// strict replacement cannot recreate a session after another replica revoked it.
func (m *Manager) Replace(ctx context.Context, previous string, s Session, strict bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if s.Authority == "" {
		s.Authority = "native"
	}
	if s.Subject == "" || s.Tenant == "" || len(s.Subject) > 256 || len(s.Tenant) > 128 || len(s.Authority) > 1024 {
		return "", ErrInvalid
	}
	now := m.now().UTC()
	expiry := now.Add(m.ttl)
	if !s.Expiry.IsZero() && s.Expiry.Before(expiry) {
		expiry = s.Expiry
	}
	if !expiry.After(now) {
		return "", ErrInvalid
	}
	s.Expiry = expiry.UTC().Truncate(time.Microsecond)
	raw, err := json.Marshal(s)
	if err != nil || len(raw) > MaxPayload {
		return "", ErrInvalid
	}
	id, err := randomID()
	if err != nil {
		return "", ErrUnavailable
	}
	if err = m.store.put(ctx, id, s, previous, strict); err != nil {
		return "", err
	}
	return id, nil
}
func (m *Manager) Get(ctx context.Context, id string) (Session, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if len(id) != 43 {
		return Session{}, false, nil
	}
	return m.store.get(ctx, id)
}
func (m *Manager) Delete(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if len(id) != 43 {
		return nil
	}
	return m.store.del(ctx, id)
}
func (m *Manager) List(ctx context.Context, id string) ([]Summary, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if len(id) != 43 {
		return nil, ErrMissing
	}
	return m.store.list(ctx, id)
}
func (m *Manager) Revoke(ctx context.Context, id, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if len(id) != 43 {
		return ErrMissing
	}
	if len(target) != 64 {
		return ErrNotFound
	}
	return m.store.revoke(ctx, id, target)
}
func (m *Manager) PutPending(ctx context.Context, state, verifier string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if len(state) > 128 || len(state) < 1 || len(verifier) > 128 || len(verifier) < 1 {
		return ErrInvalid
	}
	return m.store.pendingPut(ctx, state, verifier)
}
func (m *Manager) TakePending(ctx context.Context, state string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if len(state) > 128 || state == "" {
		return "", false, nil
	}
	return m.store.pendingTake(ctx, state)
}
func (m *Manager) Sweep(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return m.store.sweep(ctx)
}
func (m *Manager) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return m.store.ping(ctx)
}
func digest(id string) string { sum := sha256.Sum256([]byte(id)); return hex.EncodeToString(sum[:]) }
func sameOwner(a, b Session) bool {
	return a.Subject == b.Subject && a.Tenant == b.Tenant && a.Authority == b.Authority
}
func randomID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (m *Manager) Expiry(token time.Time) time.Time {
	v := m.now().UTC().Add(m.ttl)
	if !token.IsZero() && token.Before(v) {
		v = token
	}
	return v.Truncate(time.Microsecond)
}
