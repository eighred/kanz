//go:build redis

package redisadapter

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kanz-eng/kanz/pkg/bus"
)

// Client adapts a go-redis client to bus.RedisClient, the cross-pod dedup
// seen-set. Any go-redis Cmdable works — *redis.Client, *redis.ClusterClient,
// or a Dragonfly client.
type Client struct{ c redis.Cmdable }

// New wraps a go-redis Cmdable.
func New(c redis.Cmdable) *Client { return &Client{c: c} }

// ClaimNX backs bus.RedisDedup.Claim. go-redis's SetNX issues `SET key v NX PX ttl`
// — ONE command, so Redis serializes concurrent claimants and exactly one wins.
// This must never be reimplemented as Exists-then-Set: two round-trips are two
// chances to interleave, which is the TOCTOU that let concurrent redeliveries
// double-dispatch (and, on the order path, double-trade).
func (a *Client) ClaimNX(ctx context.Context, key string, lease time.Duration) (bool, error) {
	return a.c.SetNX(ctx, key, 1, lease).Result()
}

// SetWithTTL backs bus.RedisDedup.Commit: overwrite the claim's short lease with
// the full dedup window once the dispatch has succeeded.
func (a *Client) SetWithTTL(ctx context.Context, key string, ttl time.Duration) error {
	return a.c.Set(ctx, key, 1, ttl).Err()
}

// Del backs bus.RedisDedup.Release: drop a claim whose dispatch failed, so the
// redelivery is free to retry it.
func (a *Client) Del(ctx context.Context, key string) error {
	return a.c.Del(ctx, key).Err()
}

var _ bus.RedisClient = (*Client)(nil)
