// Package graph is the in-memory lineage graph: dataset nodes and the
// derived-from edges harvested from envelope causation. It answers the LIN-01d
// "where did this number come from" query — the dataset-level provenance that
// complements the AUDIT-01c event-level causal chain (that walks event_ids; this
// walks the datasets those events belong to). The Graph interface is the seam a
// durable backend (Neo4j/DataHub) slots behind; Memory is the default + test
// double, same two-impl stance as the audit store.
package graph

import (
	"sort"
	"sync"
	"time"
)

// DatasetID is a dataset's stable identity (OpenLineage namespace + name).
type DatasetID struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (d DatasetID) String() string { return d.Namespace + "." + d.Name }

// Info is a dataset node's catalog metadata (LIN-01b surfaces this).
type Info struct {
	ID        DatasetID `json:"id"`
	Domain    string    `json:"domain"`
	SchemaRef string    `json:"schema_ref"`
	// LastSeen is the latest envelope event_time observed for the dataset.
	LastSeen time.Time `json:"last_seen"`
	// Events counts envelopes harvested into the dataset.
	Events uint64 `json:"events"`
}

// Lookup is the outcome of resolving an identifier against the graph. It has
// THREE states because two is what made this service lie: the event index is
// bounded and the process starts empty, so a miss is usually a statement about
// the index, not about the estate.
//
// A bool cannot hold that difference, and a provenance service in which "I have
// no record" and "there is no record" read the same has failed at the only thing
// it does (AGENTS.md: "'nothing configured' and 'checked, and fine' must never
// look the same"). String-valued so it crosses the API as itself, the same shape
// as governance.Sensitivity in this service.
type Lookup string

const (
	// LookupRetained: found, and the answer is authoritative.
	LookupRetained Lookup = "retained"
	// LookupNotObserved: a miss the index CAN interpret — it has observed all of
	// history and evicted nothing, so this identifier was genuinely never seen.
	// Only a graph declared WithCompleteHistory can produce this.
	LookupNotObserved Lookup = "never_observed"
	// LookupUnknown: a miss the index CANNOT interpret. The identifier may have
	// been observed and evicted, or observed before this process started, or never
	// observed at all — the index cannot tell, and must not pretend to. Callers
	// must NOT read this as "no such event"; it is "no answer".
	LookupUnknown Lookup = "unknown"
)

// Coverage describes what the graph can and cannot answer for, so a bounded
// index does not silently become a lying one. It travels on every provenance
// answer — including the successful ones, because an eviction can also have cost
// the graph an EDGE (see UnlinkedCauses), which truncates an answer that
// otherwise looks complete.
type Coverage struct {
	// Retained is the number of event ids currently indexed; Capacity the hard
	// ceiling. Retained == Capacity means eviction is live.
	Retained int `json:"retained"`
	Capacity int `json:"capacity"`
	// Evicted counts event ids dropped to stay under Capacity, for the lifetime of
	// this process. Nonzero ⇒ no miss on this index is conclusive.
	Evicted uint64 `json:"evicted"`
	// Complete reports whether a miss is conclusive. It is true only for a graph
	// declared WithCompleteHistory that has evicted nothing. The deployed lineage
	// harvester is a durable consumer resuming at last ack, so it is false there —
	// and that is the honest value, not a defect.
	Complete bool `json:"complete"`
	// Since is when this index began observing. Everything before it is outside
	// coverage: the durable consumer resumes at last ack rather than replaying, so
	// a restart does not rebuild the index.
	Since time.Time `json:"since"`
	// Horizon is the event_time of the most recently evicted entry — approximately
	// the age below which the index no longer answers. Approximate because
	// eviction is insertion-ordered and envelopes can arrive out of event_time
	// order.
	//
	// A POINTER, and nil until the first eviction. `omitempty` does not suppress a
	// zero time.Time — a struct is never "empty" to encoding/json — so a plain
	// field would put "0001-01-01T00:00:00Z" on the wire and "nothing has been
	// dropped" would read as "dropped something dated the year 1". Absent means
	// absent.
	Horizon *time.Time `json:"horizon,omitempty"`
	// UnlinkedCauses counts events whose causation id was already evicted when they
	// arrived, so the derived-from edge could NOT be drawn. Nonzero means the
	// dataset topology itself is missing edges — the capacity is too small for the
	// estate's causation latency, and upstream answers may be short.
	UnlinkedCauses uint64 `json:"unlinked_causes"`
}

// Graph records dataset observations and resolves provenance. Implementations
// must be safe for concurrent use (the harvester writes while the API reads).
type Graph interface {
	// Observe records that event eventID produced dataset ds (schemaRef/domain
	// metadata), caused by parentEventID (empty ⇒ root). It links ds's upstream
	// to the dataset the parent event produced, when that parent has been seen.
	Observe(eventID string, ds DatasetID, domain, schemaRef string, eventTime time.Time, parentEventID string)
	// DatasetOf returns the dataset an event produced. Read the Lookup, not the
	// zero DatasetID: only LookupRetained is an answer.
	DatasetOf(eventID string) (DatasetID, Lookup)
	// Upstream returns every ancestor dataset of ds (transitive derived-from),
	// deterministically ordered. The walk is cycle-safe.
	Upstream(ds DatasetID) []DatasetID
	// Datasets lists every known dataset, ordered by id — the catalog view.
	Datasets() []Info
	// Get returns one dataset's info.
	Get(ds DatasetID) (Info, bool)
	// Coverage reports what this graph can answer for. Callers rendering an API
	// response MUST surface it: it is what tells a caller whether a miss meant
	// anything.
	Coverage() Coverage
}

// DefaultEventIndexCapacity bounds the event_id → dataset index.
//
// WHY A HARD ENTRY CAP AND NOT A TTL (#244). A TTL bounds retention by
// rate × window, which is not a bound at all on a ">" subscription: a burst, a
// backfill, or a JetStream redelivery storm multiplies the rate and the map
// grows with it — the same OOMKill, reached faster. A cap bounds bytes
// regardless of rate, which is the property the pod's memory limit needs.
//
// WHY INSERTION ORDER AND NOT ACCESS ORDER. An entry here is immutable — an
// event always produced the same dataset — so re-reading one teaches the index
// nothing, and promoting on read would buy only the ability to pin an
// in-flight investigation, at the cost of taking the write lock on every read
// while the harvester holds it hundreds of times a second. Evicting the
// oldest-OBSERVED entry makes the retained set exactly "the most recent N
// events", which is the axis provenance queries are skewed on.
//
// THE ASSUMPTION, STATED. Provenance is asked about recent events far more
// often than old ones — but "far more" is not "only", and a regulator asking
// about a trade from six months ago is a real query this index will NOT answer.
// That is not swept under the answer: it comes back as LookupUnknown, never as
// "no record". Answering it properly is the durable backend the package doc
// names as a seam and nothing yet binds (#244 follow-up).
//
// THE ARITHMETIC. An entry costs roughly 200 bytes (a ~36-byte event id, the
// map bucket, and a ring slot holding the id and its event_time), so 250k is
// ~50MB at the ceiling. At a sustained 300 events/sec that is a ~14-minute
// window; at a more typical 20/sec, ~3.5 hours. Raise it with
// LINEAGE_EVENT_INDEX_MAX when the pod's memory limit affords a longer window.
const DefaultEventIndexCapacity = 250_000

// indexEntry is one slot of the eviction ring. It carries the event_time only to
// report the retention Horizon; nothing reads it for provenance.
type indexEntry struct {
	eventID string
	at      time.Time
}

// Memory is the default in-memory Graph.
//
// datasets and upstream are NOT evicted: they are keyed by dataset, whose
// cardinality is the estate's schema count (dozens), not its event count. Only
// eventDS grows with traffic, so only eventDS is bounded. Losing an eventDS
// entry costs the ability to ENTER the graph by event id; the dataset topology
// it points into survives.
type Memory struct {
	mu       sync.RWMutex
	datasets map[DatasetID]*Info
	upstream map[DatasetID]map[DatasetID]struct{} // ds -> set of direct upstream ds
	eventDS  map[string]DatasetID                 // event_id -> dataset it produced

	// ring is the insertion-ordered eviction order for eventDS, grown lazily to
	// capacity then written circularly. next is the slot to write, which once the
	// ring is full is also the oldest entry. len(ring) == len(eventDS) always.
	capacity int
	ring     []indexEntry
	next     int

	evicted  uint64
	horizon  time.Time
	unlinked uint64
	complete bool
	since    time.Time
}

// Option configures a Memory.
type Option func(*Memory)

// WithEventIndexCapacity sets the hard ceiling on indexed event ids. Must be
// positive: there is no "unbounded" setting, because an unbounded index reached
// through config is the same OOMKill reached through a different file (#244).
func WithEventIndexCapacity(n int) Option { return func(m *Memory) { m.capacity = n } }

// WithCompleteHistory declares that this graph has observed EVERY event that
// ever existed — only then can a miss mean "never observed" rather than "I
// cannot say" (see Lookup).
//
// DO NOT SET THIS FOR A CONSUMER THAT RESUMES AT LAST ACK. The deployed lineage
// harvester is exactly that: a restart reads forward from the last acked
// sequence and never re-reads the history the previous process held in memory,
// so its index covers "since this pod started", not "since the estate started".
// The honest wiring omits this option, and the API then reports every miss as
// unknown. It is earned by a graph rebuilt from a replay of the stream's first
// sequence, or by the durable backend behind the Graph seam.
func WithCompleteHistory() Option { return func(m *Memory) { m.complete = true } }

func NewMemory(opts ...Option) *Memory {
	m := &Memory{
		datasets: map[DatasetID]*Info{},
		upstream: map[DatasetID]map[DatasetID]struct{}{},
		eventDS:  map[string]DatasetID{},
		capacity: DefaultEventIndexCapacity,
		since:    time.Now().UTC(),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.capacity <= 0 {
		// Loud at construction, in the composition root, rather than a graph that
		// evicts every entry it writes or one that never evicts at all.
		panic("graph: event index capacity must be positive")
	}
	return m
}

var _ Graph = (*Memory)(nil)

func (m *Memory) Observe(eventID string, ds DatasetID, domain, schemaRef string, eventTime time.Time, parentEventID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	info, ok := m.datasets[ds]
	if !ok {
		info = &Info{ID: ds, Domain: domain, SchemaRef: schemaRef}
		m.datasets[ds] = info
	}
	if schemaRef != "" {
		info.SchemaRef = schemaRef
	}
	if eventTime.After(info.LastSeen) {
		info.LastSeen = eventTime
	}
	info.Events++
	if eventID != "" {
		m.index(eventID, info.ID, eventTime)
	}

	// Link upstream when the parent's dataset is known and distinct (a dataset
	// derived from itself — a self-loop, e.g. a snapshot of the same entity — is
	// not provenance, so skip it).
	if parentEventID != "" {
		parentDS, ok := m.eventDS[parentEventID]
		switch {
		case !ok:
			// The cause was not indexed when its effect arrived — evicted, or never
			// seen because it predates this pod. Either way the edge can never be
			// drawn and the dataset topology is permanently short one derived-from.
			// Counted rather than shrugged off: it travels on every answer
			// (Coverage.UnlinkedCauses) because it makes an upstream list that looks
			// complete incomplete. A burst right after startup is the cold-start
			// cause and settles; a rate that persists once Retained sits at Capacity
			// is the capacity being below the estate's causation latency.
			m.unlinked++
		case parentDS != ds:
			if m.upstream[ds] == nil {
				m.upstream[ds] = map[DatasetID]struct{}{}
			}
			m.upstream[ds][parentDS] = struct{}{}
		}
	}
}

// index writes eventID into the bounded event index, evicting the oldest entry
// when the ring is full. Callers hold the write lock.
//
// ds is the CANONICAL DatasetID off the *Info, not the caller's copy: harvest
// builds a fresh "kanz."+domain string per event, and storing that per entry
// would hold one duplicate namespace and name allocation for every retained
// event. Reusing the first-seen strings interns them across the whole index.
func (m *Memory) index(eventID string, ds DatasetID, eventTime time.Time) {
	if _, exists := m.eventDS[eventID]; !exists {
		if len(m.ring) == m.capacity {
			old := m.ring[m.next]
			delete(m.eventDS, old.eventID)
			m.evicted++
			// Max, not last: eviction is insertion-ordered, so out-of-order arrivals
			// would otherwise walk the horizon backwards and overstate what is still
			// covered. Claiming the later boundary is the honest direction.
			if old.at.After(m.horizon) {
				m.horizon = old.at
			}
			m.ring[m.next] = indexEntry{eventID: eventID, at: eventTime}
		} else {
			m.ring = append(m.ring, indexEntry{eventID: eventID, at: eventTime})
		}
		m.next = (m.next + 1) % m.capacity
	}
	m.eventDS[eventID] = ds
}

func (m *Memory) DatasetOf(eventID string) (DatasetID, Lookup) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ds, ok := m.eventDS[eventID]; ok {
		return ds, LookupRetained
	}
	return DatasetID{}, m.missLocked()
}

// missLocked classifies a miss. Callers hold at least the read lock.
func (m *Memory) missLocked() Lookup {
	if m.complete && m.evicted == 0 {
		return LookupNotObserved
	}
	return LookupUnknown
}

// Coverage reports what this graph can answer for.
func (m *Memory) Coverage() Coverage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := Coverage{
		Retained:       len(m.eventDS),
		Capacity:       m.capacity,
		Evicted:        m.evicted,
		Complete:       m.complete && m.evicted == 0,
		Since:          m.since,
		UnlinkedCauses: m.unlinked,
	}
	if !m.horizon.IsZero() {
		h := m.horizon
		c.Horizon = &h
	}
	return c
}

func (m *Memory) Upstream(ds DatasetID) []DatasetID {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := map[DatasetID]struct{}{ds: {}}
	var out []DatasetID
	queue := []DatasetID{ds}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for up := range m.upstream[cur] {
			if _, ok := seen[up]; ok {
				continue
			}
			seen[up] = struct{}{}
			out = append(out, up)
			queue = append(queue, up)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (m *Memory) Datasets() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Info, 0, len(m.datasets))
	for _, info := range m.datasets {
		out = append(out, *info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID.Namespace != out[j].ID.Namespace {
			return out[i].ID.Namespace < out[j].ID.Namespace
		}
		return out[i].ID.Name < out[j].ID.Name
	})
	return out
}

func (m *Memory) Get(ds DatasetID) (Info, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	info, ok := m.datasets[ds]
	if !ok {
		return Info{}, false
	}
	return *info, true
}
