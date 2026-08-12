package store

import (
	"context"
	"sort"
	"sync"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// Memory is an in-memory Store for tests and local runs. It implements the full
// bitemporal point-in-time contract — it is the executable reference the
// Postgres impl mirrors, so the same correctness suite runs without a database
// (the Memory/Postgres parity pattern from the schema registry, EVT-16a). Loses
// everything on restart.
type Memory struct {
	mu sync.RWMutex
	// byInstrument keyed by instrument id → identity key → observation. The
	// identity key is the full (observation_time, kind, knowledge_time) tuple,
	// so a restatement (same observation_time + kind, newer knowledge_time) is a
	// distinct entry and an exact re-put overwrites in place (idempotent).
	byInstrument map[string]map[obsKey]Observation

	// bars is the OHLCV series (#425), keyed by its full bitemporal identity so
	// a restatement coexists with the original rather than replacing it — the
	// same contract the Postgres primary key enforces.
	bars map[barKey]Bar
}

type obsKey struct {
	obsUnixNano  int64
	kind         PriceKind
	knowUnixNano int64
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{byInstrument: make(map[string]map[obsKey]Observation)}
}

func (m *Memory) Put(_ context.Context, obs []Observation) error {
	for i := range obs {
		if err := obs[i].validate(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range obs {
		byKey, ok := m.byInstrument[o.InstrumentID]
		if !ok {
			byKey = make(map[obsKey]Observation)
			m.byInstrument[o.InstrumentID] = byKey
		}
		byKey[keyOf(o)] = cloneObs(o)
	}
	return nil
}

func (m *Memory) History(_ context.Context, q Query) ([]Observation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// latest holds, per (observation_time, kind), the visible observation with
	// the greatest knowledge_time <= AsOf — the restatement collapse.
	type otKey struct {
		obsUnixNano int64
		kind        PriceKind
	}
	latest := make(map[otKey]Observation)
	for _, o := range m.byInstrument[q.InstrumentID] {
		if !visible(o, q) {
			continue
		}
		k := otKey{obsUnixNano: o.ObservationTime.UnixNano(), kind: o.Kind}
		if cur, ok := latest[k]; !ok || o.KnowledgeTime.After(cur.KnowledgeTime) {
			latest[k] = o
		}
	}

	out := make([]Observation, 0, len(latest))
	for _, o := range latest {
		out = append(out, cloneObs(o))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ObservationTime.Equal(out[j].ObservationTime) {
			return out[i].ObservationTime.Before(out[j].ObservationTime)
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}

func (m *Memory) LatestAsOf(_ context.Context, instrumentID string, kind PriceKind, asOf time.Time) (Observation, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var best Observation
	found := false
	for _, o := range m.byInstrument[instrumentID] {
		if o.Kind != kind {
			continue
		}
		if !asOf.IsZero() && (o.ObservationTime.After(asOf) || o.KnowledgeTime.After(asOf)) {
			continue
		}
		if !found || moreRecent(o, best) {
			best = o
			found = true
		}
	}
	if !found {
		return Observation{}, false, nil
	}
	return cloneObs(best), true, nil
}

func (m *Memory) Ping(_ context.Context) error { return nil }

// visible reports whether o passes q's window + kind + knowledge-horizon filter.
func visible(o Observation, q Query) bool {
	if q.Kind != PriceKindUnspecified && o.Kind != q.Kind {
		return false
	}
	if !q.Start.IsZero() && o.ObservationTime.Before(q.Start) {
		return false
	}
	if !q.End.IsZero() && o.ObservationTime.After(q.End) {
		return false
	}
	if !q.AsOf.IsZero() && o.KnowledgeTime.After(q.AsOf) {
		return false
	}
	return true
}

// moreRecent reports whether a is a later spot mark than b: later
// observation_time wins; ties broken by later knowledge_time (the freshest
// restatement of the same date).
func moreRecent(a, b Observation) bool {
	if !a.ObservationTime.Equal(b.ObservationTime) {
		return a.ObservationTime.After(b.ObservationTime)
	}
	return a.KnowledgeTime.After(b.KnowledgeTime)
}

func keyOf(o Observation) obsKey {
	return obsKey{
		obsUnixNano:  o.ObservationTime.UnixNano(),
		kind:         o.Kind,
		knowUnixNano: o.KnowledgeTime.UnixNano(),
	}
}

// cloneObs deep-copies the Decimal so a caller mutating its input (or a stored
// value) cannot bleed into the other — the RISK-03 immutability discipline.
func cloneObs(o Observation) Observation {
	if o.Price != nil {
		o.Price = &commonpb.Decimal{Coefficient: o.Price.Coefficient, Exponent: o.Price.Exponent}
	}
	return o
}

// Compile-time assertion that Memory satisfies Store.
var _ Store = (*Memory)(nil)
