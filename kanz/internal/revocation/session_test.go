package revocation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionGenerationFencesEveryIssuanceTime(t *testing.T) {
	boundary := time.Now().Truncate(time.Second)
	srv, _ := feedServer(t, &Feed{Entries: []Entry{{SubjectHash: HashSubject("a"), NotBefore: boundary.Unix(), SessionEpoch: 2}}})
	c := mustCache(t, Config{URL: srv.URL})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{boundary.Add(-time.Second), boundary, boundary.Add(999 * time.Millisecond), boundary.Add(time.Hour)} {
		for _, old := range []int64{0, 1} {
			if err := c.Check("a", at, old); !errors.Is(err, ErrRevoked) {
				t.Fatalf("epoch %d at %s: %v", old, at, err)
			}
		}
		// A fresh account snapshot is valid even after clock rollback or same-second enable.
		if err := c.Check("a", at, 2); err != nil {
			t.Fatal(err)
		}
	}
	for _, subject := range []string{"a", "unmarked"} {
		if err := c.Check(subject, boundary, 3); !errors.Is(err, ErrUnusable) {
			t.Fatalf("ahead of cache: %v", err)
		}
	}
	if err := c.Check("a", time.Time{}, 2); !errors.Is(err, ErrRevoked) {
		t.Fatal(err)
	}
}

func TestInvalidOrRegressedFeedDoesNotRenewFreshness(t *testing.T) {
	var feed atomic.Value
	good := Feed{Kind: FeedKind, Entries: []Entry{{SubjectHash: HashSubject("a"), SessionEpoch: 2}}}
	feed.Store(good)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(feed.Load()) }))
	defer srv.Close()
	now := time.Now()
	c := mustCache(t, Config{URL: srv.URL, RefreshInterval: time.Second, MaxAge: time.Minute, Now: func() time.Time { return now }})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	for _, bad := range []Feed{
		{Kind: "kanz.revocations.v1"},
		{Kind: FeedKind, Entries: []Entry{{SubjectHash: HashSubject("a")}}},
		{Kind: FeedKind, Entries: []Entry{{SubjectHash: HashSubject("a"), SessionEpoch: 1}}},
		{Kind: FeedKind, Entries: append(good.Entries, good.Entries...)},
		{Kind: FeedKind},
	} {
		feed.Store(bad)
		if err := c.Refresh(context.Background()); err == nil {
			t.Fatal("invalid snapshot accepted")
		}
		if err := c.Check("a", now, 2); !errors.Is(err, ErrUnusable) {
			t.Fatalf("failed feed renewed freshness: %v", err)
		}
	}
}
