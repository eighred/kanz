package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

// Auth failures. They are deliberately coarse — an authentication boundary must
// not leak which check failed to an unauthenticated caller.
var (
	ErrUnauthorized = errors.New("ingest: unauthorized webhook")
	ErrReplayed     = errors.New("ingest: replayed or duplicate signal")
)

// SecretStore resolves the per-strategy HMAC secret. The claimed strategy_id
// (from the body) selects the candidate secret; the HMAC over the raw body then
// proves the sender holds it, so a passing signature authenticates the strategy.
type SecretStore interface {
	SecretFor(strategyID string) (secret string, ok bool)
}

// StaticSecrets is an in-memory SecretStore (the test/single-deploy default;
// production loads the map from Vault/CSI at the composition root).
type StaticSecrets map[string]string

func (s StaticSecrets) SecretFor(id string) (string, bool) { v, ok := s[id]; return v, ok }

// Authenticator enforces the webhook perimeter: IP allowlist, HMAC-over-raw-body
// with the strategy's shared secret, and a nonce replay window. The server-to-
// server HMAC is the perimeter boundary (the OMS's forged-issuer verifier is for
// end-user commands, not this path).
type Authenticator struct {
	secrets   SecretStore
	allowlist []*net.IPNet // empty ⇒ allow any source (dev)
	replay    *replayCache
	now       func() time.Time
}

// NewAuthenticator builds the authenticator. allowlist entries are CIDRs; an
// empty allowlist accepts any source IP. window bounds the replay cache.
func NewAuthenticator(secrets SecretStore, allowlist []*net.IPNet, window time.Duration, now func() time.Time) *Authenticator {
	if now == nil {
		now = time.Now
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	return &Authenticator{secrets: secrets, allowlist: allowlist, replay: newReplayCache(now), now: now}
}

// Authenticate verifies the source IP, the HMAC signature over rawBody, and that
// (strategyID, nonce) has not been seen. It returns ErrUnauthorized for an
// IP/secret/signature failure and ErrReplayed for a duplicate.
func (a *Authenticator) Authenticate(rawBody []byte, remoteIP net.IP, sigHeader, strategyID, nonce string, window time.Duration) error {
	if !a.ipAllowed(remoteIP) {
		return ErrUnauthorized
	}
	secret, ok := a.secrets.SecretFor(strategyID)
	if !ok {
		return ErrUnauthorized
	}
	if !validHMAC(secret, rawBody, sigHeader) {
		return ErrUnauthorized
	}
	// Replay defense is LAST: only a fully-authenticated request consumes a nonce,
	// so a forged request can neither replay nor poison the cache.
	if !a.replay.admit(strategyID+":"+nonce, window) {
		return ErrReplayed
	}
	return nil
}

func (a *Authenticator) ipAllowed(ip net.IP) bool {
	if len(a.allowlist) == 0 {
		return true
	}
	for _, n := range a.allowlist {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// validHMAC checks sigHeader == hex(HMAC-SHA256(secret, body)) in constant time.
// The header may carry a "sha256=" prefix (the common convention).
func validHMAC(secret string, body []byte, sigHeader string) bool {
	sigHeader = strings.TrimSpace(strings.TrimPrefix(sigHeader, "sha256="))
	provided, err := hex.DecodeString(sigHeader)
	if err != nil || len(provided) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), provided)
}

// replayCache is a TTL set of admitted (strategy, nonce) keys. admit records and
// returns true the first time; a repeat within the window returns false.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

func newReplayCache(now func() time.Time) *replayCache {
	return &replayCache{seen: make(map[string]time.Time), now: now}
}

func (c *replayCache) admit(key string, window time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	// Opportunistic sweep so the cache never grows unbounded under a nonce storm.
	for k, exp := range c.seen {
		if now.After(exp) {
			delete(c.seen, k)
		}
	}
	if exp, ok := c.seen[key]; ok && !now.After(exp) {
		return false
	}
	c.seen[key] = now.Add(window)
	return true
}
