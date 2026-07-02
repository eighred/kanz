// Package shard partitions the portfolio universe across a fleet of
// risk-engine replicas by consistent hashing (PARITY-05a). Each replica
// owns a stable subset of portfolios; the owning replica applies live
// state and runs the RISK-05 recompute for its shard, while non-owners
// drop those events. This is the horizontal-scale seam: the recompute
// fan-out (the expensive per-portfolio work) is spread across replicas,
// with a portfolio's ownership pinned to one replica so its RISK-05
// per-aggregate lock and dedup window stay authoritative on a single node.
//
// # Why consistent hashing (not modulo)
//
// A plain hash(portfolio) % N remaps ~every portfolio when N changes, so a
// single replica added or lost during a rolling deploy would reshuffle the
// whole fleet — every replica cold-restarts its shard from the log. A
// consistent-hash ring with virtual nodes moves only ~1/N of portfolios per
// membership change, so scaling and rolling restarts stay cheap.
//
// # Determinism
//
// Every replica is configured with the SAME member list, so all of them
// compute an identical ring and agree on each portfolio's owner without any
// coordination. Ownership is a pure function of (members, key).
package shard

import (
	"hash/fnv"
	"sort"
)

// DefaultVNodes is the per-member virtual-node count. Higher spreads keys
// more evenly across members at the cost of a larger ring; 128 keeps the
// max/mean load imbalance small for the low-tens-of-replicas fleets the
// risk-engine scales to.
const DefaultVNodes = 128

// Ring is an immutable consistent-hash ring over a fixed member set. Safe
// for concurrent use — nothing mutates after NewRing.
type Ring struct {
	members []string
	points  []point // sorted by hash
}

type point struct {
	hash   uint64
	member string
}

// NewRing builds a ring placing vnodes virtual nodes per member. Members are
// de-duplicated; a non-positive vnodes falls back to DefaultVNodes. An empty
// member set yields a ring whose Owner always returns "" (Assignment treats
// that as "own everything" — the unsharded single-replica default).
func NewRing(members []string, vnodes int) *Ring {
	if vnodes <= 0 {
		vnodes = DefaultVNodes
	}
	seen := make(map[string]struct{}, len(members))
	uniq := make([]string, 0, len(members))
	for _, m := range members {
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		uniq = append(uniq, m)
	}
	r := &Ring{members: uniq}
	for _, m := range uniq {
		for v := 0; v < vnodes; v++ {
			r.points = append(r.points, point{hash: vnodeHash(m, v), member: m})
		}
	}
	sort.Slice(r.points, func(i, j int) bool { return r.points[i].hash < r.points[j].hash })
	return r
}

// Owner returns the member that owns key — the member of the first ring
// point clockwise from hash(key), wrapping past the end. Returns "" for an
// empty ring.
func (r *Ring) Owner(key string) string {
	if len(r.points) == 0 {
		return ""
	}
	h := keyHash(key)
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i].hash >= h })
	if i == len(r.points) {
		i = 0 // wrap
	}
	return r.points[i].member
}

// Members returns the ring's de-duplicated member set (construction order).
func (r *Ring) Members() []string {
	out := make([]string, len(r.members))
	copy(out, r.members)
	return out
}

// Assignment binds a Ring to the local replica's member id and answers the
// hot-path question: does this replica own key?
type Assignment struct {
	ring *Ring
	self string
}

// NewAssignment binds ring to the self member. A nil ring or a ring with no
// members yields an Assignment that owns everything (Owns always true) — the
// single-replica, unsharded default so callers need no special-casing.
func NewAssignment(ring *Ring, self string) *Assignment {
	return &Assignment{ring: ring, self: self}
}

// Owns reports whether the local replica owns key. When the ring is empty
// (unsharded), every key is owned locally.
func (a *Assignment) Owns(key string) bool {
	if a.ring == nil || len(a.ring.points) == 0 {
		return true
	}
	return a.ring.Owner(key) == a.self
}

// Sharded reports whether an effective shard split is in force (a non-empty
// ring). Callers use it to decide whether to switch to a per-replica
// broadcast consumer group.
func (a *Assignment) Sharded() bool {
	return a.ring != nil && len(a.ring.points) > 0
}

// vnodeHash hashes a member's v-th virtual node. The "\x00" separator keeps
// member "a" vnode 11 distinct from member "a1" vnode 1.
func vnodeHash(member string, v int) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(member))
	_, _ = h.Write([]byte{0})
	var buf [4]byte
	buf[0] = byte(v)
	buf[1] = byte(v >> 8)
	buf[2] = byte(v >> 16)
	buf[3] = byte(v >> 24)
	_, _ = h.Write(buf[:])
	return mix64(h.Sum64())
}

// keyHash hashes a portfolio key onto the ring.
func keyHash(key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return mix64(h.Sum64())
}

// mix64 is the splitmix64 finalizer. FNV-1a has weak avalanche on the short,
// near-identical inputs a ring uses (member ids differing in one byte, vnode
// indices in sequence), which clusters points and starves members. This
// bit-mixing step scrambles the output so placement is near-uniform.
func mix64(z uint64) uint64 {
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}
