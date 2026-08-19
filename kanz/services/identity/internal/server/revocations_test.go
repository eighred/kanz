package server

// THE REVOCATION FEED (#532).
//
// The gateway believes this endpoint. Every failure guarded here is one where it
// answers 200 with something that reads as "checked, and nobody is revoked" when
// that is not what happened.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/revocation"
)

func getFeed(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/revocations", nil))
	return rec
}

// REGISTERED WITHOUT PROVISIONING. Unlike /users/{subject}/disable, this route
// is not gated on a provisioning surface: the gateway refuses to become ready
// until it answers, so a deployment that registered it conditionally would take
// the platform's sole ingress out of service by omission.
func TestTheFeedIsServedEvenWhereNobodyCanDisableAnAccount(t *testing.T) {
	s, _ := testServer(t, &fakeStore{}, nil)
	if s.provisioning != nil {
		t.Fatal("this test is meaningless if the fixture wires provisioning")
	}
	rec := getFeed(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /revocations = %d on a deployment with no provisioning surface; want 200. "+
			"The gateway reads a non-200 as a feed it cannot fetch and stays out of the Service",
			rec.Code)
	}
}

// "NOBODY IS REVOKED" AND "THIS FIELD IS MISSING" MUST NOT LOOK THE SAME on a
// wire contract whose whole job is to be believed. A nil slice marshals as
// `null`, which is not a list of nobody.
func TestAnEmptyFeedIsAnEmptyListAndNotNull(t *testing.T) {
	s, _ := testServer(t, &fakeStore{revocations: []revocation.Entry{}}, nil)
	rec := getFeed(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /revocations = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, `"entries":null`) {
		t.Errorf("the empty feed serialised entries as null: %s", body)
	}
	var feed revocation.Feed
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("the gateway's own reader cannot decode this body: %v", err)
	}
	if feed.AsOf == 0 {
		t.Error("as_of is unset — an operator reading this by hand cannot tell a live feed from a " +
			"cached page")
	}
}

// A STORE FAILURE IS 503, NEVER 200 WITH AN EMPTY LIST. The empty-list answer
// would tell the gateway that nobody on this platform has been revoked — the
// single most dangerous lie this endpoint can tell, repeated every 30 seconds.
func TestAStoreFailureRefusesRatherThanServingAnEmptyDenylist(t *testing.T) {
	s, _ := testServer(t, &fakeStore{revocationsErr: errors.New("pool exhausted")}, nil)
	rec := getFeed(t, s)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /revocations = %d when the store failed; want 503. A 200 here is a gateway "+
			"that believes nobody is disabled", rec.Code)
	}
	var feed revocation.Feed
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err == nil && feed.Entries == nil {
		// The body is an error object, not a feed — decoding it as a feed must not
		// yield something a lenient reader would treat as "no entries".
		if !strings.Contains(rec.Body.String(), "error") {
			t.Errorf("the failure body reads as an empty feed: %s", rec.Body.String())
		}
	}
}

// The feed carries what the gateway looks up, and it does not carry the roster.
func TestTheFeedCarriesHashesAndNotSubjects(t *testing.T) {
	const subject = "trader-a@eighred.com"
	s, _ := testServer(t, &fakeStore{revocations: []revocation.Entry{{
		SubjectHash: revocation.HashSubject(subject), NotBefore: 1755500000,
	}}}, nil)
	rec := getFeed(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /revocations = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, subject) {
		t.Fatalf("the feed published a subject in plaintext: %s. identity spends a decoy Argon2id "+
			"verify per unknown login keeping this platform's users unenumerable; this would hand "+
			"out the disabled ones for free", body)
	}
	var feed revocation.Feed
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(feed.Entries) != 1 || feed.Entries[0].SubjectHash != revocation.HashSubject(subject) {
		t.Fatalf("the feed does not carry the entry the gateway would match: %+v", feed.Entries)
	}
}

// THE TWO HALVES MUST AGREE, AND NOTHING ELSE HERE PROVES THAT.
//
// Every other test in this file drives identity's handler and reads the body
// with a decoder the test controls; every test on the gateway side drives the
// real reader against a stub the test controls. Both halves can be individually
// correct and still not fit — that is #530's exact shape, where the gateway
// expected one issuer and identity minted another while each side had passing
// tests.
//
// So this one wires identity's REAL handler to the gateway's REAL
// revocation.Cache over a real HTTP hop: the kind, the field names, the units of
// not_before and the hashing all have to line up or the revoked caller below is
// admitted.
func TestTheGatewaysRealReaderAcceptsIdentitysRealFeed(t *testing.T) {
	const subject = "user:grace"
	revokedAt := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	s, _ := testServer(t, &fakeStore{revocations: []revocation.Entry{{
		SubjectHash: revocation.HashSubject(subject), NotBefore: revokedAt.Unix(),
	}}}, nil)
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := revocation.New(revocation.Config{URL: srv.URL + "/revocations"})
	if err != nil {
		t.Fatalf("revocation.New: %v", err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("the gateway's reader refused identity's own feed: %v. The two halves of the "+
			"wire contract have drifted, and every revoked token on the platform would be "+
			"honoured until somebody noticed the gateway was never ready", err)
	}

	if err := c.Check(subject, revokedAt.Add(-time.Minute)); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("a token minted before the revocation was admitted end to end: got %v, want "+
			"ErrRevoked", err)
	}
	if err := c.Check(subject, revokedAt.Add(time.Minute)); err != nil {
		t.Errorf("a token minted after the revocation was refused end to end: %v", err)
	}
	if err := c.Check("user:someone-else", time.Time{}); err != nil {
		t.Errorf("an unmarked subject was refused end to end: %v", err)
	}
}
