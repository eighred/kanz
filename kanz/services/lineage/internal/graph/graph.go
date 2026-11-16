// Package graph stores the bounded, tenant-owned lineage projection. It is
// incomplete after restart: an unretained identifier is UNKNOWN, not absent.
package graph

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

type Owner string

// DatasetID remains the canonical schema identity. Ownership is a separate key.
type DatasetID struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (d DatasetID) String() string { return d.Namespace + "." + d.Name }

// Dataset identity is the tuple, not its display string: a.b/c and a/b.c
// stringify equally but remain distinct graph nodes with a deterministic order.
func datasetLess(a, b DatasetID) bool {
	if a.Namespace == b.Namespace {
		return a.Name < b.Name
	}
	return a.Namespace < b.Namespace
}

// Info is a stored record. A zero/caller-constructed Info has no owner; only
// admission can populate owner. Governor must read Owner(), never the caller.
type Info struct {
	ID        DatasetID `json:"id"`
	Domain    string    `json:"domain"`
	SchemaRef string    `json:"schema_ref"`
	LastSeen  time.Time `json:"last_seen"`
	Events    uint64    `json:"events"`
	owner     Owner
}

func (i Info) Owner() Owner { return i.owner }

type Lookup string

const (
	LookupRetained    Lookup = "retained"
	LookupNotObserved Lookup = "never_observed"
	LookupUnknown     Lookup = "unknown"
)

type Coverage struct {
	Retained int       `json:"retained"`
	Capacity int       `json:"capacity"`
	Evicted  uint64    `json:"evicted"`
	Complete bool      `json:"complete"`
	Since    time.Time `json:"since"`
	// Horizon is the maximum evicted event time IN THIS OWNER'S scope. Unknown
	// observations at or below it refuse admission, including legitimate late
	// facts. They require owned replay/rebaseline rather than a guessed recount.
	Horizon        *time.Time `json:"horizon,omitempty"`
	UnlinkedCauses uint64     `json:"unlinked_causes"`
}

// Limits are GLOBAL, not multiplied for every owner. StringBytesMax charges
// all string allocations retained by Memory; fixed record/map/ring overhead is
// bounded separately by DatasetMax, EdgeMax and EventIndexMax. This is not RSS.
type Limits struct {
	EventIndexMax  int
	DatasetMax     int
	EdgeMax        int
	StringBytesMax uint64
}

func (l Limits) Validate() error {
	if l.EventIndexMax <= 0 || l.DatasetMax <= 0 || l.EdgeMax <= 0 || l.StringBytesMax == 0 {
		return errors.New("graph: every global limit must be positive")
	}
	return nil
}

const DefaultEventIndexCapacity = 250_000

var (
	ErrCapacity       = errors.New("graph: global projection capacity refused admission")
	ErrHistoryUnknown = errors.New("graph: observation is outside retained owner history")
	ErrConflict       = errors.New("graph: retained event identity has conflicting lineage")
	ErrOwnerUnknown   = errors.New("graph: observation owner is unknown")
)

type CapacityError struct{ Resource string }

func (e *CapacityError) Error() string {
	return "graph: global " + e.Resource + " capacity refused admission"
}
func (e *CapacityError) Unwrap() error { return ErrCapacity }

// Observation includes the complete lineage/emission projection, not payload
// contents. Harvester first runs canonical bus.Validate. No principal, context
// fallback or LINEAGE_TENANT can supply Owner.
type Observation struct {
	Owner         Owner
	EventID       string
	Dataset       DatasetID
	Domain        string
	SchemaRef     string
	At            time.Time
	ParentEventID string
	Source        string
	CorrelationID string
}

// ObservationSnapshot is immutable outside this package. The parent was resolved
// under the same lock as admission. Retries emit this frozen view, even if a
// missing parent arrives or another event replaces the node's latest metadata.
type ObservationSnapshot struct {
	observation Observation
	input       DatasetID
	hasInput    bool
}

func (s ObservationSnapshot) Owner() Owner             { return s.observation.Owner }
func (s ObservationSnapshot) Observation() Observation { return s.observation }
func (s ObservationSnapshot) Input() (DatasetID, bool) { return s.input, s.hasInput }

type Stats struct {
	Retained       int
	Capacity       int
	Evicted        uint64
	UnlinkedCauses uint64
	Datasets       int
	Edges          int
	StringBytes    uint64
}
type Graph interface {
	Observe(Observation) (ObservationSnapshot, error)
	DatasetOf(Owner, string) (DatasetID, Lookup)
	Get(Owner, DatasetID) (Info, bool)
	Upstream(Owner, DatasetID) []Info
	Datasets(Owner) []Info
	Coverage(Owner) Coverage
	Stats() Stats
}

type datasetKey struct {
	Owner Owner
	ID    DatasetID
}
type eventKey struct {
	Owner Owner
	ID    string
}
type datasetRecord struct {
	info        Info
	lastEventID string
	evicted     uint64
	horizon     time.Time
	unlinked    uint64
}
type indexEntry struct {
	observation Observation
	input       DatasetID
	hasInput    bool
	stringBytes uint64
}

func (e *indexEntry) snapshot() ObservationSnapshot {
	return ObservationSnapshot{observation: e.observation, input: e.input, hasInput: e.hasInput}
}

// Memory retains admitted topology; it never silently destroys it to make room.
// A full dataset/edge/string budget refuses the entire observation. There is no
// owner registry: scoped counters/floors derive from existing dataset records.
// Owner keys in all indexes refer to cloned, admitted dataset keys.
type Memory struct {
	mu            sync.RWMutex
	datasets      map[datasetKey]*datasetRecord
	upstream      map[datasetKey]map[datasetKey]struct{}
	eventDS       map[eventKey]*indexEntry
	ring          []*indexEntry
	next          int
	limits        Limits
	edgeCount     int
	stringBytes   uint64
	evicted       uint64
	unlinked      uint64
	since         time.Time
	completeOwner Owner
}
type Option func(*Memory) error

// WithEventIndexCapacity overrides the same global event limit in finite fixtures.
func WithEventIndexCapacity(n int) Option {
	return func(m *Memory) error {
		if n <= 0 {
			return errors.New("graph: event index capacity must be positive")
		}
		m.limits.EventIndexMax = n
		return nil
	}
}

// WithCompleteHistory is an explicit finite-history declaration for ONE owner.
// Only a caller that successfully loaded that complete scoped history may use
// it. The deployed durable/last-ack consumer must never pass this option.
func WithCompleteHistory(owner Owner) Option {
	return func(m *Memory) error {
		if owner == "" || m.completeOwner != "" {
			return errors.New("graph: exactly one nonempty complete-history scope required")
		}
		if uint64(len(owner)) > m.limits.StringBytesMax {
			return &CapacityError{Resource: "strings"}
		}
		m.completeOwner = Owner(strings.Clone(string(owner)))
		m.stringBytes = uint64(len(owner))
		return nil
	}
}
func NewMemory(limits Limits, opts ...Option) (*Memory, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	m := &Memory{
		datasets: map[datasetKey]*datasetRecord{},
		upstream: map[datasetKey]map[datasetKey]struct{}{},
		eventDS:  map[eventKey]*indexEntry{},
		limits:   limits, since: time.Now().UTC(),
	}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("graph: nil option")
		}
		if err := opt(m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func add(a, b uint64) (uint64, bool) {
	if ^uint64(0)-a < b {
		return 0, false
	}
	return a + b, true
}
func stringCost(values ...string) (uint64, bool) {
	var n uint64
	for _, value := range values {
		var ok bool
		n, ok = add(n, uint64(len(value)))
		if !ok {
			return 0, false
		}
	}
	return n, true
}
func metadataCost(domain, schema, lastID string) (uint64, bool) {
	return stringCost(domain, schema, lastID)
}
func observationCost(o Observation) (uint64, bool) {
	// Dataset/owner strings alias the permanent admitted dataset key. EventID
	// backs both the map key and ring entry; it is cloned exactly once. These six
	// independently cloned fields also own the frozen emission projection.
	return stringCost(o.EventID, o.ParentEventID, o.Domain, o.SchemaRef, o.Source, o.CorrelationID)
}
func cloneObservation(o Observation, key datasetKey) Observation {
	o.Owner = key.Owner
	o.Dataset = key.ID
	o.EventID = strings.Clone(o.EventID)
	o.ParentEventID = strings.Clone(o.ParentEventID)
	o.Domain = strings.Clone(o.Domain)
	o.SchemaRef = strings.Clone(o.SchemaRef)
	o.Source = strings.Clone(o.Source)
	o.CorrelationID = strings.Clone(o.CorrelationID)
	return o
}
func (m *Memory) floorLocked(owner Owner) (time.Time, bool) {
	var floor time.Time
	known := false
	for key, rec := range m.datasets {
		if key.Owner != owner || rec.evicted == 0 {
			continue
		}
		if !known || rec.horizon.After(floor) {
			floor = rec.horizon
			known = true
		}
	}
	return floor, known
}

// Observe preflights every refusal and overflow before ANY graph mutation. A
// capacity/history refusal is transient; an operator may rebaseline or change
// approved budgets and redrive the same bytes. Retained duplicates are stable.
// After eviction a reused identity with a changed later timestamp cannot be
// recognized as historical conflict: no all-history identity ledger is claimed.
func (m *Memory) Observe(o Observation) (ObservationSnapshot, error) {
	if o.Owner == "" {
		return ObservationSnapshot{}, ErrOwnerUnknown
	}
	if o.EventID == "" || o.Dataset.Namespace == "" || o.Dataset.Name == "" ||
		o.Domain == "" || o.SchemaRef == "" || o.Source == "" || o.CorrelationID == "" ||
		o.At.Year() < 1 || o.At.Year() > 9999 {
		return ObservationSnapshot{}, errors.New("graph: incomplete lineage observation")
	}
	o.At = o.At.UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	ek := eventKey{Owner: o.Owner, ID: o.EventID}
	if retained, exists := m.eventDS[ek]; exists {
		if retained.observation != o {
			return ObservationSnapshot{}, ErrConflict
		}
		return retained.snapshot(), nil
	}
	if floor, known := m.floorLocked(o.Owner); known && !o.At.After(floor) {
		return ObservationSnapshot{}, ErrHistoryUnknown
	}

	key := datasetKey{Owner: o.Owner, ID: o.Dataset}
	rec, exists := m.datasets[key]
	if !exists && len(m.datasets) >= m.limits.DatasetMax {
		return ObservationSnapshot{}, &CapacityError{Resource: "datasets"}
	}
	if exists && rec.info.Events == ^uint64(0) {
		return ObservationSnapshot{}, &CapacityError{Resource: "counter"}
	}
	var input DatasetID
	hasInput, unlinked := false, false
	if o.ParentEventID != "" && o.ParentEventID != o.EventID {
		if parent, ok := m.eventDS[eventKey{Owner: o.Owner, ID: o.ParentEventID}]; ok {
			input, hasInput = parent.observation.Dataset, true
		} else {
			unlinked = true
		}
	}
	newEdge := hasInput && input != o.Dataset
	if newEdge {
		_, already := m.upstream[key][datasetKey{Owner: o.Owner, ID: input}]
		newEdge = !already
		if newEdge && m.edgeCount >= m.limits.EdgeMax {
			return ObservationSnapshot{}, &CapacityError{Resource: "edges"}
		}
	}
	if unlinked && (m.unlinked == ^uint64(0) || exists && rec.unlinked == ^uint64(0)) {
		return ObservationSnapshot{}, &CapacityError{Resource: "counter"}
	}
	var victim *indexEntry
	var victimRec *datasetRecord
	if len(m.ring) == m.limits.EventIndexMax {
		victim = m.ring[m.next]
		victimRec = m.datasets[datasetKey{Owner: victim.observation.Owner, ID: victim.observation.Dataset}]
		if m.evicted == ^uint64(0) || victimRec.evicted == ^uint64(0) {
			return ObservationSnapshot{}, &CapacityError{Resource: "counter"}
		}
	}
	latest := !exists || o.At.After(rec.info.LastSeen) ||
		o.At.Equal(rec.info.LastSeen) && o.EventID > rec.lastEventID
	projected := m.stringBytes
	if victim != nil {
		projected -= victim.stringBytes
	}
	if exists && latest {
		old, ok := metadataCost(rec.info.Domain, rec.info.SchemaRef, rec.lastEventID)
		if !ok || old > projected {
			return ObservationSnapshot{}, &CapacityError{Resource: "strings"}
		}
		projected -= old
	}
	cost, ok := observationCost(o)
	if !ok {
		return ObservationSnapshot{}, &CapacityError{Resource: "strings"}
	}
	projected, ok = add(projected, cost)
	if !ok {
		return ObservationSnapshot{}, &CapacityError{Resource: "strings"}
	}
	if !exists {
		keyCost, good := stringCost(string(o.Owner), o.Dataset.Namespace, o.Dataset.Name)
		if !good {
			return ObservationSnapshot{}, &CapacityError{Resource: "strings"}
		}
		projected, ok = add(projected, keyCost)
		if !ok {
			return ObservationSnapshot{}, &CapacityError{Resource: "strings"}
		}
	}
	if latest {
		meta, good := metadataCost(o.Domain, o.SchemaRef, o.EventID)
		if !good {
			return ObservationSnapshot{}, &CapacityError{Resource: "strings"}
		}
		projected, ok = add(projected, meta)
		if !ok {
			return ObservationSnapshot{}, &CapacityError{Resource: "strings"}
		}
	}
	if projected > m.limits.StringBytesMax {
		return ObservationSnapshot{}, &CapacityError{Resource: "strings"}
	}

	// All fallible admission checks are complete. Prepare owned strings before
	// publishing any new record. No inbound substring backing buffer is retained.
	if exists {
		key = datasetKey{Owner: rec.info.owner, ID: rec.info.ID}
	} else {
		key = datasetKey{Owner: Owner(strings.Clone(string(o.Owner))),
			ID: DatasetID{Namespace: strings.Clone(o.Dataset.Namespace), Name: strings.Clone(o.Dataset.Name)}}
		rec = &datasetRecord{info: Info{ID: key.ID, owner: key.Owner}}
	}
	var domain, schema, lastID string
	if latest {
		domain, schema, lastID = strings.Clone(o.Domain), strings.Clone(o.SchemaRef), strings.Clone(o.EventID)
	}
	entry := &indexEntry{observation: cloneObservation(o, key), input: input, hasInput: hasInput, stringBytes: cost}

	if !exists {
		m.datasets[key] = rec
	}
	if latest {
		rec.info.Domain, rec.info.SchemaRef, rec.info.LastSeen, rec.lastEventID = domain, schema, o.At, lastID
	}
	rec.info.Events++
	if unlinked {
		rec.unlinked++
		m.unlinked++
	}
	if newEdge {
		if m.upstream[key] == nil {
			m.upstream[key] = map[datasetKey]struct{}{}
		}
		// input is the canonical, same-owner admitted parent key. No new edge
		// string allocation is retained; both keys already live in datasets.
		m.upstream[key][datasetKey{Owner: key.Owner, ID: input}] = struct{}{}
		m.edgeCount++
	}
	if victim != nil {
		delete(m.eventDS, eventKey{Owner: victim.observation.Owner, ID: victim.observation.EventID})
		m.evicted++
		if victimRec.evicted == 0 || victim.observation.At.After(victimRec.horizon) {
			victimRec.horizon = victim.observation.At
		}
		victimRec.evicted++
		m.ring[m.next] = entry
	} else {
		m.ring = append(m.ring, entry)
	}
	m.next = (m.next + 1) % m.limits.EventIndexMax
	m.eventDS[eventKey{Owner: key.Owner, ID: entry.observation.EventID}] = entry
	m.stringBytes = projected
	return entry.snapshot(), nil
}

func (m *Memory) coverageLocked(owner Owner) Coverage {
	c := Coverage{Capacity: m.limits.EventIndexMax, Since: m.since}
	if owner == "" {
		return c
	}
	for key, rec := range m.datasets {
		if key.Owner != owner {
			continue
		}
		c.Evicted += rec.evicted
		c.UnlinkedCauses += rec.unlinked
	}
	for key := range m.eventDS {
		if key.Owner == owner {
			c.Retained++
		}
	}
	if floor, known := m.floorLocked(owner); known {
		c.Horizon = &floor
	}
	c.Complete = m.completeOwner != "" && owner == m.completeOwner && c.Evicted == 0
	return c
}
func (m *Memory) missLocked(owner Owner) Lookup {
	if owner != "" && m.coverageLocked(owner).Complete {
		return LookupNotObserved
	}
	return LookupUnknown
}
func (m *Memory) DatasetOf(owner Owner, eventID string) (DatasetID, Lookup) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if owner != "" {
		if e, ok := m.eventDS[eventKey{Owner: owner, ID: eventID}]; ok {
			return e.observation.Dataset, LookupRetained
		}
	}
	return DatasetID{}, m.missLocked(owner)
}
func (m *Memory) Get(owner Owner, ds DatasetID) (Info, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if owner == "" {
		return Info{}, false
	}
	rec, ok := m.datasets[datasetKey{Owner: owner, ID: ds}]
	if !ok {
		return Info{}, false
	}
	return rec.info, true
}
func (m *Memory) Coverage(owner Owner) Coverage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.coverageLocked(owner)
}
func (m *Memory) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Stats{Retained: len(m.eventDS), Capacity: m.limits.EventIndexMax,
		Evicted: m.evicted, UnlinkedCauses: m.unlinked, Datasets: len(m.datasets),
		Edges: m.edgeCount, StringBytes: m.stringBytes}
}
func (m *Memory) Datasets(owner Owner) []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Info
	if owner == "" {
		return out
	}
	for key, rec := range m.datasets {
		if key.Owner == owner {
			out = append(out, rec.info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return datasetLess(out[i].ID, out[j].ID) })
	return out
}
func (m *Memory) Upstream(owner Owner, ds DatasetID) []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if owner == "" {
		return nil
	}
	start := datasetKey{Owner: owner, ID: ds}
	if _, ok := m.datasets[start]; !ok {
		return nil
	}
	seen := map[datasetKey]bool{start: true}
	queue := []datasetKey{start}
	var out []Info
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		for parent := range m.upstream[key] {
			if parent.Owner != owner || seen[parent] {
				continue
			}
			seen[parent] = true
			if rec, ok := m.datasets[parent]; ok {
				out = append(out, rec.info)
				queue = append(queue, parent)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return datasetLess(out[i].ID, out[j].ID) })
	return out
}

var _ Graph = (*Memory)(nil)
var _ error = (*CapacityError)(nil)
