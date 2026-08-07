//go:build redis

// THE CROSS-POD DEDUPER, AGAINST A REAL REDIS AND A REAL OUTAGE (#111).
//
// dedup_redis_test.go drives RedisDedup against a FAKE — a map behind the
// bus.RedisClient interface. That proves the LOGIC (claim refuses a second
// claimant, commit suppresses, the lease bounds a ghost) and it cannot prove the
// one thing this component exists for: that `pkg/redisadapter` speaks Redis
// correctly. Specifically that ClaimNX really is an atomic `SET NX PX` and not a
// read-then-write — because a read-then-write passes every fake test ever
// written and reintroduces exactly the TOCTOU window the interface comment says
// it closes, only under concurrency, only in production.
//
// This is the same argument nonce_realredis_test.go makes for webhook-ingest's
// nonce store, applied to the seam that had not had it. RedisDedup is the
// N-replica dedup path for risk-engine, which runs `replicas: 3` plus a KEDA
// ScaledObject — a redelivery landing on a different pod than the original is
// the entire case it exists for.
//
// AND THE OUTAGE IS REAL, which is #111's actual bar ("validated under a real
// outage — not a simulated one"). The second test drives a real go-redis client,
// through the real adapter, at a port where nothing is listening: a genuine
// connection refusal from the operating system rather than a fake returning a
// canned error. What it asserts is the direction of the failure — dedup must
// FAIL OPEN. A dedup that fails CLOSED under an outage reports "already seen"
// for events nobody has seen, and the Consumer acks and discards them; the
// outage would silently delete events instead of duplicating them, and
// duplication is the recoverable direction.
//
// Gated on TEST_REDIS_URL and `-tags redis` (go-redis is only linked into that
// build). kanz/test/backing/up.sh provides the server.
package bus_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/redisadapter"
)

// realDedup returns a RedisDedup over a REAL Redis with a run-unique key prefix,
// so a parallel run of this suite cannot collide on a shared server.
func realDedup(t *testing.T, ttl time.Duration) *bus.RedisDedup {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("set TEST_REDIS_URL (and build with -tags redis) to run the real-Redis dedup tests")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse TEST_REDIS_URL: %v", err)
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("TEST_REDIS_URL is set but the server is unreachable: %v — this suite is about a REAL "+
			"server, so an unreachable one is a failure rather than a skip", err)
	}

	prefix := fmt.Sprintf("kanz:test:dedup:%d:", time.Now().UnixNano())
	d := bus.NewRedisDedup(redisadapter.New(client), ttl, bus.WithRedisDedupPrefix(prefix))
	if d == nil {
		t.Fatal("NewRedisDedup returned nil for a live client")
	}
	return d
}

// A SECOND CLAIM ON THE SAME KEY IS REFUSED BY THE SERVER.
func TestIntegration_RedisDedupClaimIsExclusive(t *testing.T) {
	d := realDedup(t, 30*time.Second)

	if !d.Claim("evt-1") {
		t.Fatal("the FIRST claim on a fresh key was refused — nothing has seen this event")
	}
	if d.Claim("evt-1") {
		t.Fatal("a SECOND claim on the same key was granted. On a real server this means ClaimNX is not " +
			"atomic — a read-then-write passes every fake-backed test and reopens the TOCTOU window " +
			"under concurrency, which is the whole reason this interface demands one round-trip")
	}
	// A different key is unaffected — otherwise the test above would pass against
	// an implementation that refuses everything after the first claim.
	if !d.Claim("evt-2") {
		t.Fatal("an unrelated key was refused, so the claim is not per-key")
	}
}

// CONCURRENT CLAIMANTS: EXACTLY ONE WINS.
//
// This is the assertion a fake cannot make honestly. The fake is a map behind a
// mutex, so it serialises by construction and "exactly one wins" is true of the
// test harness rather than of Redis. Here the exclusion is the server's.
func TestIntegration_RedisDedupExactlyOneConcurrentClaimantWins(t *testing.T) {
	d := realDedup(t, 30*time.Second)

	const racers = 16
	var wg sync.WaitGroup
	won := make(chan struct{}, racers)
	start := make(chan struct{})

	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if d.Claim("evt-race") {
				won <- struct{}{}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(won)

	if n := len(won); n != 1 {
		t.Fatalf("%d of %d concurrent claimants won the same key, want exactly 1.\n\n"+
			"More than one means two pods would both dispatch the same event. Zero means the claim is "+
			"refusing everyone, which would silently discard it.", n, racers)
	}
}

// THE LEASE EXPIRES ON THE SERVER, so a pod that dies holding a claim does not
// suppress its own redelivery forever. redisClaimLease is 5s and is pinned below
// the shortest AckWait for exactly this reason; here we prove the SERVER honours
// the expiry rather than that the constant is written down.
func TestIntegration_RedisDedupClaimLeaseExpiresOnTheServer(t *testing.T) {
	d := realDedup(t, 30*time.Second)

	if !d.Claim("evt-ghost") {
		t.Fatal("first claim refused")
	}
	if d.Claim("evt-ghost") {
		t.Fatal("the ghost claim is not held at all — the expiry assertion below would prove nothing")
	}

	// redisClaimLease is 5s; wait past it. Long, but this is the property that
	// stands between a crashed pod and permanent silent loss of its event.
	deadline := time.Now().Add(12 * time.Second)
	var reclaimed bool
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if d.Claim("evt-ghost") {
			reclaimed = true
			break
		}
	}
	if !reclaimed {
		t.Fatal("the claim never expired on the server within 12s. A pod that crashes holding a claim " +
			"would then have its own redelivery refused by its ghost, and the Consumer acks a refused " +
			"redelivery — the event is dropped by nobody having handled it")
	}
}

// COMMIT SUPPRESSES FOR THE FULL TTL, not merely for the claim lease — the
// difference between "in flight" and "already done".
func TestIntegration_RedisDedupCommitOutlivesTheClaimLease(t *testing.T) {
	d := realDedup(t, 30*time.Second)

	if !d.Claim("evt-done") {
		t.Fatal("first claim refused")
	}
	d.Commit("evt-done")

	// Past the 5s claim lease. A committed key must still be suppressed: the work
	// is finished, and re-dispatching it is a duplicate side effect.
	time.Sleep(7 * time.Second)
	if d.Claim("evt-done") {
		t.Fatal("a COMMITTED key was reclaimable after the claim lease expired. Commit must extend " +
			"suppression to the full TTL — otherwise every redelivery after 5s reprocesses work that " +
			"already completed, which for a risk fold is a double-counted position")
	}
}

// THE REAL OUTAGE, AND THE DIRECTION OF THE FAILURE (#111's bar).
//
// Not a fake returning a canned error: a real go-redis client, through the real
// redisadapter, against a port where nothing is listening — an actual connection
// refusal from the OS.
//
// Claim MUST return true. Failing open duplicates work, which the platform
// tolerates (delivery is at-least-once and the OMS's real arbiter is the version
// predicate on Store.Save). Failing CLOSED would report "already seen" for
// events nobody has seen, and the Consumer acks a refused redelivery — so a
// Redis outage would silently DELETE events. Duplication is recoverable; that is
// not.
func TestIntegration_RedisDedupFailsOpenWhenTheServerIsGone(t *testing.T) {
	if os.Getenv("TEST_REDIS_URL") == "" {
		t.Skip("set TEST_REDIS_URL (and build with -tags redis) to run the real-Redis dedup tests")
	}

	// A port that is genuinely closed: bind one, learn its number, release it.
	// Nothing is listening there afterwards, so the dial is refused by the kernel.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	dead := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}

	client := redis.NewClient(&redis.Options{
		Addr:        dead,
		DialTimeout: 500 * time.Millisecond,
		ReadTimeout: 500 * time.Millisecond,
		MaxRetries:  -1, // fail fast; the retry policy is not what is under test
	})
	t.Cleanup(func() { _ = client.Close() })

	var mu sync.Mutex
	var ops []string
	d := bus.NewRedisDedup(redisadapter.New(client), 30*time.Second,
		bus.WithRedisDedupPrefix("kanz:test:outage:"),
		bus.WithRedisDedupErrorHandler(func(op string, _ error) {
			mu.Lock()
			defer mu.Unlock()
			ops = append(ops, op)
		}))
	if d == nil {
		t.Fatal("NewRedisDedup returned nil")
	}

	if !d.Claim("evt-outage") {
		t.Fatal("Claim returned FALSE while Redis was unreachable — the deduper failed CLOSED.\n\n" +
			"That reports 'already seen' for an event nobody has seen. The Consumer acks a refused " +
			"claim and skips the dispatch, so an outage would silently discard events rather than " +
			"duplicate them. Duplication is recoverable; deletion is not.")
	}
	// Repeat: the outage must not become sticky in a way that flips the direction.
	if !d.Claim("evt-outage-2") {
		t.Fatal("a second claim during the outage was refused")
	}

	// Commit and Release must not panic or block on a dead server either — they
	// are called on the Consumer's hot path after every handler.
	d.Commit("evt-outage")
	d.Release("evt-outage-2")

	mu.Lock()
	defer mu.Unlock()
	if len(ops) == 0 {
		t.Fatal("the error handler was never invoked during a total outage — the degradation would be " +
			"SILENT, and an operator would see dedup 'working' while every claim is passing through")
	}
	seen := map[string]bool{}
	for _, op := range ops {
		seen[op] = true
	}
	for _, want := range []string{"claim", "commit", "release"} {
		if !seen[want] {
			t.Errorf("no %q error was reported during the outage (got %v) — that operation is failing "+
				"without telling anyone", want, ops)
		}
	}
}

// THE SERVER DIES MID-FLIGHT, WHICH IS THE OUTAGE PRODUCTION ACTUALLY HAS.
//
// The test above dials a port where nothing ever listened, so every attempt is a
// clean ECONNREFUSED. That is NOT the shape of a real outage: in production the
// pool holds ESTABLISHED connections to a server that then goes away, and what
// comes back is a broken pipe, an i/o timeout, or an EOF mid-reply — different
// errors, on a different code path, against sockets go-redis believes are good.
// A deduper can fail open on connection-refused and still fail closed on a
// half-open socket, and only the second one happens during a real incident.
//
// So this stops the actual server, with the pool already warm.
//
// OPT-IN via TEST_REDIS_CONTAINER (the container name) because it requires a
// container runtime and it BRIEFLY TAKES REDIS DOWN for anything else sharing
// it. It is not part of the normal suite for that reason; it is the evidence for
// #111's "validated under a real outage — not a simulated one".
//
//	TEST_REDIS_URL=redis://localhost:6379 TEST_REDIS_CONTAINER=kanz-ci-redis \
//	  go test -tags redis ./pkg/bus/ -run RedisDedupSurvivesTheServerDying -v
func TestIntegration_RedisDedupSurvivesTheServerDying(t *testing.T) {
	name := os.Getenv("TEST_REDIS_CONTAINER")
	if name == "" {
		t.Skip("set TEST_REDIS_CONTAINER (with TEST_REDIS_URL) to stop the real server mid-test")
	}
	d := realDedup(t, 30*time.Second)

	// Warm the pool and establish the happy path, so what follows is a LOSS of a
	// working connection rather than a connection that never worked.
	if !d.Claim("evt-preoutage") {
		t.Fatal("the pre-outage claim was refused — the server is not healthy, so the outage below " +
			"would prove nothing")
	}

	dockerCmd(t, "stop", name)
	// Always bring it back, even if an assertion fails: leaving the rig's Redis
	// stopped would silently break every later suite in this session.
	t.Cleanup(func() { dockerCmd(t, "start", name) })

	// Claim through a pool whose sockets are now dead. This is the assertion:
	// FAIL OPEN. Failing closed here reports "already seen" for unseen events and
	// the Consumer acks them — an outage that deletes rather than duplicates.
	for i, key := range []string{"evt-dying-1", "evt-dying-2", "evt-dying-3"} {
		if !d.Claim(key) {
			t.Fatalf("claim %d (%s) was REFUSED while the server was stopped — the deduper failed "+
				"CLOSED on a mid-flight outage. Connection-refused and broken-pipe are different code "+
				"paths; passing the closed-port test does not cover this one", i+1, key)
		}
	}
	d.Commit("evt-dying-1")
	d.Release("evt-dying-2")

	dockerCmd(t, "start", name)

	// AND IT RECOVERS. A deduper that fails open forever after one outage is a
	// deduper that has quietly stopped deduping, which on an N-replica group is
	// indistinguishable from not having one.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if !d.Claim("evt-recovered") || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if d.Claim("evt-recovered") {
		t.Fatal("after the server came back, the same key could be claimed twice — the deduper never " +
			"reconnected, so it is failing open permanently and no longer deduplicating anything")
	}
}

func dockerCmd(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
}
