package bus_test

import (
	"testing"
	"time"

	"github.com/kanz-eng/kanz/pkg/bus"
)

func TestDedupWindowSeenAfterRecord(t *testing.T) {
	w := bus.NewDedupWindow(time.Minute, 100)
	if w.Seen("k1") {
		t.Error("k1 reported Seen before any Record")
	}
	w.Record("k1")
	if !w.Seen("k1") {
		t.Error("k1 not Seen after Record")
	}
	if w.Seen("k2") {
		t.Error("k2 reported Seen but never Recorded")
	}
}

func TestDedupWindowEmptyKey(t *testing.T) {
	w := bus.NewDedupWindow(time.Minute, 100)
	if w.Seen("") {
		t.Error("empty key reported Seen")
	}
	w.Record("") // no-op
	if w.Seen("") {
		t.Error("empty key reported Seen after Record")
	}
}

func TestDedupWindowTTLExpiry(t *testing.T) {
	w := bus.NewDedupWindow(50*time.Millisecond, 100)
	w.Record("k")
	if !w.Seen("k") {
		t.Fatal("k not Seen immediately after Record")
	}
	time.Sleep(80 * time.Millisecond)
	if w.Seen("k") {
		t.Error("k still Seen past TTL")
	}
}

func TestDedupWindowCapacityEviction(t *testing.T) {
	w := bus.NewDedupWindow(time.Minute, 3)
	w.Record("a")
	time.Sleep(1 * time.Millisecond) // ensure ordering by Recorded time
	w.Record("b")
	time.Sleep(1 * time.Millisecond)
	w.Record("c")
	time.Sleep(1 * time.Millisecond)
	w.Record("d") // forces eviction; "a" is oldest
	if w.Seen("a") {
		t.Error("a should have been evicted (oldest)")
	}
	for _, k := range []string{"b", "c", "d"} {
		if !w.Seen(k) {
			t.Errorf("%s should still be Seen", k)
		}
	}
}

func TestNewDedupWindowDisabledNilSafe(t *testing.T) {
	// ttl=0 or max=0 ⇒ nil. Methods must no-op on nil receiver so the
	// Consumer can use it unconditionally.
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		max  int
	}{
		{"ttl=0", 0, 100},
		{"max=0", time.Minute, 0},
		{"both", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := bus.NewDedupWindow(tc.ttl, tc.max)
			if w != nil {
				t.Fatalf("expected nil window for %s", tc.name)
			}
			if w.Seen("k") {
				t.Error("nil Seen returned true")
			}
			w.Record("k") // must not panic
		})
	}
}
