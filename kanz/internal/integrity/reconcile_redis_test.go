package integrity_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/internal/integrity"
)

// fakeEval is an in-memory stand-in for a Redis server: it interprets the three
// PendingStore scripts against a Go map, faithfully to the documented Lua, and
// returns the exact reply shapes go-redis produces (string / int64 / []any of
// strings). It dispatches on arg arity — claim(3), sweep(1), count(0) — so the
// test never needs the unexported script constants. This exercises the store's
// encode/decode round-trip; the real atomicity is a Redis guarantee (out of
// unit-test scope, as with RedisDedup's fake client).
type fakeEval struct {
	pending map[string]string // idempotency_key -> "<transportInt>:<firstSeenNano>"
	err     error             // if set, every Eval fails (outage simulation)
	claims  int
}

func newFakeEval() *fakeEval { return &fakeEval{pending: map[string]string{}} }

func (f *fakeEval) Eval(_ context.Context, _ string, _ []string, args ...any) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	switch len(args) {
	case 3: // claim: key, transportInt, nowNano
		f.claims++
		field, transport, nano := args[0].(string), args[1].(string), args[2].(string)
		v, ok := f.pending[field]
		if !ok {
			f.pending[field] = transport + ":" + nano
			return "P", nil
		}
		if strings.SplitN(v, ":", 2)[0] == transport {
			return "D", nil
		}
		delete(f.pending, field)
		return "M:" + v, nil
	case 1: // sweep: cutoffNano
		cutoff, _ := strconv.ParseInt(args[0].(string), 10, 64)
		var out []any
		for field, v := range f.pending {
			seen, _ := strconv.ParseInt(strings.SplitN(v, ":", 2)[1], 10, 64)
			if seen < cutoff {
				out = append(out, v+":"+field)
				delete(f.pending, field)
			}
		}
		return out, nil
	default: // count
		return int64(len(f.pending)), nil
	}
}

// The Redis-backed store is a drop-in for the reconciler: two replicas over one
// shared store reconcile a NATS/Kafka split across instances (PARITY-04h →
// PARITY-05c), mirroring TestSharedStoreReconcilesCrossInstance for the
// in-memory store.
func TestRedisPendingStoreReconcilesCrossInstance(t *testing.T) {
	store := integrity.NewRedisPendingStore(newFakeEval())
	now := time.Unix(100, 0)
	clock := func() time.Time { return now }

	podA := integrity.NewReconcilerWithStore(store, time.Minute, clock)
	podB := integrity.NewReconcilerWithStore(store, time.Minute, clock)

	if got := podA.Observe(integrity.TransportNATS, envWithKey("evt-1")); got.Status != integrity.ReconcilePending {
		t.Fatalf("podA NATS: status=%v want pending", got.Status)
	}
	got := podB.Observe(integrity.TransportKafka, envWithKey("evt-1"))
	if got.Status != integrity.ReconcileMatched {
		t.Fatalf("podB Kafka: status=%v want matched (cross-instance)", got.Status)
	}
	if got.FirstTransport != integrity.TransportNATS {
		t.Errorf("FirstTransport=%v want nats", got.FirstTransport)
	}
	if store.Count() != 0 {
		t.Errorf("pending after match = %d want 0", store.Count())
	}
}

func TestRedisPendingStoreDuplicateKeepsFirstSeen(t *testing.T) {
	store := integrity.NewRedisPendingStore(newFakeEval())
	base := time.Unix(100, 0)

	if s, _, _ := store.ClaimOrMatch("k", integrity.TransportNATS, base); s != integrity.ReconcilePending {
		t.Fatalf("first claim = %v want pending", s)
	}
	// Redelivery on the SAME transport later ⇒ duplicate, and the deadline must
	// still measure from the FIRST sighting, so a sweep at base+deadline expires it.
	if s, _, _ := store.ClaimOrMatch("k", integrity.TransportNATS, base.Add(20*time.Second)); s != integrity.ReconcileDuplicate {
		t.Fatalf("redelivery = %v want duplicate", s)
	}
	disc := store.SweepExpired(30*time.Second, base.Add(31*time.Second))
	if len(disc) != 1 {
		t.Fatalf("sweep returned %d discrepancies want 1", len(disc))
	}
	if disc[0].Key != "k" || disc[0].SeenOn != integrity.TransportNATS || disc[0].MissingOn != integrity.TransportKafka {
		t.Errorf("discrepancy = %+v want key=k seen=nats missing=kafka", disc[0])
	}
	if !disc[0].FirstSeen.Equal(base) {
		t.Errorf("FirstSeen = %v want %v (first sighting, not the redelivery)", disc[0].FirstSeen, base)
	}
	if store.Count() != 0 {
		t.Errorf("count after sweep = %d want 0", store.Count())
	}
}

func TestRedisPendingStoreSweepKeepsFresh(t *testing.T) {
	store := integrity.NewRedisPendingStore(newFakeEval())
	base := time.Unix(100, 0)
	store.ClaimOrMatch("old", integrity.TransportNATS, base)
	store.ClaimOrMatch("new", integrity.TransportKafka, base.Add(25*time.Second))

	disc := store.SweepExpired(30*time.Second, base.Add(31*time.Second))
	if len(disc) != 1 || disc[0].Key != "old" {
		t.Fatalf("sweep = %+v want only the aged-out 'old'", disc)
	}
	if store.Count() != 1 {
		t.Errorf("count = %d want 1 (fresh entry retained)", store.Count())
	}
}

// A Redis outage degrades to ReconcilePending (records nothing) rather than
// fabricating a match, and reports the error via the hook.
func TestRedisPendingStoreFailsClosedToPending(t *testing.T) {
	fe := newFakeEval()
	fe.err = errors.New("redis down")
	var ops []string
	store := integrity.NewRedisPendingStore(fe, integrity.WithRedisPendingErrorHandler(
		func(op string, _ error) { ops = append(ops, op) }))

	s, ft, _ := store.ClaimOrMatch("k", integrity.TransportNATS, time.Unix(1, 0))
	if s != integrity.ReconcilePending || ft != integrity.TransportUnspecified {
		t.Fatalf("outage claim = (%v,%v) want (pending,unspecified)", s, ft)
	}
	if store.SweepExpired(time.Second, time.Unix(9, 0)) != nil {
		t.Error("sweep on outage should return nil")
	}
	if store.Count() != 0 {
		t.Error("count on outage should be 0")
	}
	if len(ops) != 3 {
		t.Errorf("error hook ops = %v want [claim sweep count]", ops)
	}
}

func TestNewRedisPendingStoreNilEvalIsNil(t *testing.T) {
	if integrity.NewRedisPendingStore(nil) != nil {
		t.Fatal("nil eval should yield a nil store (in-memory default)")
	}
	// A nil store passed to the reconciler falls back to the in-memory default.
	r := integrity.NewReconcilerWithStore(nil, time.Minute, func() time.Time { return time.Unix(0, 0) })
	if got := r.Observe(integrity.TransportNATS, envWithKey("x")); got.Status != integrity.ReconcilePending {
		t.Fatalf("fallback reconciler status = %v want pending", got.Status)
	}
}
