//go:build redis

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/eighred/kanz/pkg/redisadapter"
	"github.com/eighred/kanz/services/webhook-ingest/internal/config"
	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
)

// newNonceStore is the `redis` build: the nonce cache becomes CROSS-POD (EXEC-M17), so a
// re-delivered TradingView alert is a replay wherever it lands. That is what lets
// webhook-ingest — the one service the internet talks to, and therefore the one whose
// single-replica pin is a single point of failure — run more than one pod.
//
// It reuses pkg/redisadapter, the same bus.RedisClient binding the bus dedup uses, so
// there is one go-redis adapter in the repository and not two.
//
// A bad or unreachable Redis is a STARTUP ERROR, never a silent fallback to the
// in-process store: falling back would leave a multi-replica deployment with a per-pod
// replay defence and no indication of it, which is precisely the failure this build
// exists to remove.
func newNonceStore(cfg config.Config, logger *slog.Logger) (ingest.NonceStore, io.Closer, error) {
	if cfg.RedisURL == "" {
		if !cfg.AllowInProcessNonce {
			return nil, nil, errors.New("no WEBHOOK_INGEST_REDIS_URL: the replay defence would be IN-PROCESS, which " +
				"is correct for EXACTLY ONE REPLICA. Set it, or set WEBHOOK_INGEST_ALLOW_INPROCESS_NONCE=true to " +
				"accept a per-pod defence — in which case this deployment MUST run exactly one replica")
		}
		logger.Warn("REPLAY DEFENCE IS IN-PROCESS — this deployment MUST run exactly ONE replica",
			"fix", "set WEBHOOK_INGEST_REDIS_URL")
		return ingest.NewMemoryNonces(cfg.ReplayWindow, maxInProcessNonces), nil, nil
	}

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid WEBHOOK_INGEST_REDIS_URL: %w", err)
	}
	client := redis.NewClient(opts)
	logger.Info("replay defence is CROSS-POD (redis) — this deployment may run N replicas",
		"replay_window", cfg.ReplayWindow)
	return ingest.NewRedisNonces(redisadapter.New(client), cfg.ReplayWindow, nonceClaimLease), client, nil
}

// nonceClaimLease bounds a claim held by a pod that dies mid-signal: the alert becomes
// re-deliverable when it expires rather than being stranded un-replayable forever.
const nonceClaimLease = 5 * time.Second

// maxInProcessNonces bounds the in-process window so a nonce storm cannot exhaust memory.
const maxInProcessNonces = 100_000
