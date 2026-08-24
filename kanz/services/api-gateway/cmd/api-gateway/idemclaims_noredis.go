//go:build !redis

package main

import (
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
)

// newIdempotencyClaims is the DEFAULT build: no Redis client is linked in, so
// the Idempotency middleware falls back to its per-pod claim window.
//
// A SET API_GATEWAY_REDIS_URL IS A HARD ERROR HERE, not a warning. The operator
// asked for cross-pod idempotency and this binary cannot provide it; starting
// anyway would run replicas: 2 on per-pod claims while the deployment says
// otherwise, and the only signal would be a log line nobody reads. That is the
// asymmetry test/arch/redis_build_tag_wiring_test.go names as not exemptible —
// "there is no reading under which that is intended".
//
// Build with `-tags redis` (as services/api-gateway/Dockerfile does) to link the
// Redis-backed store.
func newIdempotencyClaims(cfg config.Config, _ time.Duration, _ int, logger *slog.Logger) (bus.Deduper, io.Closer, error) {
	if cfg.RedisURL != "" {
		logger.Error("API_GATEWAY_REDIS_URL is set but this binary was built WITHOUT -tags redis, " +
			"so cross-pod idempotency claims are not linked in. With replicas > 1 a client retry " +
			"landing on the other replica would be executed again — on /v1/orders, a second live " +
			"order from one client intent. Rebuild with -tags redis or unset the URL")
		return nil, nil, errors.New("api-gateway: API_GATEWAY_REDIS_URL is set but this binary " +
			"was built without -tags redis; cross-pod idempotency claims are not linked in")
	}
	return nil, nil, nil
}
