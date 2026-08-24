//go:build redis

package main

import (
	"io"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/redisadapter"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
)

// newIdempotencyClaims is the `redis` build: when API_GATEWAY_REDIS_URL is set
// it returns a CROSS-POD claim store for the Idempotency middleware, so a client
// retry that lands on a different replica than the original is recognised as a
// retry rather than executed a second time.
//
// WHY THE GATEWAY NEEDS THIS AND NOT ONLY THE BUS. The bus deduper protects a
// consumer from a redelivered COMMAND. This protects the estate from a
// duplicated HTTP REQUEST, one hop earlier — the two arrivals carry different
// envelopes and different order ids, so nothing downstream can tell they were
// one client intent. api-gateway-deploy.yaml runs replicas: 2.
//
// It reuses pkg/redisadapter, the same bus.RedisClient binding the bus dedup and
// the webhook-ingest nonce store use, and bus.RedisDedup, the estate's one
// implementation of claim/commit/release. A fourth copy of that concept is how
// one of them quietly stops refusing.
//
// FAILING OPEN IS INHERITED AND IS THE RIGHT CHOICE HERE. bus.RedisDedup treats
// a Redis error as "not claimed" and proceeds, so a Redis outage degrades the
// gateway to the per-pod window rather than refusing every order — a trading
// outage caused by the deduplicator would be worse than the duplicate it
// prevents. The error handler makes the degradation visible instead of silent.
//
// An empty URL returns nil, and IdempotencyWith falls back to the per-pod
// window. The caller logs which of the two is in force.
func newIdempotencyClaims(cfg config.Config, ttl time.Duration, max int, logger *slog.Logger) (bus.Deduper, io.Closer, error) {
	if cfg.RedisURL == "" {
		return nil, nil, nil
	}
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		logger.Error("invalid API_GATEWAY_REDIS_URL — idempotency falls back to per-pod claims, "+
			"so a retry landing on the other replica will be executed again", "err", err)
		return nil, nil, nil
	}
	client := redis.NewClient(opts)
	_ = max // the window bound is a property of the in-memory fallback only
	return bus.NewRedisDedup(redisadapter.New(client), ttl,
		// A SEPARATE KEY NAMESPACE from the bus deduper's "kanz:dedup:". These are
		// two different populations — bus keys are envelope idempotency_keys, these
		// are (tenant, subject, route, hash) scopes — sharing one Redis. Keeping
		// them apart means an ops query can count either without the other, and a
		// FLUSH of one namespace cannot silently disarm the other.
		bus.WithRedisDedupPrefix("kanz:gw:idem:"),
		bus.WithRedisDedupErrorHandler(func(op string, err error) {
			logger.Warn("redis idempotency claim degraded (failing open to per-pod)", "op", op, "err", err)
		})), client, nil
}
