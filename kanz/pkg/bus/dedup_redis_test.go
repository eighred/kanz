package bus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/pkg/bus"
)

// fakeRedis is an in-memory bus.RedisClient for testing RedisDedup without a
// real Redis: a key→expiry map with an injectable clock + failure switches.
type fakeRedis struct {
	mu         sync.Mutex
	data       map[string]time.Time
	now        func() time.Time
	failExists bool
	failSet    bool
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{data: map[string]time.Time{}, now: time.Now}
}

func (f *fakeRedis) Exists(_ context.Context, key string) (bool, error) {
	if f.failExists {
		return false, errors.New("redis down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	exp, ok := f.data[key]
	if !ok {
		return false, nil
	}
	if !f.now().Before(exp) { // expired
		delete(f.data, key)
		return false, nil
	}
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

func TestRedisDedup_RecordThenSeen(t *testing.T) {
	d := bus.NewRedisDedup(newFakeRedis(), time.Minute)
	if d.Seen("k1") {
		t.Fatal("Seen before Record should be false")
	}
	d.Record("k1")
	if !d.Seen("k1") {
		t.Error("Seen after Record should be true")
	}
}

// The whole point of DEBT-02a: dedup spans instances. Two RedisDedup values
// (two "pods") over ONE shared client — a key recorded by one is seen by the
// other, which DedupWindow (per-instance) cannot do.
func TestRedisDedup_CrossInstance(t *testing.T) {
	shared := newFakeRedis()
	podA := bus.NewRedisDedup(shared, time.Minute)
	podB := bus.NewRedisDedup(shared, time.Minute)

	podA.Record("evt-7")
	if !podB.Seen("evt-7") {
		t.Error("podB should see a key podA recorded (cross-instance dedup)")
	}
}

func TestRedisDedup_TTLExpiry(t *testing.T) {
	fake := newFakeRedis()
	clk := time.Unix(1000, 0)
	fake.now = func() time.Time { return clk }
	d := bus.NewRedisDedup(fake, 50*time.Second)

	d.Record("k")
	if !d.Seen("k") {
		t.Fatal("Seen within TTL should be true")
	}
	clk = clk.Add(51 * time.Second) // advance past the TTL
	if d.Seen("k") {
		t.Error("Seen after TTL should be false")
	}
}

// A Redis outage must fail OPEN: Seen=false, Record swallowed — the consumer
// keeps running on handler idempotency, never blocks. The error hook fires.
func TestRedisDedup_FailOpen(t *testing.T) {
	fake := &fakeRedis{data: map[string]time.Time{}, now: time.Now, failExists: true, failSet: true}
	var gotOps []string
	d := bus.NewRedisDedup(fake, time.Minute,
		bus.WithRedisDedupErrorHandler(func(op string, _ error) { gotOps = append(gotOps, op) }))

	if d.Seen("k") {
		t.Error("Seen must fail open (false) on a Redis error")
	}
	d.Record("k") // must not panic; best-effort
	if len(gotOps) != 2 || gotOps[0] != "seen" || gotOps[1] != "record" {
		t.Errorf("error hook ops = %v, want [seen record]", gotOps)
	}
}

func TestRedisDedup_PrefixIsolation(t *testing.T) {
	fake := newFakeRedis()
	d := bus.NewRedisDedup(fake, time.Minute, bus.WithRedisDedupPrefix("env1:"))
	d.Record("k")
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
		if d.Seen("k") { // nil-receiver no-op
			t.Error("nil RedisDedup.Seen should be false")
		}
		d.Record("k") // nil-receiver no-op, must not panic
	}
}

func TestRedisDedup_EmptyKey(t *testing.T) {
	d := bus.NewRedisDedup(newFakeRedis(), time.Minute)
	d.Record("")
	if d.Seen("") {
		t.Error("empty key must be ignored")
	}
}
