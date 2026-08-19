package shard

import (
	"errors"
	"fmt"
	"testing"
)

// mustAssign builds an Assignment or fails: NewAssignment refuses a
// half-configured ring, and ignoring that error would test a nil value.
func mustAssign(t *testing.T, ring *Ring, self string) *Assignment {
	t.Helper()
	a, err := NewAssignment(ring, self)
	if err != nil {
		t.Fatalf("NewAssignment(%v, %q): %v", ring.Members(), self, err)
	}
	return a
}

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
		assigns[m] = mustAssign(t, ring, m)
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

// NOTHING CONFIGURED is the one accepted unsharded posture: no members AND no
// id. Owns is always true and Sharded reports false.
func TestAssignment_UnshardedOwnsEverything(t *testing.T) {
	for _, tc := range []struct {
		name string
		ring *Ring
	}{
		{"nil ring", nil},
		{"empty members", NewRing(nil, 0)},
	} {
		a := mustAssign(t, tc.ring, "")
		if a.Sharded() {
			t.Errorf("%s: Sharded()=true want false", tc.name)
		}
		if !a.Owns("anything") {
			t.Errorf("%s: expected to own everything", tc.name)
		}
	}
}

// EVERY HALF-CONFIGURED RING IS REFUSED (#110). Each of these once produced a
// replica that consumed the whole spine and applied none of it (or silently
// owned all of it) while its probes stayed green; construction now fails and
// the composition root exits.
func TestNewAssignment_RefusesHalfConfigured(t *testing.T) {
	ring := NewRing([]string{"r0", "r1"}, 0)
	for _, tc := range []struct {
		name string
		ring *Ring
		self string
		want error
	}{
		{"id absent from the member list", ring, "r9", ErrSelfNotAMember},
		{"id differs only by whitespace-free typo", ring, "r0-0", ErrSelfNotAMember},
		{"members configured, no id", ring, "", ErrSelfUnset},
		{"id configured, no members", NewRing(nil, 0), "r0", ErrMembersUnset},
		{"id configured, nil ring", nil, "r0", ErrMembersUnset},
	} {
		a, err := NewAssignment(tc.ring, tc.self)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v, want %v", tc.name, err, tc.want)
		}
		if a != nil {
			t.Errorf("%s: a refused Assignment must be nil, got %+v", tc.name, a)
		}
	}
}

// The id is trimmed, because the member list is: an id that matches no member
// owns nothing, and whitespace must not be the reason.
func TestNewAssignment_TrimsSelf(t *testing.T) {
	a, err := NewAssignment(NewRing([]string{"r0", "r1"}, 0), "  r0\n")
	if err != nil {
		t.Fatalf("a padded id must still match its member: %v", err)
	}
	if !a.Sharded() {
		t.Fatal("expected Sharded() true")
	}
}

// A populated ring reports Sharded and pins ownership to self.
func TestAssignment_ShardedGatesToSelf(t *testing.T) {
	ring := NewRing([]string{"r0", "r1"}, 0)
	a := mustAssign(t, ring, "r0")
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
