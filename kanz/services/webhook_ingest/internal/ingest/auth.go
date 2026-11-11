package ingest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strings"
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
	nonces    NonceStore
	now       func() time.Time
}

// AuthOption customizes an Authenticator.
type AuthOption func(*Authenticator)

// WithNonceStore sets the store the replay defence claims nonces in.
//
// The DEFAULT is MemoryNonces, which is correct for exactly ONE replica: two pods are
// two maps, so a re-delivered alert landing on the other pod is admitted a SECOND time
// and fans out a SECOND set of orders. Pass RedisNonces and the defence spans every
// replica; that is what lets webhook-ingest — the one service the internet talks to —
// run more than one pod (EXEC-M17).
//
// THAT SECOND FAN-OUT DOES NOT REACH A VENUE, and this comment used to claim the
// opposite — "a fresh claim mints a fresh signal_id and therefore fresh order_ids"
// (#820). It does not: signal_id is DeterministicID(strategy, nonce) and order_id is
// DeterministicID(signal_id, venue), so a redelivery re-derives the SAME ids and the
// OMS admission gate collapses it — ON CONFLICT (tenant_id, order_id) DO NOTHING, ack,
// announce nothing, do not route.
//
// What this store buys is therefore refusal at the PERIMETER rather than deep in the
// OMS: a replay never traverses the pipeline, and it is the only layer that can tell a
// replay apart from a legitimate redelivery losing an admission race. See
// config.RedisURL for the full statement.
//
// The composition root REFUSES to run on the in-process default unless the deployment
// says so out loud (WEBHOOK_INGEST_ALLOW_INPROCESS_NONCE): a per-pod replay defence
// reachable by forgetting to configure Redis looks exactly like a correct one.
func WithNonceStore(s NonceStore) AuthOption {
	return func(a *Authenticator) { a.nonces = s }
}

// NewAuthenticator builds the authenticator. allowlist entries are CIDRs; an
// empty allowlist accepts any source IP. window bounds the replay cache.
func NewAuthenticator(secrets SecretStore, allowlist []*net.IPNet, window time.Duration, now func() time.Time, opts ...AuthOption) *Authenticator {
	if now == nil {
		now = time.Now
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	a := &Authenticator{secrets: secrets, allowlist: allowlist, now: now}
	for _, opt := range opts {
		opt(a)
	}
	if a.nonces == nil {
		a.nonces = NewMemoryNonces(window, maxInProcessNonces)
	}
	return a
}

// maxInProcessNonces bounds the default in-process nonce window so a nonce storm
// cannot exhaust memory. Only reached on a single-replica deployment.
const maxInProcessNonces = 100_000

// nonceKey namespaces a nonce by the strategy that signed it, so two strategies
// cannot collide (or evict each other) on the same nonce value.
func nonceKey(strategyID, nonce string) string { return strategyID + ":" + nonce }

// Authenticate verifies the source IP, the HMAC signature over rawBody, and CLAIMS
// (strategyID, nonce) for this delivery. It returns ErrUnauthorized for an
// IP/secret/signature failure and ErrReplayed for a duplicate.
//
// The claim is a LEASE, not a consumption: the caller MUST later CommitNonce (the
// alert was acted on, or deliberately refused — hold it for the full window) or
// ReleaseNonce (nothing happened — let a redelivery retry). Claiming and never
// releasing is what turned a broker blip into a permanently-lost trading signal.
func (a *Authenticator) Authenticate(ctx context.Context, rawBody []byte, remoteIP net.IP, sigHeader, strategyID, nonce string) error {
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
	// Replay defense is LAST: only a fully-authenticated request claims a nonce,
	// so a forged request can neither replay nor poison the cache.
	//
	// FAIL CLOSED. If the store cannot be reached we do not know whether this alert has
	// already traded, and "I cannot tell" must never resolve to "trade it" on the path
	// that submits orders to a live exchange. The caller answers 503 — a retryable
	// outage — rather than 409, which would tell TradingView the alert was a duplicate
	// and lose a live signal to a Redis blink.
	ok, err := a.nonces.Claim(ctx, nonceKey(strategyID, nonce))
	if err != nil {
		return err // ErrNonceStoreUnavailable
	}
	if !ok {
		return ErrReplayed
	}
	return nil
}

// CommitNonce holds the nonce for the full replay window: this alert has been DECIDED
// — emitted, or deliberately refused. A redelivery of it is a replay.
func (a *Authenticator) CommitNonce(ctx context.Context, strategyID, nonce string) {
	a.nonces.Commit(ctx, nonceKey(strategyID, nonce))
}

// ReleaseNonce frees the nonce: nothing acted on this alert (the broker was down, the
// size could not be resolved), so a redelivery MUST be free to retry it. Without this,
// a transient failure burns the nonce and the trading signal is lost for good.
func (a *Authenticator) ReleaseNonce(ctx context.Context, strategyID, nonce string) {
	a.nonces.Release(ctx, nonceKey(strategyID, nonce))
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

// The replay cache used to live here as a per-process map with a check-then-act
// admit(). It is now a NonceStore (see nonce.go) — the SAME three-phase lease the bus
// consumers use (EXEC-M7b), so the atomic claim, the lease expiry for a pod that dies
// mid-signal, and the release-on-failure retry semantics are one implementation, not
// two. MemoryNonces is that map; RedisNonces makes it cross-pod.
