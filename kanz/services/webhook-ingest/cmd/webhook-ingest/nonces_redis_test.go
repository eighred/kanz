//go:build redis

package main

// The `redis` build's composition root, exercised.
//
// The comment on newNonceStore promised "a bad or unreachable Redis is a STARTUP
// ERROR, never a silent fallback to the in-process store" and the code checked
// only redis.ParseURL: redis.NewClient allocates a pool and dials nothing, so an
// unreachable Redis started cleanly. The pod armed, answered /readyz 200, joined
// its Service, and refused every TradingView alert with a 503. These tests are
// what makes the promise true rather than written down.
//
// CI runs them: kanz-ci.yml's tagged step is
// `go test -tags redis -p 1 -race ./services/webhook-ingest/... ...`, and this
// package is under that path. Nothing here needs a Redis server — 127.0.0.1:1
// refuses the connection immediately, which is precisely the condition under
// test.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/eighred/kanz/services/webhook-ingest/internal/config"
	"github.com/eighred/kanz/services/webhook-ingest/internal/server"
)

// unreachable is a port nothing listens on: the connection is refused at once,
// so these tests are hermetic and fast rather than waiting out a dial timeout.
const unreachable = "redis://127.0.0.1:1"

func TestAnUnreachableRedisRefusesToStart(t *testing.T) {
	cfg := config.Config{RedisURL: unreachable, ReplayWindow: time.Minute}

	store, closer, err := newNonceStore(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err == nil {
		if closer != nil {
			_ = closer.Close()
		}
		t.Fatalf("newNonceStore returned %T and NO ERROR for an unreachable Redis — the pod would start, "+
			"report /readyz 200, join its Service and then refuse EVERY TradingView alert with a 503", store)
		return
	}
	if store != nil {
		t.Errorf("a store was returned alongside the refusal: %T", store)
	}
	// It must never quietly become the in-process store: that is a per-pod replay
	// defence on a deployment that asked for a cross-pod one, and nothing says so.
	for _, want := range []string{"WEBHOOK_INGEST_REDIS_URL", "unreachable", "Refusing to start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q; got: %v", want, err)
		}
	}
}

func TestAMalformedRedisURLStillRefusesToStart(t *testing.T) {
	cfg := config.Config{RedisURL: "not-a-redis-url", ReplayWindow: time.Minute}

	if _, _, err := newNonceStore(context.Background(), cfg, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a malformed WEBHOOK_INGEST_REDIS_URL started cleanly")
	}
}

// TestTheRedisStoreIsWhatReadinessCanProbe pins the type assertion main relies
// on. If redisNonces stopped satisfying server.NonceStoreHealth, main's
// `nonces.(server.NonceStoreHealth)` would silently stop matching — the probe
// would never be attached, and /readyz would go back to reporting 200 through a
// Redis outage with nothing failing to say so.
func TestTheRedisStoreIsWhatReadinessCanProbe(t *testing.T) {
	dead := redisNonces{client: redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})}
	t.Cleanup(func() { _ = dead.client.Close() })

	probe, ok := any(dead).(server.NonceStoreHealth)
	if !ok {
		t.Fatal("the redis nonce store does not satisfy server.NonceStoreHealth — main's type " +
			"assertion would not match and readiness would stop representing the replay defence")
	}
	if err := probe.NonceStoreHealthy(context.Background()); err == nil {
		t.Fatal("NonceStoreHealthy reported a DEAD Redis as healthy")
	}

	// And it honours the caller's deadline, so a hung Redis cannot hold the /readyz
	// handler past the kubelet's probe budget.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if err := probe.NonceStoreHealthy(ctx); err == nil {
		t.Fatal("NonceStoreHealthy ignored an already-expired context")
	}
}
