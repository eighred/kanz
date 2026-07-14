//go:build redis

package ingest

// EXEC-M22 — the code that lets webhook-ingest run more than one pod had NEVER RUN AGAINST
// A REAL REDIS.
//
// nonce_redis_test.go drives RedisNonces against a FAKE (a map behind the bus.RedisClient
// interface), which proves the LOGIC — the claim, the replay, the fail-closed. What it
// cannot prove is that `pkg/redisadapter` speaks Redis correctly: that ClaimNX really is an
// atomic SET NX PX and not a read-then-write, that the lease really expires, that Del really
// frees the claim. And the `redis` build tag is not compiled by CI, so nothing anywhere
// would have told us if it broke.
//
// That is the code EXEC-M22 makes load-bearing for the platform's ENTRANCE. It gets a real
// server under it before we scale the pods that depend on it.
//
// Gated on TEST_REDIS_URL and `-tags redis` (go-redis is only linked into that build).

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kanz-eng/kanz/pkg/redisadapter"
)

const (
	realWindow = 2 * time.Second
	realLease  = 1 * time.Second
)

// realNonces returns a RedisNonces over a REAL Redis, plus a unique key prefix so parallel
// runs of this suite cannot collide on the same server.
func realNonces(t *testing.T) (*RedisNonces, string) {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("set TEST_REDIS_URL (and build with -tags redis) to run the real-Redis nonce tests")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse TEST_REDIS_URL: %v", err)
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	return NewRedisNonces(redisadapter.New(client), realWindow, realLease), t.Name() + "-" + time.Now().Format("150405.000000")
}

// TestARealRedisAdmitsAnAlertExactlyOnce is the whole point: TWO PODS, ONE REDIS.
//
// Both pods point at the same server, so they are the same store. A TradingView alert
// re-delivered to the OTHER pod must be seen as the replay it is — not admitted a second
// time and fanned out as a SECOND set of orders against a live exchange. This is what makes
// `replicas: 2` safe, and it is exactly what the in-process store cannot do.
func TestARealRedisAdmitsAnAlertExactlyOnce(t *testing.T) {
	podA, key := realNonces(t)
	podB, _ := realNonces(t) // a SECOND pod, same Redis
	ctx := context.Background()

	got, err := podA.Claim(ctx, key)
	if err != nil {
		t.Fatalf("pod A claim: %v", err)
	}
	if !got {
		t.Fatal("pod A could not claim a fresh alert")
	}
	podA.Commit(ctx, key) // pod A decided it: the alert has traded

	// The SAME alert is re-delivered, and the load balancer sends it to the other pod.
	got, err = podB.Claim(ctx, key)
	if err != nil {
		t.Fatalf("pod B claim: %v", err)
	}
	if got {
		t.Fatal("POD B ADMITTED AN ALERT POD A HAD ALREADY TRADED — a re-delivered TradingView " +
			"alert would fan out a SECOND set of orders against a live exchange")
	}
}

// TestARealRedisHoldsTheClaimForTheWholeWindow: Commit must hold the key for the REPLAY
// WINDOW, not for the claim's short lease. If it decayed at the lease, a redelivery arriving
// a second later would look fresh and trade again.
func TestARealRedisHoldsTheClaimForTheWholeWindow(t *testing.T) {
	pod, key := realNonces(t)
	ctx := context.Background()

	if ok, err := pod.Claim(ctx, key); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	pod.Commit(ctx, key)

	// Past the LEASE, well inside the WINDOW. Still a replay.
	time.Sleep(realLease + 300*time.Millisecond)

	ok, err := pod.Claim(ctx, key)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ok {
		t.Fatal("a committed alert became re-tradeable after the LEASE expired — Commit must hold " +
			"it for the full replay window, or a redelivery trades twice")
	}
}

// TestARealRedisFreesAClaimNothingActedOn: a pod that claimed an alert and then FAILED to do
// anything with it (a publish error, a crash before the fan-out) must not strand the alert
// un-replayable forever. Release frees it, and the retry is admitted — the alert is a live
// trading signal, and dropping it silently is the failure this whole path exists to avoid.
func TestARealRedisFreesAClaimNothingActedOn(t *testing.T) {
	pod, key := realNonces(t)
	ctx := context.Background()

	if ok, err := pod.Claim(ctx, key); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	pod.Release(ctx, key) // nothing was published; the alert never traded

	ok, err := pod.Claim(ctx, key)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !ok {
		t.Fatal("an alert nothing acted on could not be retried — a live trading signal was stranded")
	}
}

// TestARealRedisReleasesAClaimHeldByADeadPod: a pod that dies BETWEEN claiming and deciding
// releases nothing. The claim's LEASE is what stops the alert being stranded forever: it
// expires, and the redelivery is admitted.
func TestARealRedisReleasesAClaimHeldByADeadPod(t *testing.T) {
	dead, key := realNonces(t)
	alive, _ := realNonces(t)
	ctx := context.Background()

	if ok, err := dead.Claim(ctx, key); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	// The pod dies here: no Commit, no Release. The claim is held by nobody.

	if ok, _ := alive.Claim(ctx, key); ok {
		t.Fatal("a live claim was stolen before its lease expired — two pods would trade the same alert")
	}

	time.Sleep(realLease + 300*time.Millisecond) // the lease expires

	ok, err := alive.Claim(ctx, key)
	if err != nil {
		t.Fatalf("claim after lease: %v", err)
	}
	if !ok {
		t.Fatal("the alert stayed claimed by a DEAD pod — a live trading signal stranded forever")
	}
}

// TestTwoPodsOverARealRedisFanOutAnAlertOnce is the payoff of EXEC-M22, and the thing that
// makes `replicas: 2` on the platform's entrance safe rather than a double-trade machine.
//
// TestOneNonceCacheAcrossPods (nonce_test.go) proves this over a SHARED IN-MEMORY store and
// says "RedisNonces in production" — but production is the part that had never been run. Two
// real pipelines, one real Redis, the same signed alert re-delivered to the OTHER pod.
func TestTwoPodsOverARealRedisFanOutAnAlertOnce(t *testing.T) {
	shared, _ := realNonces(t)
	pubA, pubB := &brokenPublisher{}, &brokenPublisher{}
	podA := pipelineOver(t, shared, pubA) // one Redis...
	podB := pipelineOver(t, shared, pubB) // ...two pods

	raw := body("buy", "1", "absolute_qty", "nonce-"+time.Now().Format("150405.000000000"))

	if err := post(t, podA, raw); err != nil {
		t.Fatalf("pod A: %v", err)
	}
	// The SAME alert, re-delivered — and the load balancer sends it to the other pod.
	err := post(t, podB, raw)

	if got := pubA.commands(); got != 1 {
		t.Fatalf("pod A fanned out %d orders, want 1", got)
	}
	if got := pubB.commands(); got != 0 {
		t.Fatalf("POD B FANNED OUT %d ORDERS FOR AN ALERT POD A HAD ALREADY TRADED — a re-delivered "+
			"TradingView alert became a SECOND set of orders against a live exchange (err=%v)", got, err)
	}
}

// TestTwoPodsWithPerPodCachesDOUBLE_TRADE is the bug EXEC-M22 removes, kept executable.
//
// This is EXACTLY what production ran: `replicas: 1` was not a preference, it was the only
// thing standing between the platform and this. Each pod has its OWN nonce map — which is
// what the untagged binary gives you — and the re-delivered alert is admitted a second time.
//
// It asserts the DOUBLE TRADE, on purpose. If someone makes the in-process store cross-pod
// by some other means, this test fails and tells them to delete it. Until then it is the
// reason the Redis DSN, the `-tags redis` image and replicas > 1 move together or not at all.
func TestTwoPodsWithPerPodCachesDOUBLE_TRADE(t *testing.T) {
	pubA, pubB := &brokenPublisher{}, &brokenPublisher{}
	podA := pipelineOver(t, NewMemoryNonces(time.Minute, 1000), pubA) // pod A's own map
	podB := pipelineOver(t, NewMemoryNonces(time.Minute, 1000), pubB) // pod B's own map

	raw := body("buy", "1", "absolute_qty", "nonce-per-pod")

	if err := post(t, podA, raw); err != nil {
		t.Fatalf("pod A: %v", err)
	}
	if err := post(t, podB, raw); err != nil {
		t.Fatalf("pod B: %v", err)
	}

	if pubA.commands() != 1 || pubB.commands() != 1 {
		t.Fatalf("expected the per-pod double trade (1 and 1), got %d and %d — if this store is now "+
			"cross-pod, DELETE this test", pubA.commands(), pubB.commands())
	}
	// Two pods, one alert, TWO sets of orders against a live exchange. This is why the pin
	// existed, and why Redis had to land before it could come off.
}
