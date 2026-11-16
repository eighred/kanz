package server

// /readyz must represent the REPLAY DEFENCE, not only the position book.
//
// The bug these pin: with Redis down the pod armed its book, answered /readyz
// 200, joined its Service, and refused every TradingView alert with a 503
// (ErrNonceStoreUnavailable). Failing closed is correct — reporting healthy
// while doing it is a total ingest outage on the platform's public entrance
// that no health signal contradicts, so nothing pages and nobody looks.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubNonceHealth is a replay defence whose reachability the test controls.
type stubNonceHealth struct {
	err   error
	block time.Duration
}

func (s stubNonceHealth) NonceStoreHealthy(ctx context.Context) error {
	if s.block > 0 {
		select {
		case <-time.After(s.block):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.err
}

func TestReadyzIsNotReadyWhenTheReplayDefenceIsUnreachable(t *testing.T) {
	r := &Readiness{}
	r.Set(true) // the book IS armed — the only thing wrong is the nonce store
	r.TrackNonceStore(stubNonceHealth{err: errors.New("dial tcp 10.0.0.9:6379: connect: connection refused")})

	ok, reason := r.Status(context.Background())
	if ok {
		t.Fatal("READY with an unreachable replay defence — this pod would join its Service and " +
			"503 every TradingView alert while every health signal reported healthy")
	}
	// The reason has to name the consequence: an operator reading a probe failure
	// must not have to go find out what a nonce store is.
	for _, want := range []string{"replay defence", "EVERY TradingView alert", "connection refused"} {
		if !strings.Contains(reason, want) {
			t.Errorf("readiness reason does not mention %q; got: %s", want, reason)
		}
	}

	rec := httptest.NewRecorder()
	newProbeServer(t, r).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /readyz body: %v", err)
	}
	if !strings.Contains(body["status"], "replay defence") {
		t.Errorf("/readyz body does not carry the reason; got %q", body["status"])
	}
}

func TestReadyzIsReadyWhenTheReplayDefenceAnswers(t *testing.T) {
	r := &Readiness{}
	r.Set(true)
	r.TrackNonceStore(stubNonceHealth{})

	if ok, reason := r.Status(context.Background()); !ok {
		t.Fatalf("not ready with a healthy replay defence: %s", reason)
	}

	rec := httptest.NewRecorder()
	newProbeServer(t, r).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200", rec.Code)
	}
}

// TestReadyzIsNotReadyBeforeTheBookIsArmed: the pre-existing precondition still
// gates, and now says which one it is.
func TestReadyzIsNotReadyBeforeTheBookIsArmed(t *testing.T) {
	r := &Readiness{}
	r.TrackNonceStore(stubNonceHealth{})

	ok, reason := r.Status(context.Background())
	if ok {
		t.Fatal("ready before the position book was armed")
	}
	if !strings.Contains(reason, "position book") {
		t.Errorf("reason does not say WHICH precondition is unmet; got: %s", reason)
	}
}

// TestNoNonceProbeIsNotAFailure: the default (vendor-free) build's in-process
// store cannot be unreachable and implements nothing, so an unattached probe
// must read as ready — not as a permanently broken pod.
func TestNoNonceProbeIsNotAFailure(t *testing.T) {
	r := &Readiness{}
	r.Set(true)
	if ok, reason := r.Status(context.Background()); !ok {
		t.Fatalf("not ready with no nonce probe attached (the default build's posture): %s", reason)
	}
	r.TrackNonceStore(nil) // a nil probe is a no-op, never a nil dereference
	if ok, reason := r.Status(context.Background()); !ok {
		t.Fatalf("not ready after TrackNonceStore(nil): %s", reason)
	}
}

// TestAHangingNonceStoreIsNotReady: a Redis that accepts the connection and then
// never answers is as useless to the trading path as one that refuses, and it is
// the shape that would otherwise hang the probe handler until the kubelet killed
// it — a failed probe with no reason recorded anywhere.
func TestAHangingNonceStoreIsNotReady(t *testing.T) {
	r := &Readiness{}
	r.Set(true)
	r.TrackNonceStore(stubNonceHealth{block: 10 * time.Second})

	start := time.Now()
	ok, reason := r.Status(context.Background())
	elapsed := time.Since(start)

	if ok {
		t.Fatal("READY while the replay defence never answered")
	}
	if !strings.Contains(reason, "replay defence") {
		t.Errorf("reason does not name the replay defence; got: %s", reason)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Status took %v — it must answer inside the kubelet's 1s default probe budget "+
			"(nonceProbeBudget=%v), or the probe is killed and the reason is lost", elapsed, nonceProbeBudget)
	}
}

// newProbeServer builds a Server with no pipeline — these tests only exercise
// the probe routes, and nothing on them touches the pipeline.
func newProbeServer(t *testing.T, r *Readiness) *Server {
	t.Helper()
	return New(r, nil, nil)
}
