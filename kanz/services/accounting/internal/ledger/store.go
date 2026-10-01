package ledger

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
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
	// JournalSince reads only entries committed after a dense portfolio append
	// position. Effective and Knowledge remain independent PIT axes.
	JournalSince(ctx context.Context, portfolioID string, after int64) ([]*Event, error)
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

var ErrStaleSnapshot = errors.New("ledger: checkpoint lacks a valid committed prefix")

// Snapshot is a point-in-time materialization of a book plus the committed
// append position it represents — the PERS-01 checkpoint that bounds replay
// to the journal tail. A bitemporal as-of read (ReplayAsOf) bypasses the snapshot
// and folds the journal directly; the snapshot only accelerates the
// current-knowledge book.
type Snapshot struct {
	commitPrefix    bool      // private full-read receipt permits a safe prefix behind the latest head
	JournalPosition int64     // committed source entries represented, including scheduled effects
	NextEffective   time.Time // checkpoint expires when this known economic effect becomes due
	ActionCount     int64     // rejects a checkpoint computed before a concurrent action append
	PortfolioID     string
	Positions       map[string]*Position
	Cash            map[string]*big.Rat
	Accrued         map[string]*big.Rat
	// Through is descriptive knowledge metadata, never a journal tail cursor.
	Through time.Time
	// MaxEffective is the latest effective_time folded into this snapshot — the
	// fence that makes resuming from it equal to a full replay.
	//
	// The fold is ORDER-SENSITIVE. Weighted-average cost realizes P&L in
	// sequence, and the canonical order is (effective, knowledge, entry_id).
	// A checkpoint covers COMMIT order, so a tail entry backdated before
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
		commitPrefix:     b.commitPrefix,
		JournalPosition:  b.journalPosition,
		NextEffective:    b.nextEffective,
		ActionCount:      b.actionCount,
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
	b.commitPrefix = s.commitPrefix
	b.journalPosition = s.JournalPosition
	b.actionCount = s.ActionCount
	b.nextEffective = s.NextEffective
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
	FullScanNoSnapshot     FullScanReason = "no_snapshot"
	FullScanActionRevision FullScanReason = "corporate_action_revision"
	FullScanFutureSnapshot FullScanReason = "future_snapshot"
	FullScanDueSnapshot    FullScanReason = "due_snapshot"
	// FullScanBackdatedTail: a checkpoint exists but the tail contains an entry
	// effective AT OR BEFORE the checkpoint's fence, so resuming from it would not
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
	if consistent, ok := st.(interface {
		currentBook(context.Context, string, time.Time) (*Book, FullScanReason, error)
	}); ok {
		return consistent.currentBook(ctx, portfolioID, time.Now())
	}
	return materializeCurrentAt(ctx, st, portfolioID, time.Now())
}

// Checkpoint and tail must belong to one view. Otherwise a concurrently
// invalidated checkpoint can be combined with a newer tail that omits the
// backdated announcement responsible for invalidating it.
func (m *MemoryStore) currentBook(ctx context.Context, portfolioID string, now time.Time) (*Book, FullScanReason, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return materializeCurrentAt(ctx, memoryReader{m: m}, portfolioID, now)
}

func materializeCurrentAt(ctx context.Context, st Store, portfolioID string, now time.Time) (*Book, FullScanReason, error) {
	snap, err := st.LoadSnapshot(ctx, portfolioID)
	if err != nil && !errors.Is(err, ErrNoSnapshot) {
		return nil, "", err
	}
	if err != nil {
		b, ferr := replayAll(ctx, st, portfolioID, now)
		return b, FullScanNoSnapshot, ferr
	}
	if snap.MaxEffective.IsZero() || snap.JournalPosition <= 0 {
		b, ferr := replayAll(ctx, st, portfolioID, now)
		return b, FullScanUnfencedSnapshot, ferr
	}

	if !snap.NextEffective.IsZero() && !now.Before(snap.NextEffective) {
		b, ferr := replayAll(ctx, st, portfolioID, now)
		return b, FullScanDueSnapshot, ferr
	}
	if snap.MaxEffective.After(now) {
		b, ferr := replayAll(ctx, st, portfolioID, now)
		return b, FullScanFutureSnapshot, ferr
	}

	// The bounded read: only what the checkpoint has not absorbed.
	tail, err := st.JournalSince(ctx, portfolioID, snap.JournalPosition)
	if err != nil {
		return nil, "", err
	}
	for _, e := range tail {
		if e.Type == EntryCorporateAction {
			b, ferr := replayAll(ctx, st, portfolioID, now)
			return b, FullScanActionRevision, ferr
		}
		if !e.Effective.After(snap.MaxEffective) {
			// An earlier or equal effective time may sort inside the checkpoint (see
			// Snapshot.MaxEffective). Pay for the replay; the Snapshotter's
			// next pass rebuilds the fence and the next read is bounded again.
			b, ferr := replayAll(ctx, st, portfolioID, now)
			return b, FullScanBackdatedTail, ferr
		}
	}

	b := RestoreBook(snap)
	b.applyUntil(tail, now, time.Time{})
	b.journalPosition += countJournalEntries(portfolioID, tail, time.Time{})
	b.commitPrefix = b.commitPrefix && hasJournalReadPosition(portfolioID, tail, b.journalPosition)
	return b, "", nil
}

// replayAll is the unbounded fallback: the whole journal, folded from empty.
// Correct at any journal size and unusable at a large one — every caller
// records a FullScanReason so that cost is visible rather than assumed away.
func replayAll(ctx context.Context, st Store, portfolioID string, now time.Time) (*Book, error) {
	events, err := st.Journal(ctx, portfolioID)
	if err != nil {
		return nil, err
	}
	return ReplayAsOf(portfolioID, events, now, time.Time{}), nil
}

// MemoryStore is the in-process Store. Goroutine-safe.
type MemoryStore struct {
	mu        sync.RWMutex
	journal   map[string][]*Event // portfolio -> entries
	seen      map[string]*Event   // immutable entry and dedup identity across the journal
	snapshots map[string]*Snapshot
	outbox    *outbox.Memory
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		journal:   make(map[string][]*Event),
		seen:      make(map[string]*Event),
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
	fill, err := executionFromEntry(e)
	if err != nil {
		return err
	}
	owned := *e
	owned.journalReadPosition = 0
	if e.Action != nil {
		a := *e.Action
		if a.Ratio != nil {
			a.Ratio = new(big.Rat).Set(a.Ratio)
		}
		if a.PerUnit != nil {
			a.PerUnit = new(big.Rat).Set(a.PerUnit)
		}
		owned.Action = &a
	}
	owned.ExecutionEvidence = append([]byte(nil), e.ExecutionEvidence...)
	if e.Quantity != nil {
		owned.Quantity = new(big.Rat).Set(e.Quantity)
	}
	if e.Price != nil {
		owned.Price = new(big.Rat).Set(e.Price)
	}
	if e.Cash != nil {
		owned.Cash = new(big.Rat).Set(e.Cash)
	}
	e = &owned
	if isCashMovement(e) {
		if !validCashEntry(e) {
			return ErrCashConflict
		}
		owned := *e
		owned.Cash = new(big.Rat).Set(e.Cash)
		e = &owned
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.Type == EntryCorporateAction {
		if err := ValidateActionEntry(e); err != nil {
			return err
		}
		if old := m.seen[e.EntryID]; old != nil {
			if old.Action == nil || !sameActionEntry(e, old) {
				return ErrActionConflict
			}
			return nil
		}
		var head *Event
		for _, old := range m.journal[e.PortfolioID] {
			if old.Action != nil && old.Action.ActionID == e.Action.ActionID && (head == nil || old.Action.Revision > head.Action.Revision) {
				head = old
			}
		}
		if _, err := checkActionRevision(e, head); err != nil {
			return err
		}
	}
	if fill != nil {
		legacy := "fill:" + fill.GetFillId()
		if legacy != e.EntryID && m.seen[legacy] != nil {
			return ErrLegacyExecutionUnknown
		}
		if original := m.seen[e.EntryID]; original != nil {
			if len(original.ExecutionEvidence) == 0 {
				return ErrLegacyExecutionUnknown
			}
			oldFill, err := executionFromEntry(original)
			if err != nil {
				return err
			}
			if original.PortfolioID != e.PortfolioID {
				return fillfact.ErrExecutionIdentityConflict
			}
			var revisions []*orderpb.Fill
			for _, entry := range m.journal[e.PortfolioID] {
				if len(entry.ExecutionEvidence) == 0 {
					continue
				}
				var revision orderpb.Fill
				if err := proto.Unmarshal(entry.ExecutionEvidence, &revision); err != nil {
					return err
				}
				if fillfact.ExecutionKey(&revision) == fillfact.ExecutionKey(fill) && revision.GetRecovery().GetFeeApproval() != nil {
					if len(revisions) >= fillfact.MaxMemoryFeeRevisions {
						return fillfact.ErrFeeRevision
					}
					revisions = append(revisions, &revision)
				}
			}
			if fill.GetRecovery().GetFeeApproval() != nil {
				delta, fresh, err := fillfact.CheckMemoryFeeRevision(oldFill, revisions, fill)
				if err != nil {
					return err
				}
				if fresh {
					e = feeAdjustment(e, fill, delta)
				}
			} else if !fillfact.SameExecution(fillfact.MemoryFeeHead(oldFill, revisions), fill) && !(fill.Recovery == nil && fillfact.SameExecution(oldFill, fill)) {
				return fillfact.ErrExecutionIdentityConflict
			}
		}
		if fill.GetRecovery().GetFeeApproval() != nil {
			if _, err := fillfact.FeeRevisionTerms(fill); err != nil {
				return err
			}
		}
	}
	if m.seen[e.EntryID] != nil {
		if isCashMovement(e) {
			for _, entries := range m.journal {
				for _, old := range entries {
					if old.EntryID == e.EntryID && !sameCash(e, old) {
						return ErrCashConflict
					}
				}
			}
		}
		// IDEMPOTENT, AND THAT INCLUDES THE ANNOUNCEMENT. A redelivered entry
		// changes no balance, so re-announcing would enqueue a duplicate level for
		// a fold that did not happen — harmless on the wire and noise in the
		// outbox. Postgres reaches the same place by a different route: its INSERT
		// is ON CONFLICT DO NOTHING, so the level the announcer computes is
		// unchanged and the record it enqueues is a correct restatement.
		if fill.GetRecovery() != nil && announce != nil {
			// The execution can already be booked while this new recovery case
			// still needs proof from this book. Acknowledge without reposting it.
			records, err := announce(ctx, memoryReader{m: m})
			if err != nil {
				return err
			}
			return m.outbox.Append(records...)
		}
		return nil
	}
	// The announcer reads a staged entry without changing committed state. All
	// failure points precede the journal and claim writes, matching a SQL rollback
	// without temporarily deleting or truncating the financial journal.
	if announce != nil {
		records, err := announce(ctx, memoryReader{m: m, pending: e})
		if err != nil {
			return err
		}
		if err := m.outbox.Append(records...); err != nil {
			return err
		}
	}
	m.seen[e.EntryID] = e
	m.journal[e.PortfolioID] = append(m.journal[e.PortfolioID], e)
	if e.Type == EntryCorporateAction {
		// The checkpoint is derived, never financial history. Only invalidate
		// after the staged announcement/outbox succeeded, matching SQL rollback.
		delete(m.snapshots, e.PortfolioID)
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
type memoryReader struct {
	m       *MemoryStore
	pending *Event
}

func (r memoryReader) Append(context.Context, *Event, Announcer) error {
	return errors.New("ledger: an announcer must not append")
}

func (r memoryReader) Journal(ctx context.Context, portfolioID string) ([]*Event, error) {
	return r.JournalSince(ctx, portfolioID, 0)
}

func (r memoryReader) JournalSince(_ context.Context, portfolioID string, after int64) ([]*Event, error) {
	src := r.m.journal[portfolioID]
	if after < 0 || after > int64(len(src)) {
		return nil, ErrStaleSnapshot
	}
	position := int64(len(src))
	pending := r.pending != nil && r.pending.PortfolioID == portfolioID
	if pending {
		position++
	}
	out := make([]*Event, 0, position-after)
	copyEntry := func(e *Event) {
		owned := *e
		owned.journalReadPosition = position
		out = append(out, &owned)
	}
	for _, e := range src[after:] {
		copyEntry(e)
	}
	if pending {
		copyEntry(r.pending)
	}
	return out, nil
}

func (r memoryReader) SaveSnapshot(context.Context, *Snapshot) error {
	return errors.New("ledger: an announcer must not write a snapshot")
}

func (r memoryReader) LoadSnapshot(_ context.Context, portfolioID string) (*Snapshot, error) {
	if r.pending != nil && r.pending.Type == EntryCorporateAction && r.pending.PortfolioID == portfolioID {
		return nil, ErrNoSnapshot
	}
	snap, ok := r.m.snapshots[portfolioID]
	if !ok {
		return nil, ErrNoSnapshot
	}
	return snap, nil
}

func (r memoryReader) StalePortfolios(context.Context, int) ([]string, error) {
	return nil, errors.New("ledger: an announcer must not scan for stale portfolios")
}

func (m *MemoryStore) Journal(ctx context.Context, portfolioID string) ([]*Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return (memoryReader{m: m}).Journal(ctx, portfolioID)
}

// JournalSince slices the append-ordered log: work is proportional to the tail.
func (m *MemoryStore) JournalSince(ctx context.Context, portfolioID string, after int64) ([]*Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return (memoryReader{m: m}).JournalSince(ctx, portfolioID, after)
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
		if !ok || snap.JournalPosition != int64(len(entries)) || (!snap.NextEffective.IsZero() && !time.Now().Before(snap.NextEffective)) {
			out = append(out, id)
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
	if snap.ActionCount != countActionEntries(snap.PortfolioID, m.journal[snap.PortfolioID], time.Time{}) {
		return ErrStaleActionSnapshot
	}
	head := int64(len(m.journal[snap.PortfolioID]))
	if snap.JournalPosition > head || snap.JournalPosition <= 0 || (!snap.commitPrefix && snap.JournalPosition != head) {
		return ErrStaleSnapshot
	}
	// Monotonic watermark, matching the Postgres upsert's WHERE clause — see
	// there for why moving it backwards is worse than declining the write.
	if cur, ok := m.snapshots[snap.PortfolioID]; ok && snap.JournalPosition < cur.JournalPosition {
		return nil
	}
	owned := *snap
	owned.commitPrefix = true
	m.snapshots[snap.PortfolioID] = &owned
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
