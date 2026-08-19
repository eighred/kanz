package revocation

// PER-SUBJECT REVOCATION (#532).
//
// Every test here guards a property whose failure is invisible from the outside:
// authentication still works, every request still succeeds, and the only
// difference is that a disabled account's token is one of the ones succeeding.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func feedServer(t *testing.T, feed *Feed) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	// The fixtures declare the kind the same way identity does, so a test can
	// never pass against a body the deployed reader would refuse.
	feed.Kind = FeedKind
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(feed)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func mustCache(t *testing.T, cfg Config) *Cache {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// A COLD CACHE REFUSES, AND THAT IS THE COLD-START CLAUSE OF #532.
//
// The fail-open shape this replaces is the one every bus-consumer design in this
// estate has had to answer: a fresh pod knows nothing, so "not on my list" and
// "I have no list" are the same answer, and every revoked token sails through
// while the health check is green. It is enforced HERE rather than in the
// composition root so it holds regardless of what any composition root does.
func TestCacheThatHasNeverFetchedRefusesEverybody(t *testing.T) {
	c := mustCache(t, Config{URL: "http://example.invalid/revocations"})

	err := c.Check("anyone", time.Now())
	if !errors.Is(err, ErrUnusable) {
		t.Fatalf("a cache that has never fetched the feed admitted a caller: got %v, want ErrUnusable. "+
			"A pod that knows nothing about who is disabled must not answer 'not revoked'", err)
	}
	if errors.Is(err, ErrRevoked) {
		t.Fatal("an unfetched feed reported ErrRevoked — that is a 401 and would tell every caller " +
			"their session ended during an identity outage")
	}
	if _, ok := c.Age(); ok {
		t.Error("Age reported a successful fetch that never happened")
	}
	if c.Usable() {
		t.Error("Usable is true with no feed in hand — the gauge would report an enforced control " +
			"while nothing is enforced")
	}
}

// The revocation instant, not the account's current status: a token minted
// BEFORE the mark is refused, one minted after it is not.
func TestCheckComparesTheTokenAgainstTheRevocationInstant(t *testing.T) {
	revokedAt := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	srv, _ := feedServer(t, &Feed{
		AsOf:    revokedAt.Unix(),
		Entries: []Entry{{SubjectHash: HashSubject("trader-a"), NotBefore: revokedAt.Unix()}},
	})
	c := mustCache(t, Config{URL: srv.URL})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if err := c.Check("trader-a", revokedAt.Add(-time.Minute)); !errors.Is(err, ErrRevoked) {
		t.Errorf("a token minted BEFORE the revocation was admitted: got %v, want ErrRevoked", err)
	}
	// THE RE-ENABLE CASE, and the reason this is a timestamp and not a flag. An
	// operator who disables and then re-enables an account must not resurrect the
	// old token — but the fresh one from the account's next login has to work
	// with no further operator action.
	if err := c.Check("trader-a", revokedAt.Add(time.Minute)); err != nil {
		t.Errorf("a token minted AFTER the revocation was refused: %v. A re-enabled account could "+
			"never log back in", err)
	}
	if err := c.Check("trader-b", time.Time{}); err != nil {
		t.Errorf("an unmarked subject was refused: %v", err)
	}
}

// A token that cannot be DATED cannot be shown to postdate the revocation, and
// on a control whose whole job is refusing old tokens that has to resolve the
// same way as "is old". `iat` is optional in RFC 7519, so this is reachable.
func TestAMarkedSubjectWithNoIssuedAtIsRefused(t *testing.T) {
	srv, _ := feedServer(t, &Feed{Entries: []Entry{{
		SubjectHash: HashSubject("trader-a"), NotBefore: time.Now().Add(-time.Hour).Unix(),
	}}})
	c := mustCache(t, Config{URL: srv.URL})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := c.Check("trader-a", time.Time{}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("a revoked subject presenting an undatable token was admitted: got %v, want ErrRevoked", err)
	}
	// And it costs nothing for the accounts nobody has disabled.
	if err := c.Check("trader-b", time.Time{}); err != nil {
		t.Fatalf("an unmarked subject with no iat was refused: %v — every token without an `iat` "+
			"claim would fail authentication", err)
	}
}

// PAST THE CEILING THE CACHE STOPS ANSWERING. Continuing to serve "not revoked"
// from a feed it can no longer refresh hands an attacker who can keep identity
// unreachable an unlimited extension on a disabled account.
func TestAFeedPastItsCeilingRefusesRatherThanAssumingNobodyIsRevoked(t *testing.T) {
	srv, _ := feedServer(t, &Feed{Entries: nil})
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	clock := now
	c := mustCache(t, Config{
		URL:             srv.URL,
		RefreshInterval: time.Minute,
		MaxAge:          10 * time.Minute,
		Now:             func() time.Time { return clock },
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	clock = now.Add(9 * time.Minute)
	if err := c.Check("trader-a", now); err != nil {
		t.Fatalf("a feed inside its ceiling refused: %v — a brief identity blip would become a "+
			"trading outage", err)
	}
	if !c.Usable() {
		t.Error("Usable is false inside the ceiling")
	}

	clock = now.Add(11 * time.Minute)
	if err := c.Check("trader-a", now); !errors.Is(err, ErrUnusable) {
		t.Fatalf("a feed past its ceiling still answered: got %v, want ErrUnusable", err)
	}
	if c.Usable() {
		t.Error("Usable is true past the ceiling — the gauge would report an enforced control " +
			"while every request is being refused")
	}
}

// A FAILED REFRESH KEEPS THE PREVIOUS SNAPSHOT. Discarding it would turn a
// single dropped request into an immediate authentication outage, when the
// answer it already holds is still within the ceiling and still correct.
func TestAFailedRefreshKeepsTheSnapshotItAlreadyHas(t *testing.T) {
	revokedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	fail := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(Feed{Kind: FeedKind, Entries: []Entry{{
			SubjectHash: HashSubject("trader-a"), NotBefore: revokedAt.Unix(),
		}}})
	}))
	t.Cleanup(srv.Close)

	c := mustCache(t, Config{URL: srv.URL})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	fail.Store(true)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("a 503 from the feed was reported as a successful refresh — the cache would age " +
			"silently and the operator would learn about it from the ceiling")
	}
	if err := c.Check("trader-a", revokedAt.Add(-time.Minute)); !errors.Is(err, ErrRevoked) {
		t.Fatalf("a failed refresh discarded the revocation it already held: got %v, want ErrRevoked", err)
	}
}

// The feed is a SNAPSHOT, so a successful fetch replaces the map rather than
// merging into it: an entry identity has dropped must not survive here.
func TestASuccessfulRefreshReplacesTheSnapshotRatherThanMergingIntoIt(t *testing.T) {
	revokedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	var entries atomic.Value
	entries.Store([]Entry{{SubjectHash: HashSubject("trader-a"), NotBefore: revokedAt.Unix()}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		//nolint:errcheck // the encoder writes to a test recorder
		_ = json.NewEncoder(w).Encode(Feed{Kind: FeedKind, Entries: entries.Load().([]Entry)})
	}))
	t.Cleanup(srv.Close)

	c := mustCache(t, Config{URL: srv.URL})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	entries.Store([]Entry{})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := c.Check("trader-a", revokedAt.Add(-time.Minute)); err != nil {
		t.Fatalf("an entry the feed no longer carries still refused a caller: %v", err)
	}
}

// A NON-200 IS NOT AN EMPTY FEED. Parsing an error page as "nobody is revoked"
// is the single most dangerous misreading available here, and it would be made
// every RefreshInterval without anybody noticing.
func TestANonOKResponseIsAFailureAndNotAnEmptyFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := mustCache(t, Config{URL: srv.URL})
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("a 404 was accepted as a revocation feed")
	}
	if err := c.Check("anyone", time.Now()); !errors.Is(err, ErrUnusable) {
		t.Fatalf("after a failed first fetch the cache admitted a caller: got %v, want ErrUnusable", err)
	}
}

// AN OVERSIZED BODY FAILS RATHER THAN TRUNCATING. A denylist cut short admits
// whoever fell off the end, silently.
func TestAnOversizedFeedIsRefusedRatherThanTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"` + FeedKind + `","entries":[`))
		one := `{"subject_hash":"` + strings.Repeat("a", 64) + `","not_before":1},`
		for written := 0; written <= maxFeedBytes; written += len(one) {
			_, _ = w.Write([]byte(one))
		}
		_, _ = w.Write([]byte(`{"subject_hash":"z","not_before":1}]}`))
	}))
	t.Cleanup(srv.Close)
	c := mustCache(t, Config{URL: srv.URL})
	err := c.Refresh(context.Background())
	if err == nil {
		t.Fatal("an oversized feed was accepted — the cache would hold a prefix of the denylist " +
			"and report it as complete")
	}
	if !strings.Contains(err.Error(), "partial denylist") {
		t.Errorf("the refusal does not name the consequence: %v", err)
	}
}

// An entry with no subject means the server and this reader disagree about the
// wire shape. Accepting it grows the list while protecting nobody.
func TestAnEntryWithNoSubjectIsAWireDisagreementAndNotAnEntry(t *testing.T) {
	srv, _ := feedServer(t, &Feed{Entries: []Entry{{SubjectHash: "", NotBefore: 1}}})
	c := mustCache(t, Config{URL: srv.URL})
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("an entry naming no subject was accepted into the denylist")
	}
}

// A 200 FROM THE WRONG ENDPOINT IS NOT AN EMPTY DENYLIST.
//
// This is the misconfiguration the whole discriminator exists for: identity's
// /jwks.json sits one path segment from /revocations, on the same host and port,
// and every field of Feed is optional to a JSON decoder. Without the kind check
// a gateway pointed at it would prime, report itself usable, and honour every
// revoked token on the platform forever — "nothing configured" and "checked, and
// nobody is revoked" reduced to the same observable state.
func TestAJSONBodyFromTheWrongEndpointIsRefusedAndNotReadAsAnEmptyDenylist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A real JWKS document, which is what is actually one path away.
		_, _ = w.Write([]byte(`{"keys":[{"kty":"EC","crv":"P-256","kid":"abc","use":"sig"}]}`))
	}))
	t.Cleanup(srv.Close)

	c := mustCache(t, Config{URL: srv.URL})
	err := c.Refresh(context.Background())
	if err == nil {
		t.Fatal("a JWKS document was accepted as a revocation feed — this gateway would report " +
			"itself as enforcing revocation while holding an empty denylist forever")
	}
	if !strings.Contains(err.Error(), "not a revocation feed") {
		t.Errorf("the refusal does not name what went wrong: %v", err)
	}
	if cerr := c.Check("anyone", time.Now()); !errors.Is(cerr, ErrUnusable) {
		t.Fatalf("after refusing the wrong body the cache admitted a caller: got %v, want ErrUnusable", cerr)
	}
	if c.Usable() {
		t.Error("Usable is true after refusing the body — the gauge would report an enforced control")
	}
}

// A ceiling at or below the refresh cadence expires the feed before the next
// fetch is due, so a HEALTHY system refuses every request. Refuse it at
// construction rather than discover it as an authentication outage.
func TestACeilingBelowTheRefreshCadenceIsRefusedAtConstruction(t *testing.T) {
	if _, err := New(Config{URL: "http://x/", RefreshInterval: time.Minute, MaxAge: time.Minute}); err == nil {
		t.Fatal("a MaxAge equal to the refresh interval was accepted")
	}
	if _, err := New(Config{URL: ""}); err == nil {
		t.Fatal("a cache with no feed URL was accepted — it would be constructed as if it could " +
			"tell a disabled account from an active one")
	}
}

// The two sides of the wire agree on the hash, and it is not the subject itself.
func TestTheFeedNamesNobody(t *testing.T) {
	const subject = "trader-a@eighred.com"
	h := HashSubject(subject)
	if strings.Contains(h, subject) || h == subject {
		t.Fatal("HashSubject returned the subject — the feed would publish the roster of every " +
			"account an operator has ever disabled")
	}
	if len(h) != 64 {
		t.Fatalf("HashSubject returned %d chars, want 64 (sha256 hex)", len(h))
	}
	if HashSubject(subject) != h {
		t.Fatal("HashSubject is not deterministic — the gateway could never match an entry")
	}
}

// Run keeps refetching. Proven by observing a second hit, not by sleeping for a
// fixed interval and hoping.
func TestRunKeepsRefetching(t *testing.T) {
	srv, hits := feedServer(t, &Feed{})
	c := mustCache(t, Config{URL: srv.URL, RefreshInterval: 5 * time.Millisecond, MaxAge: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(error) {}) }()

	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("Run made %d fetches in 5s — the refresh loop is not running, and a revocation "+
				"would never reach this gateway", hits.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
