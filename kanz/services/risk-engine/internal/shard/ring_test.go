package shard

import (
	"fmt"
	"testing"
)

// Ownership is a pure function of (members, key): two rings built from the
// same member set agree on every owner, regardless of construction order.
func TestRing_DeterministicAcrossReplicas(t *testing.T) {
	a := NewRing([]string{"r0", "r1", "r2"}, 0)
	b := NewRing([]string{"r2", "r0", "r1"}, 0) // different order, same set
	for i := 0; i < 1000; i++ {
		k := fmt.Sprintf("PORT-%d", i)
		if a.Owner(k) != b.Owner(k) {
			t.Fatalf("owner disagreement for %s: %q vs %q", k, a.Owner(k), b.Owner(k))
		}
	}
}

// Every key is owned by exactly one configured member.
func TestRing_OwnerAlwaysAMember(t *testing.T) {
	members := []string{"r0", "r1", "r2", "r3"}
	set := map[string]bool{}
	for _, m := range members {
		set[m] = true
	}
	r := NewRing(members, 0)
	for i := 0; i < 500; i++ {
		if o := r.Owner(fmt.Sprintf("k%d", i)); !set[o] {
			t.Fatalf("owner %q of k%d is not a member", o, i)
		}
	}
}

// Partition is complete and disjoint: across the fleet each portfolio is
// owned by exactly one replica's Assignment.
func TestAssignment_PartitionIsCompleteAndDisjoint(t *testing.T) {
	members := []string{"r0", "r1", "r2"}
	ring := NewRing(members, 0)
	assigns := map[string]*Assignment{}
	for _, m := range members {
		assigns[m] = NewAssignment(ring, m)
	}
	for i := 0; i < 2000; i++ {
		k := fmt.Sprintf("PORT-%d", i)
		owners := 0
		for _, a := range assigns {
			if a.Owns(k) {
				owners++
			}
		}
		if owners != 1 {
			t.Fatalf("%s owned by %d replicas, want exactly 1", k, owners)
		}
	}
}

// Consistent hashing moves only a small fraction of keys when one member is
// added — far below the ~2/3 a modulo scheme would reshuffle for 3→4.
func TestRing_MinimalMovementOnScaleUp(t *testing.T) {
	before := NewRing([]string{"r0", "r1", "r2"}, 0)
	after := NewRing([]string{"r0", "r1", "r2", "r3"}, 0)
	const n = 5000
	moved := 0
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("PORT-%d", i)
		if before.Owner(k) != after.Owner(k) {
			moved++
		}
	}
	frac := float64(moved) / float64(n)
	// Ideal is 1/4 = 0.25; allow generous slack for hash variance but assert
	// it is well under the modulo-scheme worst case.
	if frac > 0.40 {
		t.Fatalf("moved %.1f%% of keys on 3→4 scale-up, want <40%%", frac*100)
	}
}

// Load is reasonably balanced across members (no member starves or hogs).
func TestRing_BalancedLoad(t *testing.T) {
	members := []string{"r0", "r1", "r2", "r3", "r4"}
	r := NewRing(members, 0)
	counts := map[string]int{}
	const n = 20000
	for i := 0; i < n; i++ {
		counts[r.Owner(fmt.Sprintf("PORT-%d", i))]++
	}
	mean := float64(n) / float64(len(members))
	for m, c := range counts {
		dev := float64(c) / mean
		if dev < 0.6 || dev > 1.4 {
			t.Errorf("member %s load %d is %.2fx mean (%.0f) — imbalanced", m, c, dev, mean)
		}
	}
}

// An empty / single-member ring is the unsharded default: Owns is always
// true and Sharded reports false.
func TestAssignment_UnshardedOwnsEverything(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ring    *Ring
		self    string
		sharded bool
	}{
		{"nil ring", nil, "r0", false},
		{"empty members", NewRing(nil, 0), "r0", false},
	} {
		a := NewAssignment(tc.ring, tc.self)
		if a.Sharded() != tc.sharded {
			t.Errorf("%s: Sharded()=%v want %v", tc.name, a.Sharded(), tc.sharded)
		}
		if !a.Owns("anything") {
			t.Errorf("%s: expected to own everything", tc.name)
		}
	}
}

// A populated ring reports Sharded and pins ownership to self.
func TestAssignment_ShardedGatesToSelf(t *testing.T) {
	ring := NewRing([]string{"r0", "r1"}, 0)
	a := NewAssignment(ring, "r0")
	if !a.Sharded() {
		t.Fatal("expected Sharded() true for a populated ring")
	}
	var own, foreign int
	for i := 0; i < 1000; i++ {
		if a.Owns(fmt.Sprintf("k%d", i)) {
			own++
		} else {
			foreign++
		}
	}
	if own == 0 || foreign == 0 {
		t.Fatalf("expected a mix of owned/foreign keys, got own=%d foreign=%d", own, foreign)
	}
}
