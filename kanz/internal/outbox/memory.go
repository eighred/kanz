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
//
// # WHERE THE TWO QUEUES DIVERGE, AND WHY THAT IS THE CORRECT DIRECTION (#890)
//
// Postgres KEEPS a published row and stamps published_at on it. Memory DROPS it.
// The row is the same row in both, and the difference is what a published row is
// FOR: in the table it is a durable artifact an operator can query, archive and
// prune, and #817's last_error assertion reads the column of one. In a slice
// behind a mutex it is reachable by nothing — no query surface, no archival, no
// retention policy — so keeping it buys the estate no answer it could not get
// otherwise and costs one *memRow, plus its marshalled payload, per business
// event this process has ever emitted, for the life of the process.
//
// This queue used to keep them: rows was append-only, MarkPublished flipped a
// bool and every read FILTERED on it. Growth was therefore one entry per order
// accepted, per fill folded, per ledger posting — monotonic in trading volume on
// the OMS's own heap, reported by nothing until the pod is OOM-killed.
type Memory struct {
	mu   sync.Mutex
	next int64
	// rows holds exactly the UNPUBLISHED records — the in-memory spelling of
	// Postgres's `WHERE published_at IS NULL`, which is the predicate on every
	// read this queue has. Its length is therefore the pending depth and not the
	// process's lifetime volume: it rises with a backlog and falls as the relay
	// drains it.
	//
	// The BACKING ARRAY does not shrink, so the retained capacity settles at the
	// high-water mark of pending depth. That is bounded by how far behind the
	// relay has ever fallen, which is what the age gauge already alerts on —
	// unlike the old behaviour, it is not bounded by anything the process did in
	// the past and cannot undo.
	rows    []*memRow
	locked  map[string]bool
	nowFunc func() time.Time
}

type memRow struct {
	id       int64
	attempts int
	rec      Record
	enqueued time.Time
	// lastErr mirrors the outbox.last_error column, bounded the same way. It is
	// not bookkeeping this queue needs: it is here because Memory is not a
	// permissive double, and a queue that accepted a failure cause and dropped
	// it would let a test certify a stall whose reason production can report and
	// this cannot (#817).
	lastErr string
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
		if r.rec.PartitionKey != key {
			continue
		}
		out = append(out, Pending{ID: r.id, Attempts: r.attempts, Record: r.rec, LastError: r.lastErr})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// MarkPublished DROPS the record, and dropping is this queue's evictor (#890).
//
// # WHY THIS CANNOT LOSE A FACT
//
// It is reached from exactly one place: Relay.drainKey, on the line after
// Publish returned nil. The broker HAS the event before this runs, which is the
// same instant Postgres.MarkPublished stamps published_at — after which
// `WHERE published_at IS NULL` excludes the row from every read that table has,
// permanently. Removal is that exclusion, made total. It is downstream of the
// publish, never ahead of it, so the at-least-once direction the whole package
// rests on is untouched: a publish that FAILS goes to MarkFailed, which keeps
// the row at the head of its key exactly as before.
//
// The relay does not hold the row. Pending is a VALUE copy taken before the
// publish, so nothing it is still reading dies here.
//
// # WHY AN ABSENT ROW IS SILENCE RATHER THAN AN ERROR, IN BOTH QUEUES
//
// Postgres marks with `AND published_at IS NULL` and treats RowsAffected()==0 as
// success — "another relay published it between our read and this update, or RLS
// hid it; the record is out". It cannot distinguish that from an id that never
// existed, and does not need to. A dropped row lands in the same place here, and
// m.next is never rewound, so an id is never reused and a late mark for a
// drained record can never reach a different one.
func (m *Memory) MarkPublished(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i := m.indexOf(id); i >= 0 {
		// The vacated tail slot is cleared before the reslice: leaving the
		// pointer above len would keep one drained record's marshalled payload
		// reachable until an Append happens to overwrite that slot, which on a
		// queue that has just gone quiet is never.
		copy(m.rows[i:], m.rows[i+1:])
		m.rows[len(m.rows)-1] = nil
		m.rows = m.rows[:len(m.rows)-1]
	}
	return nil
}

// MarkFailed records an attempt on a record still in the queue. A record that is
// no longer here has published — see MarkPublished — and Postgres's own
// `AND published_at IS NULL` predicate makes the same call a no-op there.
func (m *Memory) MarkFailed(_ context.Context, id int64, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i := m.indexOf(id); i >= 0 {
		m.rows[i].attempts++
		m.rows[i].lastErr = boundedCause(cause)
	}
	return nil
}

func (m *Memory) OldestPendingAge(_ context.Context, now time.Time) (time.Duration, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldest time.Time
	for _, r := range m.rows {
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
	return len(m.rows)
}

// indexOf is the ONE place a record is located by id, so the two Mark* calls
// cannot drift about what "the record is no longer here" means.
func (m *Memory) indexOf(id int64) int {
	for i, r := range m.rows {
		if r.id == id {
			return i
		}
	}
	return -1
}
