package outbox

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Memory is the in-process Queue.
//
// IT IS NOT A CONVENIENCE DOUBLE. It honours the same ordering contract, the
// same exclusive per-key drain and the same at-least-once semantics as Postgres,
// for the same reason order.MemoryStore honours the CAS contract: every
// service-level test in the OMS runs against it, so a seam that reordered where
// Postgres does not — or that let two drainers into one key — would certify
// behaviour production does not have. fakeBus already taught this repository
// what a permissive test double costs.
//
// WHAT IT CANNOT BE is durable. It is the queue the OMS gets when
// OMS_DATABASE_URL is unset, which is already the deployment that must run
// exactly one replica because its order store is a map (cmd/oms/main.go
// openStores). Its outbox degrades with it: a crash loses the records, exactly
// as it loses the orders they announce. That is coherent — the two halves fail
// together instead of disagreeing — and cmd/oms/main.go says it out loud.
type Memory struct {
	mu      sync.Mutex
	next    int64
	rows    []*memRow
	locked  map[string]bool
	nowFunc func() time.Time
}

type memRow struct {
	id        int64
	attempts  int
	rec       Record
	enqueued  time.Time
	published bool
}

// NewMemory returns an empty in-process Queue.
func NewMemory() *Memory {
	return &Memory{locked: make(map[string]bool), nowFunc: time.Now}
}

var _ Queue = (*Memory)(nil)

// Append enqueues records. It is the in-memory analogue of Enqueue, and the
// caller is expected to hold whatever lock makes it atomic with its state change
// — order.MemoryStore calls it under the same mutex hold as the map write,
// which is that store's whole equivalent of a transaction.
func (m *Memory) Append(records ...Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range records {
		if r.TenantID == "" {
			return errors.New("outbox: record with no tenant")
		}
		m.next++
		m.rows = append(m.rows, &memRow{id: m.next, rec: r, enqueued: m.nowFunc().UTC()})
	}
	return nil
}

func (m *Memory) PendingKeys(_ context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	first := map[string]int64{}
	for _, r := range m.rows {
		if r.published {
			continue
		}
		if cur, ok := first[r.rec.PartitionKey]; !ok || r.id < cur {
			first[r.rec.PartitionKey] = r.id
		}
	}
	keys := make([]string, 0, len(first))
	for k := range first {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return first[keys[i]] < first[keys[j]] })
	if len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

// LockKey honours both modes, like Postgres, so a test can drive two relays over
// one queue and see the background one skip while the inline one waits.
//
// The wait is a POLL rather than a condition variable, deliberately: the hold is
// a publish, the contention window is microseconds in-process, and a poll cannot
// deadlock a test that forgot to release. It respects ctx so a cancelled test
// does not hang.
func (m *Memory) LockKey(ctx context.Context, key string, wait bool) (func(), bool, error) {
	for {
		m.mu.Lock()
		if !m.locked[key] {
			m.locked[key] = true
			m.mu.Unlock()
			return func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				delete(m.locked, key)
			}, true, nil
		}
		m.mu.Unlock()
		if !wait {
			return nil, false, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (m *Memory) Pending(_ context.Context, key string, limit int) ([]Pending, error) {
	if limit <= 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Pending
	for _, r := range m.rows {
		if r.published || r.rec.PartitionKey != key {
			continue
		}
		out = append(out, Pending{ID: r.id, Attempts: r.attempts, Record: r.rec})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (m *Memory) MarkPublished(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.row(id); r != nil {
		r.published = true
	}
	return nil
}

func (m *Memory) MarkFailed(_ context.Context, id int64, _ error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.row(id); r != nil && !r.published {
		r.attempts++
	}
	return nil
}

func (m *Memory) OldestPendingAge(_ context.Context, now time.Time) (time.Duration, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldest time.Time
	for _, r := range m.rows {
		if r.published {
			continue
		}
		if oldest.IsZero() || r.enqueued.Before(oldest) {
			oldest = r.enqueued
		}
	}
	if oldest.IsZero() {
		return 0, false, nil
	}
	age := now.UTC().Sub(oldest)
	if age < 0 {
		age = 0
	}
	return age, true, nil
}

// PendingCount reports how many records are still unpublished. Tests assert on
// it; nothing in production reads it (the age gauge is the operational signal,
// because a depth of 1 that never drains is worse than a depth of 1000 that
// does).
func (m *Memory) PendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if !r.published {
			n++
		}
	}
	return n
}

func (m *Memory) row(id int64) *memRow {
	for _, r := range m.rows {
		if r.id == id {
			return r
		}
	}
	return nil
}
