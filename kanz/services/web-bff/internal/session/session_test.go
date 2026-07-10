package session

import (
	"sync"
	"testing"
	"time"
)

// clock is a controllable time source for TTL tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestCreateGetDelete(t *testing.T) {
	m := NewManager(time.Hour)
	id, err := m.Create(Session{AccessToken: "tok", Subject: "u-1", Tenant: "acme"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, ok := m.Get(id)
	if !ok || got.AccessToken != "tok" || got.Subject != "u-1" {
		t.Fatalf("Get = %+v ok=%v", got, ok)
	}
	m.Delete(id)
	if _, ok := m.Get(id); ok {
		t.Fatal("session survived Delete")
	}
}

func TestSessionExpiresWithTTL(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(30*time.Minute, c.now)
	id, err := m.Create(Session{AccessToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	c.advance(29 * time.Minute)
	if _, ok := m.Get(id); !ok {
		t.Fatal("session expired early")
	}
	c.advance(2 * time.Minute) // now 31m > 30m ttl
	if _, ok := m.Get(id); ok {
		t.Fatal("session did not expire after TTL")
	}
}

func TestCreateClampsToTokenExpiry(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(time.Hour, c.now)
	// Token expires in 5 min, shorter than the 1h TTL — the session must not
	// outlive the token.
	id, err := m.Create(Session{AccessToken: "tok", Expiry: c.now().Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	c.advance(6 * time.Minute)
	if _, ok := m.Get(id); ok {
		t.Fatal("session outlived its token")
	}
}

func TestCreateRejectsExpiredToken(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(time.Hour, c.now)
	if _, err := m.Create(Session{AccessToken: "tok", Expiry: c.now().Add(-time.Minute)}); err == nil {
		t.Fatal("Create accepted an already-expired token")
	}
}

func TestPendingIsOneTime(t *testing.T) {
	m := NewManager(time.Hour)
	m.PutPending("state-1", "verifier-1")
	if v, ok := m.TakePending("state-1"); !ok || v != "verifier-1" {
		t.Fatalf("TakePending = %q ok=%v", v, ok)
	}
	// A second take (a replayed callback) must miss.
	if _, ok := m.TakePending("state-1"); ok {
		t.Fatal("pending login was redeemable twice — replay not prevented")
	}
}

func TestPendingExpires(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(time.Hour, c.now)
	m.PutPending("s", "v")
	c.advance(PendingTTL + time.Second)
	if _, ok := m.TakePending("s"); ok {
		t.Fatal("expired pending login was still redeemable")
	}
}

func TestSweepDropsExpired(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(10*time.Minute, c.now)
	id, _ := m.Create(Session{AccessToken: "tok"})
	m.PutPending("s", "v")
	c.advance(2 * time.Hour)
	m.Sweep()
	// After sweep the underlying maps are empty; Get/Take still report gone.
	if _, ok := m.Get(id); ok {
		t.Error("session not swept")
	}
	if _, ok := m.TakePending("s"); ok {
		t.Error("pending not swept")
	}
}
