package revocation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ErrRevoked is returned for a token minted before its subject's revocation
// mark. The token itself verifies perfectly — the account behind it was
// disabled after the token was issued — so a caller maps this to 401, exactly
// as it would a bad signature. The caller is told nothing more: "your account
// was disabled at 14:03" is a fact about someone else's operational response
// that a holder of a stolen token has no business learning.
var ErrRevoked = errors.New("revocation: token predates this subject's revocation")

// ErrUnusable is returned when the cache CANNOT ANSWER — it has never fetched
// the feed, or its newest successful fetch is older than MaxAge.
//
// IT IS NOT ErrRevoked, AND THE DISTINCTION IS THE WHOLE CONTROL. A caller that
// collapsed the two would answer 401 during an identity outage, which the web
// client reads as "sign in again" and acts on by destroying the session — for
// every user at once, against a login service that is by assumption down. This
// is a 503: the credential was never judged, and that is the server's fault.
//
// FAILING CLOSED IS DELIBERATE AND IT IS BOUNDED. A cache that kept answering
// "not revoked" from a list it can no longer refresh hands an attacker who can
// keep identity unreachable an unlimited extension on a disabled account — the
// exact control this package exists to provide. Past MaxAge it stops answering
// instead. The same trade, with the same shape, is made one layer out by
// pkg/auth.ErrKeysStale for the SIGNING KEY axis; this is the per-subject one.
var ErrUnusable = errors.New("revocation: no usable revocation feed — the gateway cannot tell whether this subject was disabled")

const (
	// DefaultRefreshInterval is how often the feed is refetched, and therefore
	// the ORDINARY bound on how long a disabled account's outstanding token
	// keeps working: one interval plus one fetch.
	//
	// 30s is chosen against what the fetch costs, which is one small GET
	// against a service the gateway already polls nothing else from. The
	// alternative shape — identity pushing a FACT onto the bus — would be
	// faster, and it is not free: identity has no bus.Producer, no outbox and
	// no tenancy permissions block, and its composition root argues explicitly
	// against giving the credential authority a NATS connection
	// (services/identity/cmd/identity/main.go). See the PR for #532.
	DefaultRefreshInterval = 30 * time.Second

	// DefaultMaxAge is the CEILING on how long a cached feed may still be
	// answered from. Past it, Check refuses everything with ErrUnusable.
	//
	// 15 MINUTES IS A TRADE AND BOTH SIDES ARE REAL. Shorter, and a brief
	// identity blip becomes a trading outage on the platform's sole ingress for
	// orders — the failure this estate would notice first, and the one that is
	// far more likely than a revocation landing inside the same window. Longer,
	// and the window an attacker gets by holding identity down stops being
	// something anybody would call bounded. It is deliberately the same number
	// pkg/auth.OIDCConfig.KeyGracePeriod settled on for the adjacent axis, so an
	// operator has one figure to remember rather than two.
	DefaultMaxAge = 15 * time.Minute

	// maxFeedBytes caps the response body. Exceeded, the fetch FAILS rather
	// than truncating: a denylist cut short admits whoever fell off the end,
	// and it would do it silently. A feed this large is a bug in identity, and
	// the gateway going stale (and eventually refusing, loudly, with ErrUnusable)
	// is the correct response to a control it cannot read in full.
	maxFeedBytes = 8 << 20
)

// Config configures a Cache.
type Config struct {
	// URL is the revocation feed identity serves. Required.
	URL string
	// RefreshInterval is how often Run refetches (default
	// DefaultRefreshInterval).
	RefreshInterval time.Duration
	// MaxAge is the ceiling past which the cache refuses to answer (default
	// DefaultMaxAge). It MUST exceed RefreshInterval.
	MaxAge time.Duration
	// HTTPClient fetches the feed (default: a 10s client).
	HTTPClient *http.Client
	// Now is the clock, injectable so staleness is testable without sleeping.
	Now func() time.Time
}

// Cache holds the newest revocation feed the gateway managed to fetch, and
// answers whether one verified token is still good.
type Cache struct {
	url     string
	refresh time.Duration
	maxAge  time.Duration
	httpc   *http.Client
	now     func() time.Time

	mu sync.RWMutex
	// marks maps subject hash -> NotBefore. Rebuilt whole on every successful
	// fetch, because the feed is a snapshot: merging would keep an entry
	// identity has deliberately dropped.
	marks map[string]time.Time
	// fetchedAt is the zero time until the FIRST successful fetch. That zero is
	// what makes a cold pod refuse instead of admitting everybody — see Check.
	fetchedAt time.Time
}

// New builds a cache. It performs no I/O; call Prime or Refresh for that.
func New(cfg Config) (*Cache, error) {
	if cfg.URL == "" {
		return nil, errors.New("revocation: feed URL required — a gateway with no feed cannot tell a " +
			"disabled account from an active one, and must not be constructed as if it could")
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = DefaultRefreshInterval
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = DefaultMaxAge
	}
	// A ceiling at or below the refresh interval is a cache that is stale before
	// its own next scheduled fetch: every request between the ceiling and the
	// refresh is refused, forever, on a healthy system. Refuse at construction
	// rather than discover it as an authentication outage. Same relation, same
	// reason, as pkg/auth's MaxKeyAge vs MinRefreshInterval.
	if cfg.MaxAge <= cfg.RefreshInterval {
		return nil, fmt.Errorf("revocation: MaxAge (%s) must exceed RefreshInterval (%s) — "+
			"a ceiling below the refresh cadence expires the feed before the next fetch is due, "+
			"so a healthy system would refuse every request", cfg.MaxAge, cfg.RefreshInterval)
	}
	httpc := cfg.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: 10 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Cache{url: cfg.URL, refresh: cfg.RefreshInterval, maxAge: cfg.MaxAge, httpc: httpc, now: now}, nil
}

// Refresh fetches the feed once and replaces the cached snapshot on success.
// On failure the previous snapshot is KEPT — it is still within MaxAge and
// still the best answer available; Check is what decides when it stops being.
func (c *Cache) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("revocation: build request: %w", err)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("revocation: fetch %s: %w", c.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revocation: fetch %s: status %d", c.url, resp.StatusCode)
	}
	// LimitReader at maxFeedBytes+1 so an oversized body is DETECTED rather than
	// silently cut to the limit and parsed as if complete.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return fmt.Errorf("revocation: read %s: %w", c.url, err)
	}
	if len(body) > maxFeedBytes {
		return fmt.Errorf("revocation: feed at %s exceeds %d bytes — refusing a partial denylist",
			c.url, maxFeedBytes)
	}
	var feed Feed
	if err := json.Unmarshal(body, &feed); err != nil {
		return fmt.Errorf("revocation: decode %s: %w", c.url, err)
	}
	// THE BODY MUST SAY WHAT IT IS. Every field below is optional to the
	// decoder, so any 200 carrying a JSON object would otherwise land here as a
	// feed with no entries — and be indistinguishable from a platform on which
	// nobody has been disabled. See FeedKind.
	if feed.Kind != FeedKind {
		return fmt.Errorf("revocation: %s answered with kind %q, want %q — this URL is not a "+
			"revocation feed, and reading it as one would give this gateway an empty denylist it "+
			"reports as healthy", c.url, feed.Kind, FeedKind)
	}

	marks := make(map[string]time.Time, len(feed.Entries))
	for _, e := range feed.Entries {
		if e.SubjectHash == "" {
			// An entry naming no subject cannot be matched against any token, so
			// accepting it would grow the list while protecting nobody. It means
			// the server and this reader disagree about the wire shape.
			return fmt.Errorf("revocation: feed at %s carries an entry with no subject_hash — "+
				"the feed and this reader disagree about the wire contract", c.url)
		}
		marks[e.SubjectHash] = time.Unix(e.NotBefore, 0).UTC()
	}

	c.mu.Lock()
	c.marks = marks
	c.fetchedAt = c.now()
	c.mu.Unlock()
	return nil
}

// Run refetches on RefreshInterval until ctx is done. A failed refresh is not
// fatal — the previous snapshot ages toward MaxAge, and Check reports the
// consequence — so this loop only ever stops on cancellation.
//
// It reports each failure through onErr, which must not be nil: a refresh that
// fails silently is the whole defect this package exists to remove, one layer
// up. The gateway logs it and exports the age; see cmd/api-gateway.
func (c *Cache) Run(ctx context.Context, onErr func(error)) {
	t := time.NewTicker(c.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Refresh(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// Check reports whether a verified token may still be honoured.
//
// subject is the token's `sub` IN PLAINTEXT — the hashing happens here so no
// caller has to know the feed's representation. issuedAt is the token's `iat`.
//
// A ZERO issuedAt IS REFUSED WHENEVER THE SUBJECT IS MARKED, and that is
// deliberate. `iat` is optional in RFC 7519; a token without one cannot be
// PROVEN to postdate the revocation, and "cannot prove it is new" must resolve
// the same way as "is old" on a control whose entire job is to refuse old
// tokens. An unmarked subject needs no `iat` at all, so this costs nothing for
// the accounts nobody has disabled.
func (c *Cache) Check(subject string, issuedAt time.Time) error {
	c.mu.RLock()
	fetchedAt, marks := c.fetchedAt, c.marks
	c.mu.RUnlock()

	// NEVER FETCHED. A pod that has not yet read the feed knows nothing about
	// who is disabled, and answering "fine" from that state is precisely the
	// silent fail-open #532 names. It refuses until it has been primed —
	// which, in the gateway, happens before it reports itself ready at all.
	if fetchedAt.IsZero() {
		return fmt.Errorf("%w: never fetched", ErrUnusable)
	}
	if age := c.now().Sub(fetchedAt); age > c.maxAge {
		return fmt.Errorf("%w: last fetch %s ago, ceiling %s", ErrUnusable, age.Truncate(time.Second), c.maxAge)
	}

	notBefore, marked := marks[HashSubject(subject)]
	if !marked {
		return nil
	}
	if issuedAt.IsZero() || issuedAt.Before(notBefore) {
		return ErrRevoked
	}
	return nil
}

// Age is how long ago the newest successful fetch landed, and whether there has
// been one at all. It is the number the gateway exports: an operator watching it
// climb toward MaxAge is watching a control that is about to stop answering,
// which is the only warning available before every request starts failing.
func (c *Cache) Age() (time.Duration, bool) {
	c.mu.RLock()
	fetchedAt := c.fetchedAt
	c.mu.RUnlock()
	if fetchedAt.IsZero() {
		return 0, false
	}
	return c.now().Sub(fetchedAt), true
}

// Usable reports whether Check can answer right now — a fetch has landed and it
// is within MaxAge. Exported as a gauge so "this gateway is enforcing
// revocation" and "this gateway has never managed to read the feed" are
// distinguishable from outside the process.
func (c *Cache) Usable() bool {
	age, ok := c.Age()
	return ok && age <= c.maxAge
}

// MaxAge is the configured ceiling, so a caller reporting the posture can name
// the bound rather than restate a default that may have been overridden.
func (c *Cache) MaxAge() time.Duration { return c.maxAge }
