package integrity

import (
	"sync"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// Transport names the two delivery paths a correctness-sensitive consumer
// reconciles: the NATS live spine (low-latency hot path, EVT-08) and the
// Kafka durable log of record (EVT-09).
type Transport int

const (
	// TransportUnspecified: zero value; an Observe with this is treated as
	// not-applicable (a caller must say which side an observation came from).
	TransportUnspecified Transport = iota
	TransportNATS
	TransportKafka
)

func (t Transport) String() string {
	switch t {
	case TransportNATS:
		return "nats"
	case TransportKafka:
		return "kafka"
	default:
		return "unspecified"
	}
}

// other returns the counterpart transport — the side an event seen on this
// transport still needs to be confirmed on. TransportUnspecified has no
// counterpart.
func (t Transport) other() Transport {
	switch t {
	case TransportNATS:
		return TransportKafka
	case TransportKafka:
		return TransportNATS
	default:
		return TransportUnspecified
	}
}

// ReconcileStatus classifies one observation against what has been seen on
// the two transports so far.
type ReconcileStatus int

const (
	// ReconcileNotApplicable: nil envelope, empty idempotency_key, or an
	// unspecified transport — nothing to reconcile. (idempotency_key is a
	// Required envelope field; a defensive reconciler reports not-applicable
	// rather than keying state on "".)
	ReconcileNotApplicable ReconcileStatus = iota

	// ReconcilePending: first sight of this key on this transport. The event
	// is now awaiting confirmation on the other transport. NOT a discrepancy
	// yet — the two paths have independent latencies (the NATS spine
	// normally leads the Kafka log), so a one-sided sighting is only a
	// problem once it ages past MatchDeadline (see Sweep).
	ReconcilePending

	// ReconcileMatched: the key was already pending from the OTHER transport,
	// so the event is now confirmed on both. The pending entry is cleared.
	ReconcileMatched

	// ReconcileDuplicate: the key was already pending from the SAME transport
	// — a redelivery before the counterpart arrived. The original first-seen
	// time is kept so the match deadline still measures from the true first
	// sighting.
	ReconcileDuplicate
)

func (s ReconcileStatus) String() string {
	switch s {
	case ReconcilePending:
		return "pending"
	case ReconcileMatched:
		return "matched"
	case ReconcileDuplicate:
		return "duplicate"
	default:
		return "not_applicable"
	}
}

// ReconcileResult is the outcome of one Observe call.
type ReconcileResult struct {
	Key       string    // the envelope idempotency_key — the cross-transport identity
	Transport Transport // which side this observation came from
	Status    ReconcileStatus

	// FirstTransport is the side that saw the event first; set only on
	// ReconcileMatched.
	FirstTransport Transport

	// MatchLatency is the wall-clock gap between the first sighting and the
	// confirming one; set only on ReconcileMatched. A large value means one
	// transport lagged the other badly even though the event was not lost.
	MatchLatency time.Duration
}

// Discrepancy is an event seen on exactly one transport that was never
// confirmed on the other within MatchDeadline — the data-integrity signal
// DATA-05 exists to surface (a correctness-sensitive consumer must know the
// hot path and the log of record diverged).
type Discrepancy struct {
	Key       string
	SeenOn    Transport // the only transport that delivered it
	MissingOn Transport // the transport that never confirmed it
	FirstSeen time.Time
	Age       time.Duration // now - FirstSeen at sweep time
}

// DefaultMatchDeadline is how long a one-sided sighting may stay unmatched
// before Sweep reports it as a discrepancy. Conservative placeholder: it
// must comfortably exceed the normal NATS-to-Kafka skew for the workload,
// so a healthy lag of the durable log behind the live spine never
// false-positives. Tune per deployment.
const DefaultMatchDeadline = 30 * time.Second

type pendingEntry struct {
	transport Transport
	firstSeen time.Time
}

// Reconciler matches events delivered over the NATS live spine against the
// same events on the Kafka log of record, keyed on idempotency_key — the
// transport-blind identity the dedup layer already stamps (EVT-17d: the
// same key rides Nats-Msg-Id on NATS and a user header on Kafka, and for
// FACTs equals event_id). An event confirmed on both is reconciled; one
// that ages out on a single side is a discrepancy.
//
// Identity choice: idempotency_key, not event_id, because it is the one
// identifier guaranteed identical across both publishes of the same logical
// event and is already the broker dedup key. Feed the reconciler the
// POST-dedup stream on each side (EVT-17d) so each key arrives ~once per
// transport; on a match the key is forgotten (report-once, like the gap
// detector advancing its mark), so a redelivery after the match would
// re-enter as Pending.
//
// Detection only — DATA-06 wires quality_flags, DATA-07 emits the
// DataQualityEvent. Concurrent-safe. The pending set lives behind a
// PendingStore: the default in-memory store is per-process (re-baselines on
// restart); a shared store (DEBT-02b) lets N reconciler replicas coordinate so
// a NATS sighting on one pod matches the Kafka sighting on another.
type Reconciler struct {
	matchDeadline time.Duration
	now           func() time.Time
	store         PendingStore
}

// PendingStore holds the cross-transport pending set. The default in-memory
// implementation is per-process; the **shared-state reconciler mode** (DEBT-02b)
// injects a distributed store (e.g. Redis, with ClaimOrMatch realized as an
// atomic Lua script — the bus stays decoupled from the client the same way
// RedisDedup does) so replicas that each see only one transport still reconcile.
//
// ClaimOrMatch MUST be atomic per key: the check-set-or-delete is a single
// read-modify-write, and a non-atomic distributed impl would let two replicas
// both record Pending and never match. SweepExpired/Count have no such
// constraint.
type PendingStore interface {
	// ClaimOrMatch atomically reconciles one sighting of key on transport at
	// now: absent ⇒ record + ReconcilePending; present same-transport ⇒
	// ReconcileDuplicate (firstSeen kept); present other-transport ⇒ delete +
	// ReconcileMatched with the first transport and its firstSeen. The latter
	// two return values are meaningful only for ReconcileMatched.
	ClaimOrMatch(key string, transport Transport, now time.Time) (ReconcileStatus, Transport, time.Time)
	// SweepExpired removes and returns every entry older than deadline as of now.
	SweepExpired(deadline time.Duration, now time.Time) []Discrepancy
	// Count is the number of pending entries (a gauge).
	Count() int
}

// NewReconciler returns a reconciler using the wall clock and a fresh in-memory
// store. A non-positive matchDeadline falls back to DefaultMatchDeadline.
func NewReconciler(matchDeadline time.Duration) *Reconciler {
	return NewReconcilerWithClock(matchDeadline, time.Now)
}

// NewReconcilerWithClock is NewReconciler with an injectable clock, so the
// match-deadline timing is testable from outside the package (the
// codebase's clock-injection convention — cf. PRED-07's
// NewSyncClientWithStub). A nil now falls back to time.Now.
func NewReconcilerWithClock(matchDeadline time.Duration, now func() time.Time) *Reconciler {
	return NewReconcilerWithStore(NewInMemoryPendingStore(), matchDeadline, now)
}

// NewReconcilerWithStore builds a reconciler over a caller-supplied PendingStore
// — the entry point for the shared-state mode (DEBT-02b): pass a distributed
// store and several reconciler replicas coordinate through it. A nil store
// falls back to a fresh in-memory one (parity with NewReconciler).
func NewReconcilerWithStore(store PendingStore, matchDeadline time.Duration, now func() time.Time) *Reconciler {
	if store == nil {
		store = NewInMemoryPendingStore()
	}
	if matchDeadline <= 0 {
		matchDeadline = DefaultMatchDeadline
	}
	if now == nil {
		now = time.Now
	}
	return &Reconciler{matchDeadline: matchDeadline, now: now, store: store}
}

// Observe records one event seen on the given transport and reports whether
// it matched a pending sighting on the other transport, is newly pending, or
// is a same-side duplicate. A nil envelope, empty idempotency_key, or
// unspecified transport is not-applicable and touches no state.
func (r *Reconciler) Observe(transport Transport, env *envelopepb.Envelope) ReconcileResult {
	if env == nil || env.IdempotencyKey == "" || transport == TransportUnspecified {
		return ReconcileResult{Transport: transport, Status: ReconcileNotApplicable}
	}
	now := r.now()
	res := ReconcileResult{Key: env.IdempotencyKey, Transport: transport}
	status, firstTransport, firstSeen := r.store.ClaimOrMatch(env.IdempotencyKey, transport, now)
	res.Status = status
	if status == ReconcileMatched {
		res.FirstTransport = firstTransport
		res.MatchLatency = now.Sub(firstSeen)
	}
	return res
}

// Sweep returns every pending event older than MatchDeadline as a
// Discrepancy and removes it from the pending set, so each is reported once
// (a later straggler on the missing side re-enters as Pending). A periodic
// job calls this; the returned slice is nil when nothing has aged out.
func (r *Reconciler) Sweep() []Discrepancy {
	return r.store.SweepExpired(r.matchDeadline, r.now())
}

// PendingCount is how many events are currently awaiting confirmation on the
// other transport — a gauge for observability and tests.
func (r *Reconciler) PendingCount() int {
	return r.store.Count()
}

// inMemoryPendingStore is the default per-process PendingStore (the original
// Reconciler state). Concurrent-safe via a mutex, which also makes ClaimOrMatch
// atomic within the process.
type inMemoryPendingStore struct {
	mu      sync.Mutex
	pending map[string]pendingEntry
}

// NewInMemoryPendingStore returns a fresh in-memory PendingStore. Share one
// instance across several Reconcilers to coordinate them within a process; for
// cross-process coordination inject a distributed store instead.
func NewInMemoryPendingStore() PendingStore {
	return &inMemoryPendingStore{pending: make(map[string]pendingEntry)}
}

func (s *inMemoryPendingStore) ClaimOrMatch(key string, transport Transport, now time.Time) (ReconcileStatus, Transport, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.pending[key]
	if !ok {
		s.pending[key] = pendingEntry{transport: transport, firstSeen: now}
		return ReconcilePending, TransportUnspecified, time.Time{}
	}
	if entry.transport == transport {
		// Redelivery on the same side; keep the original firstSeen so the
		// deadline still measures from first sight.
		return ReconcileDuplicate, TransportUnspecified, time.Time{}
	}
	delete(s.pending, key)
	return ReconcileMatched, entry.transport, entry.firstSeen
}

func (s *inMemoryPendingStore) SweepExpired(deadline time.Duration, now time.Time) []Discrepancy {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Discrepancy
	for key, entry := range s.pending {
		age := now.Sub(entry.firstSeen)
		if age > deadline {
			out = append(out, Discrepancy{
				Key:       key,
				SeenOn:    entry.transport,
				MissingOn: entry.transport.other(),
				FirstSeen: entry.firstSeen,
				Age:       age,
			})
			delete(s.pending, key)
		}
	}
	return out
}

func (s *inMemoryPendingStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}
