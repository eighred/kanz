//go:build !redis

package main

import (
	"errors"
	"io"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk_engine/internal/config"
)

type dedupConfig struct{}

// loadDedupConfig runs before listeners or transport I/O. An explicit shared
// selection cannot silently become per-instance operation on the wrong image.
func loadDedupConfig(cfg config.Config) (dedupConfig, error) {
	if cfg.RedisURL != "" {
		return dedupConfig{}, errors.New("RISK_ENGINE_REDIS_URL requires the shared-dedup image (redis build tag)")
	}
	return dedupConfig{}, nil
}

// The default image remains redis-free and uses the bus's per-instance window.
func newDeduper(_ dedupConfig, logger *slog.Logger, reg prometheus.Registerer) (bus.Deduper, io.Closer) {
	sharedDedupMetrics(reg, false)
	logger.Info("risk dedup configured", "mode", "per_instance")
	return nil, nil
}
