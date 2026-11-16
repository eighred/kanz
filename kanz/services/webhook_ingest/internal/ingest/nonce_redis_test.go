package ingest

// RedisNonces is the store that lets webhook-ingest run more than one pod. It speaks
// bus.RedisClient — the same minimal interface bus.RedisDedup uses — so it is driven
// here against a fake, hermetically: the claim's atomicity, the cross-pod replay, and
// the one behaviour that made this a separate type from bus.RedisDedup — IT FAILS
// CLOSED.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeRedis is a Redis: one map, shared by every pod that points at it. down makes
// every call fail, the way a real one does.
type fakeRedis struct {
	mu   sync.Mutex
	keys map[string]bool
	down bool
}

func newFakeRedis() *fakeRedis { return &fakeRedis{keys: map[string]bool{}} }

func (f *fakeRedis) ClaimNX(_ context.Context, key string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return false, errors.New("dial tcp: connection refused")
	}
	if f.keys[key] {
		return false, nil
	}
	f.keys[key] = true
	return true, nil
}

func (f *fakeRedis) SetWithTTL(_ context.Context, key string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errors.New("dial tcp: connection refused")
	}
	f.keys[key] = true
	return nil
}

func (f *fakeRedis) Del(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errors.New("dial tcp: connection refused")
	}
	delete(f.keys, key)
	return nil
}

// TestRedisNoncesSpanPods: two pods, one Redis. The alert pod A traded is a replay at
// pod B — which is the whole reason this store exists, and what lifts the single-replica
// pin off the one service the internet talks to.
func TestRedisNoncesSpanPods(t *testing.T) {
	rdb := newFakeRedis()
	pubA, pubB := &brokenPublisher{}, &brokenPublisher{}
	podA := pipelineOver(t, NewRedisNonces(rdb, time.Minute, time.Second), pubA)
	podB := pipelineOver(t, NewRedisNonces(rdb, time.Minute, time.Second), pubB)

	raw := body("buy", "1", "absolute_qty", "redis-1")

	if err := post(t, podA, raw); err != nil {
		t.Fatalf("pod A rejected a good alert: %v", err)
	}
	if err := post(t, podB, raw); !errors.Is(err, ErrReplayed) {
		t.Fatalf("pod B admitted an alert pod A already traded: %v", err)
	}
	if n := pubB.commands(); n != 0 {
		t.Fatalf("pod B fanned out %d orders — a DOUBLE TRADE across replicas", n)
	}
}

// TestRedisNoncesFailCLOSED is why this is not bus.RedisDedup.
//
// bus.RedisDedup fails OPEN — a Redis error makes Claim return true — and for bus dedup
// that is right: it suppresses duplicate WORK, and an outage should degrade to doing the
// work twice rather than halting consumption. Here the store IS the replay defence on
// the path that submits orders to a live exchange. "I cannot tell whether this alert
// already traded" must never resolve to "trade it" — the same stance the halt gate takes
// when it loses the bus (EXEC-M6).
//
// So: no orders, and the error is NOT ErrReplayed. That distinction is load-bearing —
// the server answers 503 (retryable) rather than 409, because telling TradingView its
// alert was a duplicate would drop a live trading signal on the floor over a Redis blink.
func TestRedisNoncesFailCLOSED(t *testing.T) {
	rdb := newFakeRedis()
	rdb.down = true
	pub := &brokenPublisher{}
	p := pipelineOver(t, NewRedisNonces(rdb, time.Minute, time.Second), pub)

	raw := body("buy", "1", "absolute_qty", "redis-2")
	err := post(t, p, raw)

	if err == nil {
		t.Fatal("the alert was TRADED while the replay defence was unreachable")
	}
	if !errors.Is(err, ErrNonceStoreUnavailable) {
		t.Errorf("err = %v, want ErrNonceStoreUnavailable", err)
	}
	if errors.Is(err, ErrReplayed) {
		t.Error("a Redis outage was reported as a REPLAY — the caller would give up on a live signal")
	}
	if n := pub.commands(); n != 0 {
		t.Errorf("orders fanned out = %d, want 0 — an unverifiable alert must not trade", n)
	}
}

// TestRedisNoncesRecover: the outage ends and the alert — never traded — is admitted.
// A fail-closed refusal must be a PAUSE, not a permanent rejection.
func TestRedisNoncesRecover(t *testing.T) {
	rdb := newFakeRedis()
	rdb.down = true
	pub := &brokenPublisher{}
	p := pipelineOver(t, NewRedisNonces(rdb, time.Minute, time.Second), pub)

	raw := body("buy", "1", "absolute_qty", "redis-3")
	if err := post(t, p, raw); !errors.Is(err, ErrNonceStoreUnavailable) {
		t.Fatalf("err = %v, want ErrNonceStoreUnavailable", err)
	}

	rdb.down = false
	if err := post(t, p, raw); err != nil {
		t.Fatalf("after Redis recovered, the re-delivered alert = %v, want admitted", err)
	}
	if n := pub.commands(); n != 1 {
		t.Errorf("orders = %d, want 1", n)
	}
}

// TestRedisNonceClaimIsAtomic: the store must never do Exists-then-Set. Two pods racing
// one re-delivered alert issue the same single-round-trip claim, and exactly one wins.
func TestRedisNonceClaimIsAtomic(t *testing.T) {
	rdb := newFakeRedis()
	store := NewRedisNonces(rdb, time.Minute, time.Second)

	const pods = 32
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := 0; i < pods; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.Claim(context.Background(), "momentum:race-1")
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d pods claimed the same alert, want exactly 1 — every extra winner is a double trade", wins)
	}
}
