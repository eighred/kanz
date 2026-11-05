//go:build redis

package redisadapter

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func valkeyClient(t *testing.T) (*redis.Client, context.Context, string) {
	t.Helper()
	address := os.Getenv("TEST_REDIS_URL")
	if address == "" {
		t.Skip("TEST_REDIS_URL is not configured")
	}
	opts, err := redis.ParseURL(address)
	if err != nil {
		t.Fatal("invalid test Redis protocol URL")
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal("test server unavailable")
	}
	key := fmt.Sprintf("kanz:valkey-proof:%s:%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = client.Del(cleanup, key).Err()
	})
	return client, ctx, key
}

func TestValkeyIdentityAndVersion(t *testing.T) {
	expected := os.Getenv("TEST_VALKEY_VERSION")
	if expected == "" {
		t.Skip("TEST_VALKEY_VERSION is not configured")
	}
	client, ctx, _ := valkeyClient(t)
	info, err := client.Info(ctx, "server").Result()
	if err != nil {
		t.Fatal("server identity lookup failed")
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(info, "\r\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[key] = value
		}
	}
	if fields["server_name"] != "valkey" || fields["valkey_version"] != expected {
		t.Fatalf("expected Valkey %s, got server=%q version=%q", expected, fields["server_name"], fields["valkey_version"])
	}
}

func TestValkeyAdapterAtomicClaim(t *testing.T) {
	client, ctx, key := valkeyClient(t)
	other, _, _ := valkeyClient(t)
	adapters := []*Client{New(client), New(other)}
	type outcome struct {
		won bool
		err error
	}
	results := make(chan outcome, 32)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 32 {
		workers.Go(func() {
			<-start
			won, err := adapters[i%2].ClaimNX(ctx, key, 5*time.Second)
			results <- outcome{won, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	winners := 0
	for result := range results {
		if result.err != nil {
			t.Fatal("claim failed")
		}
		if result.won {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("atomic claim winners=%d, want 1", winners)
	}
	assertLease(t, client, ctx, key, 0, 5*time.Second)
}

func TestValkeyAdapterLeaseExpires(t *testing.T) {
	client, ctx, key := valkeyClient(t)
	adapter := New(client)
	const lease = 250 * time.Millisecond
	if won, err := adapter.ClaimNX(ctx, key, lease); err != nil || !won {
		t.Fatal("initial claim failed")
	}
	assertLease(t, client, ctx, key, 0, lease)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		won, err := adapter.ClaimNX(ctx, key, lease)
		if err != nil {
			t.Fatal("retry claim failed")
		}
		if won {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expired lease still prevents retry")
}

func TestValkeyAdapterCommitExtendsTTL(t *testing.T) {
	client, ctx, key := valkeyClient(t)
	adapter := New(client)
	if won, err := adapter.ClaimNX(ctx, key, 250*time.Millisecond); err != nil || !won {
		t.Fatal("initial claim failed")
	}
	if err := adapter.SetWithTTL(ctx, key, 5*time.Second); err != nil {
		t.Fatal("commit failed")
	}
	assertLease(t, client, ctx, key, 4*time.Second, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	if won, err := adapter.ClaimNX(ctx, key, time.Second); err != nil || won {
		t.Fatal("committed claim was lost when its original lease expired")
	}
}

func TestValkeyAdapterReleaseAllowsRetry(t *testing.T) {
	client, ctx, key := valkeyClient(t)
	other, _, _ := valkeyClient(t)
	adapter := New(client)
	if won, err := adapter.ClaimNX(ctx, key, 5*time.Second); err != nil || !won {
		t.Fatal("initial claim failed")
	}
	if err := adapter.Del(ctx, key); err != nil {
		t.Fatal("release failed")
	}
	if won, err := New(other).ClaimNX(ctx, key, time.Second); err != nil || !won {
		t.Fatal("released claim prevents another client retrying")
	}
}

func assertLease(t *testing.T, client *redis.Client, ctx context.Context, key string, lower, upper time.Duration) {
	t.Helper()
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil || ttl <= lower || ttl > upper {
		t.Fatalf("lease TTL=%s outside (%s,%s], error=%v", ttl, lower, upper, err)
	}
}
