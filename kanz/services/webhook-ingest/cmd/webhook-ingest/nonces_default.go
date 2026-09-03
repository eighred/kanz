//go:build !redis

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/eighred/kanz/services/webhook-ingest/internal/config"
	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
)

// newNonceStore is the DEFAULT (vendor-free) build: there is no Redis client linked
// into this binary, so the only store available is the in-process one.
//
// And the in-process store is correct for EXACTLY ONE REPLICA. Two pods are two maps: a
// re-delivered TradingView alert landing on the other pod is admitted a SECOND time and
// fans out a SECOND set of orders.
//
// THOSE ORDERS DO NOT REACH A LIVE EXCHANGE, which is a correction to what this comment
// used to assert (#820). signal_id is DeterministicID(strategy, nonce) and order_id is
// DeterministicID(signal_id, venue), so a redelivery re-derives the SAME order_id and
// the OMS admission gate refuses it: ON CONFLICT (tenant_id, order_id) DO NOTHING acks
// the delivery and stops it BEFORE routing. The value of THIS store is that the replay
// is refused at the perimeter and can be named as one — see config.RedisURL.
//
// So this REFUSES TO START unless the deployment says out loud that it accepts a per-pod
// replay defence. A defence that degrades to per-pod by FORGETTING to configure Redis is
// indistinguishable from a correct one, and the deployment that forgot is exactly the one
// that will be scaled to N replicas by an autoscaler nobody consulted.
//
// The store this build returns does NOT implement server.NonceStoreHealth, and that is
// correct rather than an omission: a map in this address space cannot be unreachable, so
// there is nothing for /readyz to check. The reachability that CAN fail belongs to the
// `redis` build, and lives there (nonces_redis.go). ctx is unused here for the same
// reason — the signature is shared so main does not need to know which build it is.
func newNonceStore(_ context.Context, cfg config.Config, logger *slog.Logger) (ingest.NonceStore, io.Closer, error) {
	if cfg.RedisURL != "" {
		return nil, nil, errors.New("WEBHOOK_INGEST_REDIS_URL is set but this binary was built WITHOUT -tags redis, " +
			"so it cannot reach it. Rebuild with -tags redis, or the replay defence would silently be per-pod")
	}
	if !cfg.AllowInProcessNonce {
		return nil, nil, errors.New("no WEBHOOK_INGEST_REDIS_URL: the replay defence would be IN-PROCESS, which is " +
			"correct for EXACTLY ONE REPLICA — a re-delivered alert landing on a second pod would be admitted again " +
			"and would fan out a SECOND set of orders. Point WEBHOOK_INGEST_REDIS_URL at a Redis (and build with " +
			"-tags redis), or set WEBHOOK_INGEST_ALLOW_INPROCESS_NONCE=true to accept it — in which case this " +
			"deployment MUST run exactly one replica")
	}
	logger.Warn("REPLAY DEFENCE IS IN-PROCESS — this deployment MUST run exactly ONE replica. A re-delivered "+
		"TradingView alert landing on a second pod would be admitted again and would fan out a SECOND set of orders",
		"fix", "set WEBHOOK_INGEST_REDIS_URL (with -tags redis) to make the nonce cache cross-pod")
	return ingest.NewMemoryNonces(cfg.ReplayWindow, maxInProcessNonces), nil, nil
}

// maxInProcessNonces bounds the in-process window so a nonce storm cannot exhaust memory.
const maxInProcessNonces = 100_000
