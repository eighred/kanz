package compliance

import (
	"crypto/sha256"
	"sync"
)

// sayOnce is this package's ONE "say it once, count always" ledger: the memory
// behind every warn-the-first-time path on the pre-trade gate and in the mandate
// registry.
//
// # Why it is one type and not two maps
//
// It was two maps. PreTradeGate.firstTime and MandateRegistry.warnOnce were the
// same eleven lines twice, and when the first of them turned out to be keyed by
// something a caller supplies (#814), only one of the two copies would have been
// repaired. That is the failure mode CLAUDE.md names outright — a copied helper
// is how a fix stops spreading — and it is the same argument middleware.bucketSet
// makes one service out, where two leaking token-bucket maps became one type with
// the eviction discipline written once.
//
// # Why there are two key classes, and why the split is in the TYPE
//
// The two classes have genuinely different key spaces, and a single ledger over
// both is worse than either:
//
//   - stable keys are (tenant, portfolio). The estate controls them.
//   - volatile keys carry an instrument id, which is a free-form string off
//     SubmitOrder that nothing on the path to this gate validates beyond
//     "not empty".
//
// Holding them in one evicting map would let a stream of invented instrument ids
// push out the portfolio-level warnings, which then re-fire per order — turning a
// memory leak into a log flood, which is the trade this fix is not allowed to
// make. Holding them in one NON-evicting map is the leak. So the class is a
// property of the method you call, and a caller-supplied component cannot enter
// the ledger except through firstAbout.
//
// The zero value is usable — the maps are created on first write — so a type
// embedding this needs no constructor change.
type sayOnce struct {
	mu sync.Mutex

	// stable holds keys whose every variable part is (tenant, portfolio), and it
	// is deliberately NEVER evicted.
	//
	// BOUNDED BY CONSTRUCTION, not by being small today. An order only reaches
	// the gate after order.delegatedAndEntitled, so a "user:" issuer can name
	// only a portfolio the gateway stamped from its verified principal; the two
	// machine issuers that publish order.order.submit take theirs from a strategy
	// signal or a rebalance proposal, both in-estate. The registry's keys are
	// narrower still — warnMisfiled and warnSystemFallback only form one for a
	// portfolio that already HAS a published mandate.
	//
	// Evicting here would be actively wrong: an ungoverned portfolio is
	// ungoverned all day, so a horizon would re-announce it forever.
	stable map[string]bool

	// volatile holds the caller-keyed warnings as a fixed-width digest of the
	// whole key, mapped to the sequence number of the last sighting.
	//
	// THE DIGEST IS WHAT MAKES THE BOUND A BOUND, and it is not a security
	// device. A count ceiling over STRING keys bounds entries and not bytes: an
	// instrument id is capped only by the transport's message size, so 50,000
	// remembered keys could still be hundreds of megabytes. Sixteen bytes plus a
	// sequence is a fixed cost per entry that can be multiplied out and stated.
	// The price is a collision, which would silence one first warning; at this
	// ceiling that is a probability around 1e-13, against a certainty of
	// unbounded growth without it.
	volatile map[[16]byte]uint64

	// seq is the sighting counter that gives every volatile entry its recency.
	// It is monotonic and never reset; at one increment per REFUSED order a
	// uint64 outlives the pod by any margin worth writing down.
	seq uint64
}

// maxVolatileWarnings is the hard ceiling on caller-keyed warnings one ledger
// remembers.
//
// THE NUMBER IS TAKEN, NOT INVENTED. refdata.DefaultMaxEntries is 50,000 — the
// estate's own statement of the most instrument records one process will hold
// resident, from internal/refdata, the single classifier source, whose cache the
// composition root wires into this very gate. A set that exists only to suppress
// duplicate LOG LINES has no claim on more memory than the classifier that has
// to hold a full record per instrument, so it takes the same ceiling and not a
// byte more. Deriving it that way also means it moves if the estate's view of
// "how many instruments is a lot" ever moves.
//
// IT IS A BACKSTOP AND NOT THE WORKING BOUND, the same way maxRateBuckets is.
// The working set is the (portfolio, instrument) pairs that are UNPRICED right
// now — a handful on a healthy platform, and the whole traded book only during a
// price-feed outage. At roughly 32 bytes an entry the ceiling itself costs under
// two megabytes, so it is cheap to set far above the working set and let it stay
// unreached.
const maxVolatileWarnings = 50_000

// first reports whether key has not been named yet, and records it. For keys the
// estate controls; see the stable field.
func (s *sayOnce) first(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stable[key] {
		return false
	}
	if s.stable == nil {
		s.stable = map[string]bool{}
	}
	s.stable[key] = true
	return true
}

// firstAbout reports whether key has not been named yet ABOUT supplied — a
// component that came off the wire — and records it.
//
// WHAT EVICTION COSTS, SAID PLAINLY. A key dropped by the sweep re-warns the next
// time its instrument arrives. That is the right direction to fail, and the
// alternatives are both worse: remembering forever is the leak this replaces, and
// a fixed-size sketch (a bloom filter, a per-portfolio bitmask) would SILENCE a
// first warning that was never said — and a refusal nobody hears is the exact
// failure noteUngoverned and noteUnpriced were written to end.
//
// The re-warning is bounded rather than a flood, because recency is what
// survives: every sighting refreshes an entry's sequence, so an instrument that
// would spam the log is precisely the one the sweep keeps. What it drops is the
// tail nothing has mentioned in the last maxVolatileWarnings/2 sightings, and
// naming that again after such a gap is a reminder rather than noise.
func (s *sayOnce) firstAbout(key, supplied string) bool {
	// The separator is why ("unpriced:t:p", "X") and ("unpriced:t:pX", "") cannot
	// collapse onto one digest: without it a caller could silence another
	// portfolio's warning by choosing an instrument id, which is the same
	// cross-silencing #243 fixed by putting the tenant in the key.
	sum := sha256.Sum256([]byte(key + "\x00" + supplied))
	var d [16]byte
	copy(d[:], sum[:16])

	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if s.volatile == nil {
		s.volatile = map[[16]byte]uint64{}
	}
	if _, seen := s.volatile[d]; seen {
		s.volatile[d] = s.seq // sighted again: it keeps its place
		return false
	}
	if len(s.volatile) >= maxVolatileWarnings {
		s.sweep()
	}
	s.volatile[d] = s.seq
	return true
}

// sweep sheds every entry not sighted within the last maxVolatileWarnings/2
// sightings. Caller holds s.mu.
//
// WHY A GENERATIONAL SWEEP AND NOT AN EVICT-THE-OLDEST LRU. Evicting one entry
// means scanning the map for the minimum on EVERY insert once it is full — 50,000
// probes per refused order, driven by a caller who chose the instrument id. That
// converts a memory leak into a CPU one on the pre-trade path, which is a worse
// place to spend it. Shedding half in a single pass costs that scan once per
// maxVolatileWarnings/2 inserts: amortised constant time per order.
//
// THE BOUND IS HARD RATHER THAN HOPED FOR. Only maxVolatileWarnings/2 distinct
// sequence numbers lie at or above the cutoff and every entry holds a distinct
// one, so the map is guaranteed to be at most half full when this returns — the
// ceiling cannot be crossed by any arrival pattern.
func (s *sayOnce) sweep() {
	keep := uint64(maxVolatileWarnings / 2)
	var cutoff uint64
	if s.seq > keep {
		cutoff = s.seq - keep
	}
	for d, at := range s.volatile {
		if at < cutoff {
			delete(s.volatile, d)
		}
	}
}
