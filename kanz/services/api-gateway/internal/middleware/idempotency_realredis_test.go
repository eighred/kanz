//go:build redis

// THE CROSS-POD IDEMPOTENCY CLAIM, AGAINST A REAL REDIS (#721).
//
// The tests beside this one drive the middleware through its default per-pod
// window. That proves the LOGIC — the scope carries the tenant, a concurrent
// duplicate is refused, a 5xx releases — and it cannot prove the thing this
// deployment actually needs: that two SEPARATE gateway processes, which share no
// memory, refuse each other's duplicate.
//
// api-gateway-deploy.yaml runs replicas: 2. A client retry is routed by the
// Service and lands on whichever pod it lands on. With per-pod claims the two
// pods are two maps, so the retry is not recognised as a retry — /v1/orders
// mints a SECOND order id and publishes a SECOND SubmitOrder. Nothing downstream
// collapses them: the OMS's admission is ON CONFLICT DO NOTHING keyed on
// order_id, and these are two different ids. One client intent, two live orders
// at the venue.
//
// Two independently constructed middleware chains here stand in for the two
// pods. They share nothing but the Redis the estate gives them.
//
// Gated on TEST_REDIS_URL and `-tags redis`, matching pkg/bus's own real-Redis
// test — go-redis is linked only into the tagged build.
package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/redisadapter"
)

func realRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL unset; skipping the real-Redis idempotency proof")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("TEST_REDIS_URL=%q: %v", url, err)
	}
	c := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TWO PODS, ONE REDIS, ONE ORDER.
func TestIdempotencyIsSharedAcrossPodsOverRealRedis(t *testing.T) {
	client := realRedis(t)
	// A prefix unique to this run: a reused broker/store is how a test that counts
	// passes once and fails forever after.
	prefix := "kanz:test:gw:idem:" + t.Name() + ":"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		keys, _ := client.Keys(ctx, prefix+"*").Result()
		if len(keys) > 0 {
			_ = client.Del(ctx, keys...).Err()
		}
	})

	// Each "pod" gets its own middleware instance and its own replay cache — they
	// share only the Redis-backed claim, exactly as two pods would.
	var servedA, servedB int
	pod := func(counter *int) http.Handler {
		claims := bus.NewRedisDedup(redisadapter.New(client), time.Minute,
			bus.WithRedisDedupPrefix(prefix))
		return IdempotencyWith(claims, time.Minute, 100)(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				*counter++
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"order_id":"o-1","status":"submitted"}`))
			}))
	}
	podA, podB := pod(&servedA), pod(&servedB)

	first := httptest.NewRecorder()
	podA.ServeHTTP(first, idemRequest("acme", "u1", http.MethodPost, "/v1/orders", "retry-1"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("pod A returned %d, want 202", first.Code)
	}

	// The retry lands on the OTHER pod.
	second := httptest.NewRecorder()
	podB.ServeHTTP(second, idemRequest("acme", "u1", http.MethodPost, "/v1/orders", "retry-1"))

	if servedB != 0 {
		t.Errorf("pod B executed the retry (%d handler runs). With replicas: 2 that is a second "+
			"live order at the venue from one client intent", servedB)
	}
	if second.Code != http.StatusConflict {
		t.Errorf("pod B returned %d, want 409 — the duplicate must be refused and the client told, "+
			"not executed", second.Code)
	}
	if servedA != 1 {
		t.Errorf("pod A ran the handler %d times, want 1", servedA)
	}
}

// THE TENANT SCOPE SURVIVES THE ROUND TRIP THROUGH REDIS. The scope is built in
// Go and used as a Redis key; this pins that two tenants using one key are two
// keys in the store, not one — the cross-tenant defect would otherwise reappear
// at the shared layer even with the local one fixed.
func TestCrossTenantKeysStayDistinctInRealRedis(t *testing.T) {
	client := realRedis(t)
	prefix := "kanz:test:gw:idem:" + t.Name() + ":"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		keys, _ := client.Keys(ctx, prefix+"*").Result()
		if len(keys) > 0 {
			_ = client.Del(ctx, keys...).Err()
		}
	})

	var served int
	claims := bus.NewRedisDedup(redisadapter.New(client), time.Minute,
		bus.WithRedisDedupPrefix(prefix))
	h := IdempotencyWith(claims, time.Minute, 100)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			p := PrincipalFromContext(r.Context())
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"order_id":"order-for-` + p.Tenant + `"}`))
		}))

	acme := httptest.NewRecorder()
	h.ServeHTTP(acme, idemRequest("acme", "u1", http.MethodPost, "/v1/orders", "shared-key"))
	globex := httptest.NewRecorder()
	h.ServeHTTP(globex, idemRequest("globex", "u2", http.MethodPost, "/v1/orders", "shared-key"))

	if globex.Code == http.StatusConflict {
		t.Fatal("globex was refused a key acme had used — the tenants share a Redis key")
	}
	if got := globex.Body.String(); strings.Contains(got, "order-for-acme") {
		t.Errorf("CROSS-TENANT LEAK through Redis: globex received %q", got)
	}
	if served != 2 {
		t.Errorf("the handler ran %d time(s); both tenants placed an order", served)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keys, err := client.Keys(ctx, prefix+"*").Result()
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("Redis holds %d key(s) for two tenants using the same Idempotency-Key, want 2: %v",
			len(keys), keys)
	}
}
