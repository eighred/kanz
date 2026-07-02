//go:build redis

package redisadapter

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kanz-eng/kanz/internal/integrity"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// Client adapts a go-redis client to both shared-state seams: it satisfies
// bus.RedisClient (cross-pod dedup) and integrity.RedisEval (the reconciler's
// atomic PendingStore), so one Redis connection serves both. Any go-redis
// Cmdable works — *redis.Client, *redis.ClusterClient, or a Dragonfly client.
type Client struct{ c redis.Cmdable }

// New wraps a go-redis Cmdable.
func New(c redis.Cmdable) *Client { return &Client{c: c} }

// Exists backs bus.RedisClient.Seen. A Redis error propagates so Seen fails open.
func (a *Client) Exists(ctx context.Context, key string) (bool, error) {
	n, err := a.c.Exists(ctx, key).Result()
	return n > 0, err
}

// SetWithTTL backs bus.RedisClient.Record.
func (a *Client) SetWithTTL(ctx context.Context, key string, ttl time.Duration) error {
	return a.c.Set(ctx, key, 1, ttl).Err()
}

// Eval backs integrity.RedisEval — the atomic PendingStore Lua scripts run
// server-side, so ClaimOrMatch is atomic across replicas.
func (a *Client) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.c.Eval(ctx, script, keys, args...).Result()
}

// Compile-time assertions: one Client satisfies both seams.
var (
	_ bus.RedisClient     = (*Client)(nil)
	_ integrity.RedisEval = (*Client)(nil)
)
