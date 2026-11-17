//go:build redis

package main

import (
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/redisadapter"
	"github.com/eighred/kanz/services/risk_engine/internal/config"
)

type dedupConfig struct {
	opts *redis.Options
}

// Parse without I/O before listeners start. ParseURL errors may contain the
// configured URL, so the returned diagnostic deliberately excludes their text.
func loadDedupConfig(cfg config.Config) (dedupConfig, error) {
	if cfg.RedisURL == "" {
		return dedupConfig{}, nil
	}
	if strings.ContainsAny(cfg.RedisURL, "?#") {
		return dedupConfig{}, errors.New("invalid RISK_ENGINE_REDIS_URL: query and fragment options are unsupported; coordination budgets are fixed")
	}
	// The SDK supplies localhost when authority is absent and does not bound
	// numeric ports. Those conveniences would conceal malformed explicit input.
	endpoint, err := url.Parse(cfg.RedisURL)
	const invalidEndpoint = "invalid RISK_ENGINE_REDIS_URL: require a redis:// or rediss:// hostname and port 1..65535 when specified"
	if err != nil || (endpoint.Scheme != "redis" && endpoint.Scheme != "rediss") || endpoint.Hostname() == "" {
		return dedupConfig{}, errors.New(invalidEndpoint)
	}
	bracketed := strings.HasPrefix(endpoint.Host, "[")
	if bracketed || strings.Contains(endpoint.Hostname(), ":") {
		ip, err := netip.ParseAddr(endpoint.Hostname())
		if !bracketed || err != nil || !ip.Is6() {
			return dedupConfig{}, errors.New(invalidEndpoint)
		}
	}
	if port := endpoint.Port(); port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return dedupConfig{}, errors.New(invalidEndpoint)
		}
	} else if strings.HasSuffix(endpoint.Host, ":") {
		return dedupConfig{}, errors.New(invalidEndpoint) // explicit empty port is not omission
	}
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil || opts.Network == "unix" {
		return dedupConfig{}, errors.New("invalid RISK_ENGINE_REDIS_URL: expected a valid redis:// or rediss:// endpoint")
	}
	// Coordination may degrade but cannot consume an unbounded admission-path
	// budget. URL data selects endpoint/auth/database, never retry/timeout policy.
	opts.DialTimeout = time.Second
	opts.ReadTimeout = time.Second
	opts.WriteTimeout = time.Second
	opts.PoolTimeout = time.Second
	opts.MaxRetries = -1
	opts.ContextTimeoutEnabled = true
	return dedupConfig{opts: opts}, nil
}

// Allocation is lazy: backend outage is a coordination degradation, not a
// configuration fallback. Handler idempotency remains the correctness floor.
func newDeduper(cfg dedupConfig, logger *slog.Logger, reg prometheus.Registerer) (bus.Deduper, io.Closer) {
	onError := sharedDedupMetrics(reg, cfg.opts != nil)
	if cfg.opts == nil {
		logger.Info("risk dedup configured", "mode", "per_instance")
		return nil, nil
	}
	client := redis.NewClient(cfg.opts)
	// TTL matches the bus default dedup window (2m) so the distributed and
	// broker-side windows reinforce, as RedisDedup documents.
	deduper := bus.NewRedisDedup(redisadapter.New(client), 2*time.Minute,
		bus.WithRedisDedupErrorHandler(func(op string, _ error) {
			onError(op)
			logger.Warn("shared risk dedup operation failed (failing open)", "op", op)
		}))
	logger.Info("risk dedup configured", "mode", "shared", "backend_verified", false)
	return deduper, client
}
