//go:build !redis

package main

// The DEFAULT (vendor-free) build's composition root.
//
// Its in-process store cannot be unreachable, so there is nothing for /readyz to
// probe here — main's `nonces.(server.NonceStoreHealth)` assertion is expected to
// miss, and readiness must not treat that as a fault. What this build DOES have
// to be loud about is the per-pod replay defence itself, and it already refuses
// to start without an explicit admission. These pin both.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/webhook-ingest/internal/config"
	"github.com/eighred/kanz/services/webhook-ingest/internal/server"
)

func TestDefaultBuildRefusesAnUnadmittedInProcessReplayDefence(t *testing.T) {
	_, _, err := newNonceStore(context.Background(), config.Config{ReplayWindow: time.Minute},
		slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("the default build started with a per-pod replay defence nobody admitted to — a " +
			"re-delivered alert landing on a second pod would fan out a SECOND set of orders")
	}
	if !strings.Contains(err.Error(), "WEBHOOK_INGEST_ALLOW_INPROCESS_NONCE") {
		t.Errorf("refusal does not name the opt-in; got: %v", err)
	}
}

func TestDefaultBuildRefusesARedisURLItCannotReach(t *testing.T) {
	_, _, err := newNonceStore(context.Background(),
		config.Config{RedisURL: "redis://127.0.0.1:6379", ReplayWindow: time.Minute},
		slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("a binary built WITHOUT -tags redis accepted WEBHOOK_INGEST_REDIS_URL — the deployment " +
			"believes its replay defence is cross-pod and it is per-pod")
	}
}

// TestTheInProcessStoreIsNotReadinessProbed: not an omission — a map in this
// address space cannot be unreachable. The point is that main's optional
// attachment stays optional, so this build's /readyz keeps answering.
func TestTheInProcessStoreIsNotReadinessProbed(t *testing.T) {
	nonces, _, err := newNonceStore(context.Background(),
		config.Config{ReplayWindow: time.Minute, AllowInProcessNonce: true},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newNonceStore with the admission set: %v", err)
	}

	r := &server.Readiness{}
	r.Set(true)
	if probe, ok := nonces.(server.NonceStoreHealth); ok {
		r.TrackNonceStore(probe)
	}
	if ok, reason := r.Status(context.Background()); !ok {
		t.Fatalf("the default build reports NOT READY with a perfectly good in-process store: %s", reason)
	}
}
