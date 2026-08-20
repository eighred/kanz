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
//
// That is also the weakness, and it is why NewAssignment validates rather than
// accepts: with no coordination there is nothing to notice that a list is
// wrong. A list naming a pod that is not running leaves those portfolios owned
// by nobody and silently unrevalued; a replica whose id is not in the list owns
// nothing at all. Neither is observable from the ring, so the pairings that
// cause them are refused at construction instead (#110).
package shard

import (
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
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
	if r == nil {
		return nil
	}
	out := make([]string, len(r.members))
	copy(out, r.members)
	return out
}

// Has reports whether member is on the ring. Linear over the member set,
// which is a fleet-sized list — this is a startup check, never a hot path.
func (r *Ring) Has(member string) bool {
	if r == nil {
		return false
	}
	for _, m := range r.members {
		if m == member {
			return true
		}
	}
	return false
}

// A HALF-CONFIGURED RING IS THE FAILURE THIS PACKAGE CANNOT ABSORB (#110).
//
// Ownership is a pure function of (members, self), and every wrong pairing of
// those two produces a replica that keeps passing its probes, keeps acking its
// deliveries, and publishes measures computed from a book it never assembled.
// None of the three below is detectable from a metric that reads 0 vs 1, so
// they are refused at construction and the composition root turns them into a
// refusal to start:
//
//   - self is absent from the member list. Owns() is false for EVERY key, so
//     the replica drops every event it receives and its risk store stays empty.
//     A restored snapshot then ages in place and is republished on each
//     recompute. This is the "a wrong list is worse than none" case, and it is
//     what an operator gets from one stale pod name in a hand-written list.
//   - members are configured but self is empty. The ring is real and nobody is
//     on it, so the replica silently reverts to owning everything — the exact
//     unsharded posture the operator was configuring their way out of.
//   - self is configured but the member list is empty. Same outcome, opposite
//     typo, and equally invisible.
//
// A ring that is empty AND has no self is not an error: that is the deliberate
// single-instance default (see ShardPosture, which WARNs about it by name).
var (
	// ErrSelfNotAMember: this replica's id is missing from the ring it was
	// handed, so it would own nothing and apply nothing.
	ErrSelfNotAMember = errors.New("shard: this replica's id is absent from the member list")
	// ErrSelfUnset: a member list without an identity to match against it.
	ErrSelfUnset = errors.New("shard: a member list is configured but this replica has no id on it")
	// ErrMembersUnset: an identity with no ring to place it on.
	ErrMembersUnset = errors.New("shard: this replica has an id but the member list is empty")
)

// Assignment binds a Ring to the local replica's member id and answers the
// hot-path question: does this replica own key?
type Assignment struct {
	ring *Ring
	self string
}

// NewAssignment binds ring to the self member, refusing every pairing that
// would silently mis-shard (see the block above). A nil/empty ring with an
// empty self is the unsharded single-replica default and returns an Assignment
// that owns everything, so callers can wire it unconditionally.
func NewAssignment(ring *Ring, self string) (*Assignment, error) {
	self = strings.TrimSpace(self)
	populated := ring != nil && len(ring.members) > 0
	switch {
	case !populated && self == "":
		return &Assignment{}, nil // unsharded: owns everything
	case !populated:
		return nil, fmt.Errorf("%w (id %q)", ErrMembersUnset, self)
	case self == "":
		return nil, fmt.Errorf("%w (members %v)", ErrSelfUnset, ring.Members())
	case !ring.Has(self):
		return nil, fmt.Errorf("%w: %q not in %v", ErrSelfNotAMember, self, ring.Members())
	}
	return &Assignment{ring: ring, self: self}, nil
}

// Owns reports whether the local replica owns key. When the ring is empty
// (unsharded), every key is owned locally.
func (a *Assignment) Owns(key string) bool {
	if a.ring == nil || len(a.ring.points) == 0 {
		return true
	}
	return a.ring.Owner(key) == a.self
}

// Owner names the member the ring assigns key to, or "" when unsharded.
//
// It exists so a refusal can say WHERE the answer is. A replica that declines a
// query for a portfolio it does not own gives the caller a dead end unless it
// also names the owner; with the name, a caller (or an operator reading a log)
// can tell "this portfolio is elsewhere" from "this portfolio does not exist",
// which are the two readings of a bare not-found and only one of them is true.
func (a *Assignment) Owner(key string) string {
	if a.ring == nil {
		return ""
	}
	return a.ring.Owner(key)
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
