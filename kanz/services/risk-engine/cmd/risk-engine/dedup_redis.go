//go:build redis

package main

import (
	"io"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/redisadapter"
	"github.com/eighred/kanz/services/risk-engine/internal/config"
)

// newDeduper is the `redis` build: when RISK_ENGINE_REDIS_URL is set it builds
// a cross-pod bus.Deduper backed by Redis (PARITY-05c cutover) so an N-replica
// group dedups redeliveries that land on a different replica than the original.
// The one redisadapter.Client satisfies both shared-state seams; here we use its
// bus.RedisClient half. Empty URL ⇒ per-instance dedup (nil Deduper). The
// returned Closer tears down the connection pool on shutdown.
func newDeduper(cfg config.Config, logger *slog.Logger) (bus.Deduper, io.Closer) {
	if cfg.RedisURL == "" {
		return nil, nil
	}
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		logger.Error("invalid RISK_ENGINE_REDIS_URL — falling back to per-instance dedup", "err", err)
		return nil, nil
	}
	client := redis.NewClient(opts)
	// TTL matches the bus default dedup window (2m) so the distributed and
	// broker-side windows reinforce, as RedisDedup documents.
	deduper := bus.NewRedisDedup(redisadapter.New(client), 2*time.Minute,
		bus.WithRedisDedupErrorHandler(func(op string, err error) {
			logger.Warn("redis dedup degraded (failing open)", "op", op, "err", err)
		}))
	logger.Info("cross-pod shared-state dedup enabled (redis)")
	return deduper, client
}
