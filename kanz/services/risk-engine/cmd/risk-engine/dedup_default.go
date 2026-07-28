//go:build !redis

package main

import (
	"io"
	"log/slog"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk-engine/internal/config"
)

// newDeduper is the default (redis-free) build: cross-pod dedup is a wiring
// choice deferred to the `redis` build so the base binary carries no go-redis
// dependency (the PARITY-04a/04h build-tag stance). Returns a nil Deduper, so
// bus.NewConsumer keeps its per-instance in-memory dedup window. If RedisURL is
// set here it is logged as ignored — the operator must build with `-tags redis`.
func newDeduper(cfg config.Config, logger *slog.Logger) (bus.Deduper, io.Closer) {
	if cfg.RedisURL != "" {
		logger.Warn("RISK_ENGINE_REDIS_URL set but binary built without -tags redis — using per-instance dedup")
	}
	return nil, nil
}
