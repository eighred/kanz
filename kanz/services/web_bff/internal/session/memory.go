package session

import (
	"context"
	"sort"
	"sync"
	"time"
)

type pending struct {
	value  string
	expiry time.Time
}
type memory struct {
	mu       sync.Mutex
	sessions map[string]stored
	pending  map[string]pending
	limits   Limits
	now      func() time.Time
}

func newMemory(l Limits) *memory {
	return &memory{sessions: map[string]stored{}, pending: map[string]pending{}, limits: l, now: time.Now}
}
func (m *memory) prune() {
	now := m.now()
	for k, v := range m.sessions {
		if !now.Before(v.Session.Expiry) {
			delete(m.sessions, k)
		}
	}
	for k, v := range m.pending {
		if !now.Before(v.expiry) {
			delete(m.pending, k)
		}
	}
}
func (m *memory) put(ctx context.Context, id string, s Session, old string, strict bool) error {
	if err := ctx.Err(); err != nil {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	key := digest(old)
	previous, exists := m.sessions[key]
	if strict && (!exists || !sameOwner(previous.Session, s)) {
		return ErrMissing
	}
	total, count := len(m.sessions), 0
	for _, v := range m.sessions {
		if sameOwner(v.Session, s) {
			count++
		}
	}
	if exists {
		total--
		if sameOwner(previous.Session, s) {
			count--
		}
	}
	if total >= m.limits.Sessions || count >= m.limits.PerSubject {
		return ErrCapacity
	}
	delete(m.sessions, key)
	m.sessions[digest(id)] = stored{s, m.now().UTC()}
	return nil
}
func (m *memory) get(ctx context.Context, id string) (Session, bool, error) {
	if ctx.Err() != nil {
		return Session{}, false, ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.sessions[digest(id)]
	if ok && !m.now().Before(v.Session.Expiry) {
		delete(m.sessions, digest(id))
		ok = false
	}
	if !ok {
		return Session{}, false, nil
	}
	return v.Session, true, nil
}
func (m *memory) del(ctx context.Context, id string) error {
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, digest(id))
	return nil
}
func (m *memory) list(ctx context.Context, id string) ([]Summary, error) {
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	key := digest(id)
	actor, ok := m.sessions[key]
	if !ok {
		return nil, ErrMissing
	}
	out := []Summary{}
	for k, v := range m.sessions {
		if sameOwner(v.Session, actor.Session) {
			out = append(out, Summary{k, v.Created, v.Session.Expiry, k == key})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (m *memory) revoke(ctx context.Context, id, target string) error {
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	a, ok := m.sessions[digest(id)]
	v, found := m.sessions[target]
	if !ok {
		return ErrMissing
	}
	if !found || !sameOwner(a.Session, v.Session) {
		return ErrNotFound
	}
	delete(m.sessions, target)
	return nil
}
func (m *memory) pendingPut(ctx context.Context, key, value string) error {
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	key = digest(key)
	if _, ok := m.pending[key]; ok {
		return ErrInvalid
	}
	if len(m.pending) >= m.limits.Pending {
		return ErrCapacity
	}
	m.pending[key] = pending{value, m.now().Add(PendingTTL)}
	return nil
}
func (m *memory) pendingTake(ctx context.Context, key string) (string, bool, error) {
	if ctx.Err() != nil {
		return "", false, ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key = digest(key)
	v, ok := m.pending[key]
	delete(m.pending, key)
	if !ok || !m.now().Before(v.expiry) {
		return "", false, nil
	}
	return v.value, true, nil
}
func (m *memory) ping(ctx context.Context) error {
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	return nil
}
func (m *memory) sweep(ctx context.Context) error {
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	return nil
}
