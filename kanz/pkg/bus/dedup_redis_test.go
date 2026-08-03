package bus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
)

// fakeRedis is an in-memory bus.RedisClient for testing RedisDedup without a
// real Redis: a key→expiry map with an injectable clock + failure switches.
//
// ClaimNX holds the mutex across the check AND the set, which is what real Redis
// gives you for free with a single SET NX EX. A fake that split them would be
// modelling a Redis that does not exist and would hide the very race the claim
// protocol exists to close.
type fakeRedis struct {
	mu        sync.Mutex
	data      map[string]time.Time
	now       func() time.Time
	failClaim bool
	failSet   bool
	failDel   bool
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{data: map[string]time.Time{}, now: time.Now}
}

func (f *fakeRedis) ClaimNX(_ context.Context, key string, lease time.Duration) (bool, error) {
	if f.failClaim {
		return false, errors.New("redis down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if exp, ok := f.data[key]; ok && f.now().Before(exp) {
		return false, nil // held
	}
	f.data[key] = f.now().Add(lease)
	return true, nil
}

func (f *fakeRedis) SetWithTTL(_ context.Context, key string, ttl time.Duration) error {
	if f.failSet {
		return errors.New("redis down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = f.now().Add(ttl)
	return nil
}

func (f *fakeRedis) Del(_ context.Context, key string) error {
	if f.failDel {
		return errors.New("redis down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, key)
	return nil
}

func TestRedisDedup_ClaimIsExclusive(t *testing.T) {
	d := bus.NewRedisDedup(newFakeRedis(), time.Minute)
	if !d.Claim("k1") {
		t.Fatal("first Claim should succeed")
	}
	if d.Claim("k1") {
		t.Error("second Claim of a held key must fail — that is the whole guarantee")
	}
	d.Commit("k1")
	if d.Claim("k1") {
		t.Error("Claim of a committed key must fail for the dedup window")
	}
}

// A failed dispatch releases, so the redelivery gets a real retry. This is the
// property the old Seen/Record split protected and that a bare SETNX would have
// destroyed: claim-and-never-release turns a handler failure into a lost event.
func TestRedisDedup_ReleaseAllowsRetry(t *testing.T) {
	d := bus.NewRedisDedup(newFakeRedis(), time.Minute)
	if !d.Claim("k1") {
		t.Fatal("first Claim should succeed")
	}
	d.Release("k1") // dispatch failed
	if !d.Claim("k1") {
		t.Error("a released key must be claimable again — the redelivery would be lost otherwise")
	}
}

// The crash window. A worker that claims and dies never Commits and never
// Releases; the lease must expire so the event is reprocessed rather than
// silently dropped forever.
func TestRedisDedup_LeaseExpiryRecoversAbandonedClaim(t *testing.T) {
	fake := newFakeRedis()
	clk := time.Unix(1000, 0)
	fake.now = func() time.Time { return clk }
	// No lease option: redisClaimLease (5s) is fixed by construction and is
	// pinned below the shortest AckWait precisely so this recovery works — see
	// its comment in dedup_redis.go.
	d := bus.NewRedisDedup(fake, time.Minute)

	if !d.Claim("k") {
		t.Fatal("first Claim should succeed")
	}
	clk = clk.Add(2 * time.Second)
	if d.Claim("k") {
		t.Error("claim must still be held inside its lease")
	}
	clk = clk.Add(4 * time.Second) // now past the 5s lease
	if !d.Claim("k") {
		t.Error("an abandoned claim must expire with its lease — the event would be lost forever otherwise")
	}
}

// The whole point of DEBT-02a: dedup spans instances. Two RedisDedup values
// (two "pods") over ONE shared client — a key claimed by one cannot be claimed
// by the other, which DedupWindow (per-instance) cannot do.
func TestRedisDedup_CrossInstance(t *testing.T) {
	shared := newFakeRedis()
	podA := bus.NewRedisDedup(shared, time.Minute)
	podB := bus.NewRedisDedup(shared, time.Minute)

	if !podA.Claim("evt-7") {
		t.Fatal("podA should claim a fresh key")
	}
	if podB.Claim("evt-7") {
		t.Error("podB claimed a key podA holds — cross-pod dedup is not working")
	}
	podA.Commit("evt-7")
	if podB.Claim("evt-7") {
		t.Error("podB claimed a key podA committed")
	}
}

func TestRedisDedup_CommitTTLExpiry(t *testing.T) {
	fake := newFakeRedis()
	clk := time.Unix(1000, 0)
	fake.now = func() time.Time { return clk }
	d := bus.NewRedisDedup(fake, 50*time.Second)

	if !d.Claim("k") {
		t.Fatal("Claim should succeed")
	}
	d.Commit("k")
	clk = clk.Add(51 * time.Second) // past the committed TTL
	if !d.Claim("k") {
		t.Error("a key past its dedup window must be claimable again")
	}
}

// A Redis outage must fail OPEN: Claim=true (proceed), Commit/Release swallowed —
// the consumer keeps running, never blocks. The error hook fires.
func TestRedisDedup_FailOpen(t *testing.T) {
	fake := &fakeRedis{
		data: map[string]time.Time{}, now: time.Now,
		failClaim: true, failSet: true, failDel: true,
	}
	var gotOps []string
	d := bus.NewRedisDedup(fake, time.Minute,
		bus.WithRedisDedupErrorHandler(func(op string, _ error) { gotOps = append(gotOps, op) }))

	if !d.Claim("k") {
		t.Error("Claim must fail OPEN (true) on a Redis error — a Redis outage must not stall consumption")
	}
	d.Commit("k")  // must not panic; best-effort
	d.Release("k") // must not panic; best-effort
	want := []string{"claim", "commit", "release"}
	if len(gotOps) != 3 || gotOps[0] != want[0] || gotOps[1] != want[1] || gotOps[2] != want[2] {
		t.Errorf("error hook ops = %v, want %v", gotOps, want)
	}
}

func TestRedisDedup_PrefixIsolation(t *testing.T) {
	fake := newFakeRedis()
	d := bus.NewRedisDedup(fake, time.Minute, bus.WithRedisDedupPrefix("env1:"))
	d.Claim("k")
	fake.mu.Lock()
	_, ok := fake.data["env1:k"]
	fake.mu.Unlock()
	if !ok {
		t.Error("key should be stored under the configured prefix")
	}
}

func TestRedisDedup_DisabledAndNilSafe(t *testing.T) {
	// nil client or non-positive ttl ⇒ nil RedisDedup, whose methods are no-ops.
	for _, d := range []*bus.RedisDedup{
		bus.NewRedisDedup(nil, time.Minute),
		bus.NewRedisDedup(newFakeRedis(), 0),
	} {
		if d != nil {
			t.Fatalf("expected nil (disabled) RedisDedup")
		}
		// Dedup disabled ⇒ every delivery must PROCEED, so a nil Claim is true.
		if !d.Claim("k") {
			t.Error("nil RedisDedup.Claim must be true — a disabled deduper must not swallow every event")
		}
		d.Commit("k")  // nil-receiver no-op, must not panic
		d.Release("k") // nil-receiver no-op, must not panic
	}
}

func TestRedisDedup_EmptyKey(t *testing.T) {
	d := bus.NewRedisDedup(newFakeRedis(), time.Minute)
	// No idempotency key ⇒ nothing to dedup on ⇒ the dispatch proceeds.
	if !d.Claim("") {
		t.Error("an empty key must not block the dispatch")
	}
	d.Commit("")
	d.Release("")
}

// Concurrency is the reason this type exists: N goroutines racing one key must
// yield exactly ONE winner. Run with -race.
func TestRedisDedup_ConcurrentClaimHasExactlyOneWinner(t *testing.T) {
	d := bus.NewRedisDedup(newFakeRedis(), time.Minute)
	const racers = 64

	var mu sync.Mutex
	won := 0
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.Claim("hot-key") {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if won != 1 {
		t.Fatalf("%d of %d concurrent claimants won, want exactly 1 — each extra winner is a duplicate dispatch (on the order path, a double trade)", won, racers)
	}
}
