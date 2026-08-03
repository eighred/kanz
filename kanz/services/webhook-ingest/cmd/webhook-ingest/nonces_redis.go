//go:build redis

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/eighred/kanz/pkg/redisadapter"
	"github.com/eighred/kanz/services/webhook-ingest/internal/config"
	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
	"github.com/eighred/kanz/services/webhook-ingest/internal/server"
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
//
// THAT SENTENCE WAS FALSE FOR EVERYTHING EXCEPT A MALFORMED URL, and this is what it
// cost. redis.NewClient allocates a connection pool and dials NOTHING — it cannot fail,
// and it did not. Only redis.ParseURL was ever checked. With Redis down the pod started,
// the position book armed, /readyz answered 200, the pod joined its Service, and then
// refused EVERY TradingView alert with ErrNonceStoreUnavailable. Failing closed is
// correct; doing it while every health signal reads healthy is a total trading outage on
// the one service the internet talks to, and nothing pages for it.
//
// So the reachability claim is now made by code, at both of the moments it can break:
//
//   - AT STARTUP, by a real PING. An unreachable Redis is now the startup error this
//     comment always said it was, so a pod that cannot enforce the cross-pod replay
//     defence never enters its Service in the first place.
//   - AFTERWARDS, by readiness. A boot-time check says nothing about the Redis that dies
//     at 10:00 on a pod that booted fine at 09:00 — the identical outage, arrived at from
//     the other direction. redisNonces carries NonceStoreHealthy for the /readyz probe,
//     and main attaches it (server.Readiness.TrackNonceStore).
func newNonceStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (ingest.NonceStore, io.Closer, error) {
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

	// The connection this build exists for, actually made. Bounded so an unreachable
	// host fails the pod instead of hanging its startup — a pod stuck before its probe
	// server is a pod with no signal at all, which is the shape of the bug above.
	pingCtx, cancel := context.WithTimeout(ctx, nonceStartupPing)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("WEBHOOK_INGEST_REDIS_URL is unreachable (%q): the cross-pod replay "+
			"defence cannot be consulted, so this pod would refuse EVERY TradingView alert with a 503 while "+
			"reporting healthy. Refusing to start instead: %w", opts.Addr, err)
	}

	logger.Info("replay defence is CROSS-POD (redis) — this deployment may run N replicas",
		"replay_window", cfg.ReplayWindow, "addr", opts.Addr)
	store := ingest.NewRedisNonces(redisadapter.New(client), cfg.ReplayWindow, nonceClaimLease)
	return redisNonces{NonceStore: store, client: client}, client, nil
}

// redisNonces is the cross-pod store plus the health check /readyz needs. It is the
// ONLY thing in this file that leaves the `redis` build, and it leaves it as the
// build-agnostic server.NonceStoreHealth — so go-redis stays out of the default binary.
type redisNonces struct {
	ingest.NonceStore
	client *redis.Client
}

// NonceStoreHealthy is the readiness question, asked of the real server: can this pod
// claim a nonce right now? A PING that fails means every alert arriving at this pod
// will be refused, and readiness must say so rather than let the pod keep taking
// traffic it will only 503.
func (r redisNonces) NonceStoreHealthy(ctx context.Context) error { return r.client.Ping(ctx).Err() }

var _ server.NonceStoreHealth = redisNonces{}

// nonceStartupPing bounds the boot-time reachability check. Generous relative to a
// healthy PING (sub-millisecond in-cluster) and short relative to any pod start budget,
// so a Redis that is merely slow is not mistaken for one that is gone.
const nonceStartupPing = 5 * time.Second

// nonceClaimLease bounds a claim held by a pod that dies mid-signal: the alert becomes
// re-deliverable when it expires rather than being stranded un-replayable forever.
const nonceClaimLease = 5 * time.Second

// maxInProcessNonces bounds the in-process window so a nonce storm cannot exhaust memory.
const maxInProcessNonces = 100_000
