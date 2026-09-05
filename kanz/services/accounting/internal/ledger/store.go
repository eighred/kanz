package ledger

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/outbox"
)

// Store is the durable home of the append-only journal and the periodic book
// snapshot (PERS-01). The in-memory default below preserves exact semantics for
// tests and a single replica; a durable, replayable backend (Postgres, the
// risk-engine persist.StateStore stance) plugs in behind this interface with no
// change to the book or the fold. Append is idempotent on EntryID — the journal
// is exactly-once over an at-least-once producer.
type Store interface {
	// Append records one journal entry (idempotent on EntryID), and enqueues
	// whatever announce returns IN THE SAME TRANSACTION (#804).
	//
	// THE announce PARAMETER IS THE SAME DECISION order.Store.Create'S IS (#292).
	// The cash announcement was a second, independent write whose only recovery
	// was the next fold for the same portfolio, so one broker blip refused every
	// order for that portfolio under a buying-power mandate until unrelated
	// activity happened to arrive. It is a parameter rather than something the
	// store reaches for, so the ledger goes on knowing nothing about subjects or
	// FACTs.
	//
	// Nil announces nothing.
	Append(ctx context.Context, e *Event, announce Announcer) error
	// Journal returns a portfolio's ENTIRE journal (the caller orders for the
	// fold). It is unbounded by construction and grows for the life of the
	// portfolio: only the snapshotter, which must fold everything to build a
	// checkpoint, and the no-checkpoint fallback in MaterializeCurrent may call
	// it. A request handler that reaches for it has reintroduced #229.
	Journal(ctx context.Context, portfolioID string) ([]*Event, error)
	// JournalSince returns the entries whose knowledge_time is strictly after
	// the given watermark — the snapshot TAIL, and the read every current-book
	// materialization takes. Note this is the mirror image of the bitemporal
	// point-in-time read (Postgres.JournalAsOf, entries at or BEFORE a bound):
	// the tail is what a checkpoint has not yet absorbed.
	JournalSince(ctx context.Context, portfolioID string, after time.Time) ([]*Event, error)
	// SaveSnapshot upserts a portfolio's latest book snapshot.
	SaveSnapshot(ctx context.Context, snap *Snapshot) error
	// LoadSnapshot returns a portfolio's latest snapshot, or ErrNoSnapshot.
	LoadSnapshot(ctx context.Context, portfolioID string) (*Snapshot, error)
	// StalePortfolios returns up to limit portfolio ids whose journal holds
	// entries past their snapshot watermark — including those with no snapshot
	// at all. It is the Snapshotter's work queue.
	StalePortfolios(ctx context.Context, limit int) ([]string, error)
}

// Announcer renders the FACTs that must become true at the same instant the
// entry does. It is called INSIDE Append's transaction, over a Store whose reads
// SEE THE UNCOMMITTED ENTRY — which is what lets it announce the level the fold
// produced rather than the one that preceded it.
//
// It must do nothing but read and build: no publish, no second Append, no I/O of
// its own. Everything it returns is enqueued before the commit, so an entry
// whose announcement cannot be built is an entry that does not land — the
// correct direction, and the same stance outbox.From takes on a record with no
// tenant.
type Announcer func(ctx context.Context, st Store) ([]outbox.Record, error)

// ErrNoSnapshot is returned by LoadSnapshot when a portfolio has no snapshot yet.
var ErrNoSnapshot = errors.New("ledger: no snapshot")

// Snapshot is a point-in-time materialization of a book plus the knowledge
// watermark it folds entries through — the PERS-01 checkpoint that bounds replay
// to the journal tail. A bitemporal as-of read (ReplayAsOf) bypasses the snapshot
// and folds the journal directly; the snapshot only accelerates the
// current-knowledge book.
type Snapshot struct {
	PortfolioID string
	Positions   map[string]*Position
	Cash        map[string]*big.Rat
	Accrued     map[string]*big.Rat
	// Through is the knowledge watermark: every journal entry with Knowledge at or
	// before Through is already folded into this snapshot.
	Through time.Time
	// MaxEffective is the latest effective_time folded into this snapshot — the
	// fence that makes resuming from it equal to a full replay.
	//
	// The fold is ORDER-SENSITIVE. Weighted-average cost realizes P&L in
	// sequence, and the canonical order is (effective, knowledge, entry_id).
	// A checkpoint is ordered by KNOWLEDGE, so a tail entry backdated before
	// this fence — a late-reported fill, a restated corporate action — would be
	// folded last when a full replay would have folded it in the middle. The
	// resulting positions, and therefore the NAV, differ.
	//
	// MaterializeCurrent compares this against the tail and REFUSES the
	// checkpoint when the tail is backdated, paying for a full replay instead.
	// A zero value means "unrecorded" and is likewise never trusted: an
	// unbounded read that is loud and slow beats a bounded read that is wrong.
	MaxEffective time.Time

	// SettledPositions and SettledCash are the settled-basis fold at the same
	// watermark — the checkpoint's half of Book.SettledPositions (#1043).
	//
	// A CHECKPOINT MUST CARRY BOTH BASES OR THE MATERIALIZED BOOK IS WRONG ON ONE
	// OF THEM. MaterializeCurrent restores from here and folds only the tail, so a
	// snapshot that omitted the settled view would produce a book whose settled
	// balances are short by everything the checkpoint absorbed — a number that is
	// not late but WRONG, and wrong in the direction that overstates nothing while
	// understating what the fund owns.
	SettledPositions map[string]*Position
	SettledCash      map[string]*big.Rat

	// SettlementStated is whether this checkpoint carries a settled view at all.
	// FALSE for one written before the settlement axis existed: its settled maps
	// are empty because nobody wrote them, not because nothing settled. Restoring
	// from it clears the book's SettlementBasisComplete, so the settled read fails
	// closed until the Snapshotter's next pass rewrites the checkpoint — the same
	// stance MaxEffective takes on a fenceless one, for the same reason.
	SettlementStated bool
	// UnknownSettlement and PendingSettlement carry the two entry counts across a
	// checkpoint. Without them a restored book would report zero unknowns and
	// declare a settled basis it cannot support.
	UnknownSettlement int
	PendingSettlement int
}

// Snapshot captures the book's current state with the given knowledge watermark.
// The returned snapshot is a deep copy — mutating the book afterwards does not
// corrupt it.
func (b *Book) Snapshot(through time.Time) *Snapshot {
	s := &Snapshot{
		PortfolioID:      b.PortfolioID,
		Positions:        make(map[string]*Position, len(b.Positions)),
		Cash:             make(map[string]*big.Rat, len(b.Cash)),
		Accrued:          make(map[string]*big.Rat, len(b.Accrued)),
		SettledPositions: make(map[string]*Position, len(b.SettledPositions)),
		SettledCash:      make(map[string]*big.Rat, len(b.SettledCash)),
		Through:          through,
		MaxEffective:     b.maxEffective,
		// A LIVE BOOK ALWAYS STATES ITS SETTLED VIEW, even when that view is empty
		// and every entry behind it was unknown — the counts below are what makes
		// the difference readable. Only LoadSnapshot can produce a false here, for
		// a row written before the settlement axis existed.
		SettlementStated:  b.settlementStated,
		UnknownSettlement: b.unknownSettlement,
		PendingSettlement: b.pendingSettlement,
	}
	copyPositions(s.Positions, b.Positions)
	copyPositions(s.SettledPositions, b.SettledPositions)
	for k, v := range b.Cash {
		s.Cash[k] = new(big.Rat).Set(v)
	}
	for k, v := range b.SettledCash {
		s.SettledCash[k] = new(big.Rat).Set(v)
	}
	for k, v := range b.Accrued {
		s.Accrued[k] = new(big.Rat).Set(v)
	}
	return s
}

// copyPositions deep-copies a holdings map into dst. One implementation, because
// the snapshot and the restore each carry TWO of them now (traded and settled)
// and four hand-written copy loops is how one of them ends up sharing a *big.Rat
// with the book it was supposed to detach from.
func copyPositions(dst, src map[string]*Position) {
	for k, p := range src {
		dst[k] = &Position{
			Qty:      new(big.Rat).Set(p.Qty),
			AvgCost:  new(big.Rat).Set(p.AvgCost),
			Realized: new(big.Rat).Set(p.Realized),
		}
	}
}

// RestoreBook rebuilds a book from a snapshot. The dedup set starts empty, so a
// subsequent fold of the journal TAIL (entries after the watermark) is safe; do
// not re-apply entries already folded into the snapshot.
func RestoreBook(s *Snapshot) *Book {
	b := NewBook(s.PortfolioID)
	b.maxEffective = s.MaxEffective
	copyPositions(b.Positions, s.Positions)
	copyPositions(b.SettledPositions, s.SettledPositions)
	for k, v := range s.Cash {
		b.Cash[k] = new(big.Rat).Set(v)
	}
	for k, v := range s.SettledCash {
		b.SettledCash[k] = new(big.Rat).Set(v)
	}
	for k, v := range s.Accrued {
		b.Accrued[k] = new(big.Rat).Set(v)
	}
	// THE CHECKPOINT DECIDES WHETHER THE RESTORED BOOK MAY ANSWER A SETTLED
	// QUESTION. A row written before the settlement axis existed states nothing,
	// and a book restored from it must not report an empty settled view as a
	// complete one — see Snapshot.SettlementStated.
	b.settlementStated = s.SettlementStated
	b.unknownSettlement = s.UnknownSettlement
	b.pendingSettlement = s.PendingSettlement
	return b
}

// FullScanReason says why a materialization could not be served from a
// checkpoint and read the whole journal instead. Empty means it was bounded.
//
// It is returned rather than logged because "nothing configured" and "checked,
// and fine" must never look the same: the pre-#229 code took the unbounded path
// on every single request and said nothing, which is precisely why a snapshot
// subsystem that was never wired went unnoticed from the day it was written.
type FullScanReason string

const (
	// FullScanNoSnapshot: the portfolio has no checkpoint yet. Expected for a
	// new portfolio; SUSTAINED means the Snapshotter is not running or not
	// keeping up.
	FullScanNoSnapshot FullScanReason = "no_snapshot"
	// FullScanBackdatedTail: a checkpoint exists but the tail contains an entry
	// effective BEFORE the checkpoint's fence, so resuming from it would not
	// equal a replay. See Snapshot.MaxEffective.
	FullScanBackdatedTail FullScanReason = "backdated_tail"
	// FullScanUnfencedSnapshot: the checkpoint records no MaxEffective, so the
	// backdating fence cannot be evaluated and the checkpoint is not trusted.
	FullScanUnfencedSnapshot FullScanReason = "unfenced_snapshot"
)

// MaterializeCurrent returns the current-knowledge book from the store.
//
// IT LOADS THE CHECKPOINT FIRST, ON PURPOSE (#229). Until then it called
// Journal — the entire lifetime journal, no bounds, no LIMIT — and only THEN
// loaded the snapshot, so the checkpoint bounded the in-memory fold and never
// the query. Every NAV request selected every entry the portfolio had ever had
// and allocated three *big.Rat per row while holding one pool connection. The
// comment that used to sit here claimed it was "bounded by the snapshot"; it
// was not, and could not have been, because SaveSnapshot had no caller in the
// tree and ledger_snapshots was a permanently empty table.
//
// The second return value names why the bounded path was not taken, so a caller
// can count it. An empty reason means the read was served from a checkpoint.
func MaterializeCurrent(ctx context.Context, st Store, portfolioID string) (*Book, FullScanReason, error) {
	snap, err := st.LoadSnapshot(ctx, portfolioID)
	if err != nil && !errors.Is(err, ErrNoSnapshot) {
		return nil, "", err
	}
	if err != nil {
		b, ferr := replayAll(ctx, st, portfolioID)
		return b, FullScanNoSnapshot, ferr
	}
	if snap.MaxEffective.IsZero() {
		b, ferr := replayAll(ctx, st, portfolioID)
		return b, FullScanUnfencedSnapshot, ferr
	}

	// The bounded read: only what the checkpoint has not absorbed.
	tail, err := st.JournalSince(ctx, portfolioID, snap.Through)
	if err != nil {
		return nil, "", err
	}
	for _, e := range tail {
		if e.Effective.Before(snap.MaxEffective) {
			// A backdated entry cannot be folded onto a checkpoint (see
			// Snapshot.MaxEffective). Pay for the replay; the Snapshotter's
			// next pass rebuilds the fence and the next read is bounded again.
			b, ferr := replayAll(ctx, st, portfolioID)
			return b, FullScanBackdatedTail, ferr
		}
	}

	b := RestoreBook(snap)
	for _, e := range sortedFor(tail, time.Time{}, time.Time{}, false) {
		b.Apply(e)
	}
	return b, "", nil
}

// replayAll is the unbounded fallback: the whole journal, folded from empty.
// Correct at any journal size and unusable at a large one — every caller
// records a FullScanReason so that cost is visible rather than assumed away.
func replayAll(ctx context.Context, st Store, portfolioID string) (*Book, error) {
	events, err := st.Journal(ctx, portfolioID)
	if err != nil {
		return nil, err
	}
	return Replay(portfolioID, events), nil
}

// MemoryStore is the in-process Store. Goroutine-safe.
type MemoryStore struct {
	mu        sync.RWMutex
	journal   map[string][]*Event // portfolio -> entries
	seen      map[string]bool     // entry dedup across the journal
	snapshots map[string]*Snapshot
	outbox    *outbox.Memory
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		journal:   make(map[string][]*Event),
		seen:      make(map[string]bool),
		snapshots: make(map[string]*Snapshot),
		outbox:    outbox.NewMemory(),
	}
}

// Outbox is the in-process queue this store enqueues announcements into. Never
// nil, so a Folder built on this seam always has somewhere to put one.
func (m *MemoryStore) Outbox() outbox.Queue { return m.outbox }

func (m *MemoryStore) Append(ctx context.Context, e *Event, announce Announcer) error {
	if e == nil || e.EntryID == "" {
		return errors.New("ledger: cannot append entry with empty entry_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen[e.EntryID] {
		// IDEMPOTENT, AND THAT INCLUDES THE ANNOUNCEMENT. A redelivered entry
		// changes no balance, so re-announcing would enqueue a duplicate level for
		// a fold that did not happen — harmless on the wire and noise in the
		// outbox. Postgres reaches the same place by a different route: its INSERT
		// is ON CONFLICT DO NOTHING, so the level the announcer computes is
		// unchanged and the record it enqueues is a correct restatement.
		return nil
	}
	m.seen[e.EntryID] = true
	m.journal[e.PortfolioID] = append(m.journal[e.PortfolioID], e)

	// ENQUEUED UNDER THE SAME LOCK THAT WROTE THE ENTRY — this store's whole
	// equivalent of the transaction Postgres.Append opens, and the same argument
	// order.MemoryStore.Create makes. The announcer reads m through the unexported
	// readers below, which do not re-take the lock.
	if announce != nil {
		records, err := announce(ctx, memoryReader{m})
		if err != nil {
			return err
		}
		if err := m.outbox.Append(records...); err != nil {
			return err
		}
	}
	return nil
}

// memoryReader is the MemoryStore seen from inside its own write lock: the same
// journal and snapshots, with the mutex already held.
//
// IT EXISTS BECAUSE THE ANNOUNCER READS THE BOOK IT IS ANNOUNCING. Calling
// m.Journal from inside Append would take m.mu a second time and deadlock, and
// dropping the lock around the announcer would let another fold interleave
// between the entry and the level computed from it — the reordering the durable
// store's advisory lock exists to prevent, reintroduced in the seam every
// DB-free test runs on.
type memoryReader struct{ m *MemoryStore }

func (r memoryReader) Append(context.Context, *Event, Announcer) error {
	return errors.New("ledger: an announcer must not append")
}

func (r memoryReader) Journal(_ context.Context, portfolioID string) ([]*Event, error) {
	src := r.m.journal[portfolioID]
	out := make([]*Event, len(src))
	copy(out, src)
	return out, nil
}

func (r memoryReader) JournalSince(_ context.Context, portfolioID string, after time.Time) ([]*Event, error) {
	var out []*Event
	for _, e := range r.m.journal[portfolioID] {
		if e.Knowledge.After(after) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (r memoryReader) SaveSnapshot(context.Context, *Snapshot) error {
	return errors.New("ledger: an announcer must not write a snapshot")
}

func (r memoryReader) LoadSnapshot(_ context.Context, portfolioID string) (*Snapshot, error) {
	snap, ok := r.m.snapshots[portfolioID]
	if !ok {
		return nil, ErrNoSnapshot
	}
	return snap, nil
}

func (r memoryReader) StalePortfolios(context.Context, int) ([]string, error) {
	return nil, errors.New("ledger: an announcer must not scan for stale portfolios")
}

func (m *MemoryStore) Journal(_ context.Context, portfolioID string) ([]*Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.journal[portfolioID]
	out := make([]*Event, len(src))
	copy(out, src)
	return out, nil
}

// JournalSince returns the portfolio's entries known strictly after `after`.
// The in-memory store filters where Postgres pushes a range scan down; the
// RESULT must be identical, because this is the seam every DB-free test asserts
// the fold through.
func (m *MemoryStore) JournalSince(_ context.Context, portfolioID string, after time.Time) ([]*Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Event
	for _, e := range m.journal[portfolioID] {
		if e.Knowledge.After(after) {
			out = append(out, e)
		}
	}
	return out, nil
}

// StalePortfolios returns portfolios whose journal has moved past their
// snapshot watermark. Sorted, so a repeated call under a limit is stable rather
// than starving whichever portfolio the map iteration happened to skip.
func (m *MemoryStore) StalePortfolios(_ context.Context, limit int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for id, entries := range m.journal {
		snap, ok := m.snapshots[id]
		for _, e := range entries {
			if !ok || e.Knowledge.After(snap.Through) {
				out = append(out, id)
				break
			}
		}
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) SaveSnapshot(_ context.Context, snap *Snapshot) error {
	if snap == nil || snap.PortfolioID == "" {
		return errors.New("ledger: cannot save snapshot with empty portfolio_id")
	}
	if snap.MaxEffective.IsZero() {
		// A checkpoint without its backdating fence is one MaterializeCurrent
		// will never trust, so storing it buys nothing and hides the mistake
		// behind a permanent, silent full scan. Refuse it at the seam instead.
		return errors.New("ledger: cannot save snapshot with no MaxEffective fence")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Monotonic watermark, matching the Postgres upsert's WHERE clause — see
	// there for why moving it backwards is worse than declining the write.
	if cur, ok := m.snapshots[snap.PortfolioID]; ok && snap.Through.Before(cur.Through) {
		return nil
	}
	m.snapshots[snap.PortfolioID] = snap
	return nil
}

func (m *MemoryStore) LoadSnapshot(_ context.Context, portfolioID string) (*Snapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snap, ok := m.snapshots[portfolioID]
	if !ok {
		return nil, ErrNoSnapshot
	}
	return snap, nil
}
