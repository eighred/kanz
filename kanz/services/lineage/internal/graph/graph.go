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

// Graph records dataset observations and resolves provenance. Implementations
// must be safe for concurrent use (the harvester writes while the API reads).
type Graph interface {
	// Observe records that event eventID produced dataset ds (schemaRef/domain
	// metadata), caused by parentEventID (empty ⇒ root). It links ds's upstream
	// to the dataset the parent event produced, when that parent has been seen.
	Observe(eventID string, ds DatasetID, domain, schemaRef string, eventTime time.Time, parentEventID string)
	// DatasetOf returns the dataset an event produced, if harvested.
	DatasetOf(eventID string) (DatasetID, bool)
	// Upstream returns every ancestor dataset of ds (transitive derived-from),
	// deterministically ordered. The walk is cycle-safe.
	Upstream(ds DatasetID) []DatasetID
	// Datasets lists every known dataset, ordered by id — the catalog view.
	Datasets() []Info
	// Get returns one dataset's info.
	Get(ds DatasetID) (Info, bool)
}

// Memory is the default in-memory Graph.
type Memory struct {
	mu       sync.RWMutex
	datasets map[DatasetID]*Info
	upstream map[DatasetID]map[DatasetID]struct{} // ds -> set of direct upstream ds
	eventDS  map[string]DatasetID                 // event_id -> dataset it produced
}

func NewMemory() *Memory {
	return &Memory{
		datasets: map[DatasetID]*Info{},
		upstream: map[DatasetID]map[DatasetID]struct{}{},
		eventDS:  map[string]DatasetID{},
	}
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
		m.eventDS[eventID] = ds
	}

	// Link upstream when the parent's dataset is known and distinct (a dataset
	// derived from itself — a self-loop, e.g. a snapshot of the same entity — is
	// not provenance, so skip it).
	if parentEventID != "" {
		if parentDS, ok := m.eventDS[parentEventID]; ok && parentDS != ds {
			if m.upstream[ds] == nil {
				m.upstream[ds] = map[DatasetID]struct{}{}
			}
			m.upstream[ds][parentDS] = struct{}{}
		}
	}
}

func (m *Memory) DatasetOf(eventID string) (DatasetID, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ds, ok := m.eventDS[eventID]
	return ds, ok
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
