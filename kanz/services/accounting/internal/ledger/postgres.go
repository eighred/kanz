package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/outbox"
)

// Postgres is the durable Store backed by the 0001_ledger.sql schema — the
// append-only journal plus the periodic book snapshot (PARITY-02a). It reuses
// the risk-engine persist.Postgres stance (pgxpool, RLS-scoped writes via the
// app.tenant_id GUC) and preserves exact fold semantics: the in-memory
// MemoryStore stays the test seam, and Replay/MaterializeCurrent over this
// store's Journal reconstruct byte-identical books.
type Postgres struct {
	pool *pgxpool.Pool
	// q is what the READS run on. It is the pool everywhere except inside
	// Append, where withTx swaps in the open transaction — see withTx.
	q querier
}

// querier is the read/write surface *pgxpool.Pool and pgx.Tx have in common.
//
// IT EXISTS SO A BALANCE CAN BE COMPUTED FROM INSIDE THE APPEND'S OWN
// TRANSACTION (#804). The cash announcement is now enqueued with the entry that
// caused it, and a level read through the POOL from inside that transaction
// would not see the uncommitted entry — it would announce the balance as it was
// BEFORE the fold, every time, which is a wrong number rather than a late one.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NewPostgres returns a Postgres store over an existing pool. The caller owns
// the pool lifecycle (Close), matching persist.NewPostgres.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool, q: pool} }

// withTx returns a Store whose reads run inside tx.
//
// A SHALLOW COPY, NOT A MUTATION. Postgres is shared by every goroutine folding
// into this book; swapping the field in place would point one fold's reads at
// another fold's transaction. The copy is handed only to the announcer callback
// and dies with the transaction.
//
// Its WRITES are the ones that must not be reached: an announcer exists to READ
// the book it is about to announce, and a second Append from inside an Append
// would take the same advisory lock it is already holding and enqueue a record
// nothing ordered. appendUnavailable says so rather than deadlocking or
// succeeding quietly.
func (p *Postgres) withTx(tx pgx.Tx) *Postgres { return &Postgres{pool: p.pool, q: tx} }

func (p *Postgres) currentBook(ctx context.Context, portfolioID string, now time.Time) (*Book, FullScanReason, error) {
	if _, insideAppend := p.q.(pgx.Tx); insideAppend {
		// Append already owns the portfolio lock, and its announcement must see
		// the uncommitted entry rather than open a second transaction.
		return materializeCurrentAt(ctx, p, portfolioID, now)
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	book, reason, err := materializeCurrentAt(ctx, p.withTx(tx), portfolioID, now)
	if err != nil {
		return nil, reason, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, reason, err
	}
	return book, reason, nil
}

// Outbox is the durable queue this store enqueues announcements into. Built on
// the same pool the journal is written through, so the relay reads under the
// same RLS scope that wrote the record and there is no cross-tenant read to be
// had.
func (p *Postgres) Outbox() outbox.Queue { return outbox.NewPostgres(p.pool, "accounting") }

// Append records one journal entry, idempotent on entry_id (ON CONFLICT DO
// NOTHING) — the journal is exactly-once over an at-least-once producer.
//
// announce is called WITH THE ENTRY WRITTEN AND NOT YET COMMITTED, over a Store
// whose reads see it, and the records it returns are enqueued in the same
// transaction (#804). Nil announces nothing, which is what the MemoryStore seam
// and every read-only caller pass.
//
// # Why the announcement had to move in here
//
// It used to be a second, independent write after Append returned, and its error
// was discarded on purpose — the ledger is the book of record and nacking the
// fold to retry a publish would turn a broker blip into a stalled ledger. That
// reasoning was right. What it left out was the recovery: the ONLY thing that
// re-announced a portfolio was the next fold for that portfolio, so a single
// blip refused every order for it under a buying-power mandate until unrelated
// activity happened to arrive. Enqueued here, the entry and its announcement
// commit together and the relay retries the publish on its own schedule.
func (p *Postgres) Append(ctx context.Context, e *Event, announce Announcer) error {
	if e == nil || e.EntryID == "" {
		return errors.New("ledger: cannot append entry with empty entry_id")
	}
	action, err := encodeAction(e.Action)
	if err != nil {
		return fmt.Errorf("encode action %s: %w", e.EntryID, err)
	}

	// THE WRITE DECLARES WHOSE COLLATERAL IT MOVES.
	//
	// An exchange liquidates per ACCOUNT, so an entry that cannot say which account it
	// settled against is an entry the books cannot be trusted on. app.venue_account_id
	// is that declaration, and the engine RAISES (42501) on an INSERT that omits it —
	// so a future code path that forgets cannot silently misfile a fill into another
	// portfolio's collateral. Empty is a legitimate declaration: an entry that touches
	// no exchange account at all (a manual cash movement, a corporate action).
	//
	// It is TRANSACTION-scoped (set_config local=true), not session-scoped: this pool
	// is shared, one connection serves many accounts in turn, and a declaration that
	// outlived its transaction would be the next entry's silent default — which is the
	// exact bug this guards against, reintroduced by the guard itself.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("append entry %s: begin: %w", e.EntryID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit
	if isCashMovement(e) {
		duplicate, err := checkCashEntry(ctx, tx, e)
		if err != nil {
			return err
		}
		if duplicate {
			return tx.Commit(ctx)
		}
	}

	// THE PER-PORTFOLIO LOCK, FIRST, AND IT IS WHAT MAKES THE OUTBOX ORDERED (#804).
	//
	// The relay publishes one partition key in id order, and id order is COMMIT
	// order only where a key's transactions cannot interleave. The OMS gets that
	// free: every enqueue there rides a compare-and-swap, so two transactions for
	// one order cannot both commit. This journal has no such write — it is
	// append-only with ON CONFLICT DO NOTHING — so two folds for one portfolio
	// would otherwise commit independently, and the relay would publish the older
	// BALANCE last. internal/cashview replaces its map entry unconditionally with
	// no as_of guard, so that stale level is what every buying-power check then
	// reads until the next fold. Same defect as #795, same lock shape as
	// position.lockInstrument.
	//
	// It is taken before the venue-account declaration and before the INSERT, so
	// every transaction acquires in one order and two portfolios never contend.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext(app_current_tenant()), hashtext($1))`,
		e.PortfolioID); err != nil {
		return fmt.Errorf("append entry %s: lock portfolio %s: %w", e.EntryID, e.PortfolioID, err)
	}

	if _, err := tx.Exec(ctx, `SELECT set_config('app.venue_account_id', $1, true)`, e.VenueAccountID); err != nil {
		return fmt.Errorf("append entry %s: declare venue account: %w", e.EntryID, err)
	}
	if e.Type == EntryCorporateAction {
		duplicate, err := checkPostgresAction(ctx, tx, e)
		if err != nil {
			return err
		}
		if duplicate {
			return tx.Commit(ctx)
		}
		// Announcement time may precede an existing checkpoint's watermark.
		// Invalidate in the SAME transaction, before its announcement reads the
		// book; JournalSince alone cannot discover a backdated knowledge time.
		if _, err := tx.Exec(ctx, `UPDATE ledger_snapshots SET corporate_action_version = NULL WHERE portfolio_id = $1`, e.PortfolioID); err != nil {
			return fmt.Errorf("invalidate action checkpoint: %w", err)
		}
	}
	prepared, err := prepareExecutionEntry(ctx, tx, e)
	if err != nil {
		return err
	}
	if prepared != nil {
		e = prepared
		_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries
			(tenant_id, entry_id, portfolio_id, venue_account_id, entry_type, instrument_id,
			 quantity, price, cash, cash_currency, action,
			 effective_time, knowledge_time, settlement_status, settlement_date, source_ref, execution_evidence)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (tenant_id, entry_id) DO NOTHING
	`, e.EntryID, e.PortfolioID, e.VenueAccountID, int(e.Type), e.InstrumentID,
			ratText(e.Quantity), ratText(e.Price), ratText(e.Cash), e.CashCurrency, action,
			e.Effective, e.Knowledge, int(e.SettlementBasis), nullTime(e.SettlementDate), e.SourceRef, e.ExecutionEvidence)
		if err != nil {
			return fmt.Errorf("append entry %s: %w", e.EntryID, err)
		}
	}
	// THE ANNOUNCEMENT COMMITS WITH THE ENTRY (#804, #292). It reads through
	// p.withTx(tx), so the level it computes INCLUDES the entry above — the fold's
	// own effect, which is the whole point of announcing it.
	if announce != nil {
		records, aerr := announce(ctx, p.withTx(tx))
		if aerr != nil {
			return fmt.Errorf("append entry %s: announce: %w", e.EntryID, aerr)
		}
		if err := outbox.Enqueue(ctx, tx, records...); err != nil {
			return fmt.Errorf("append entry %s: enqueue announcement: %w", e.EntryID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("append entry %s: commit: %w", e.EntryID, err)
	}
	return nil
}

// Journal returns a portfolio's ENTIRE journal in the canonical fold order
// (effective, knowledge, entry_id) — served by ledger_entries_bitemporal_idx,
// so Replay over the result reproduces the in-memory book exactly.
//
// UNBOUNDED BY CONSTRUCTION: no time bounds, no LIMIT, every entry the
// portfolio has ever had. That is correct for the two callers that need a whole
// fold — the Snapshotter, and MaterializeCurrent's no-checkpoint fallback — and
// it is the query that made #229 a request-path liability. Reach for
// JournalSince instead unless you genuinely need the whole book from empty.
func (p *Postgres) Journal(ctx context.Context, portfolioID string) ([]*Event, error) {
	return p.journal(ctx, portfolioID, time.Time{}, time.Time{}, nil)
}

// JournalSince seeks the immutable append index, never financial timestamps.
func (p *Postgres) JournalSince(ctx context.Context, portfolioID string, after int64) ([]*Event, error) {
	if after < 0 {
		return nil, ErrStaleSnapshot
	}
	if after == 0 {
		return p.Journal(ctx, portfolioID)
	}
	return p.journal(ctx, portfolioID, time.Time{}, time.Time{}, &after)
}

// JournalAsOf returns replay-ready effects at the two PIT cutoffs. The SQL
// bounds knowledge first; revision selection must precede the effective filter
// so a moved ex-date cannot resurrect a superseded version. Confirmed payments
// are derived replay stages, never additional persisted journal records. Replay
// over this result equals ReplayAsOf over the immutable full journal.
func (p *Postgres) JournalAsOf(ctx context.Context, portfolioID string, effectiveAsOf, knowledgeAsOf time.Time) ([]*Event, error) {
	events, err := p.journal(ctx, portfolioID, time.Time{}, knowledgeAsOf, nil)
	if err != nil {
		return nil, err
	}
	return sortedFor(events, effectiveAsOf, knowledgeAsOf, true), nil
}

// StalePortfolios also discovers untouched legacy portfolios with no head.
// New portfolios compare commit positions, including late/equal-knowledge fills.
func (p *Postgres) StalePortfolios(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, errors.New("ledger: StalePortfolios needs a positive limit")
	}
	rows, err := p.q.Query(ctx, `
		SELECT p.portfolio_id
		FROM (SELECT portfolio_id FROM ledger_heads WHERE position > 0
		      UNION SELECT portfolio_id FROM ledger_entries e WHERE NOT EXISTS
		        (SELECT 1 FROM ledger_heads h WHERE h.portfolio_id=e.portfolio_id)) p
		LEFT JOIN ledger_heads h ON h.portfolio_id=p.portfolio_id
		LEFT JOIN ledger_snapshots s ON s.portfolio_id=p.portfolio_id
		WHERE s.journal_position IS NULL OR s.corporate_action_version IS DISTINCT FROM 2
		   OR h.position IS NULL OR h.position <> s.journal_position OR s.next_effective_time <= now()
		ORDER BY s.updated_at ASC NULLS FIRST, p.portfolio_id
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("query stale portfolios: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stale portfolio: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (p *Postgres) journal(ctx context.Context, portfolioID string, effBound, knowBound time.Time, after *int64) ([]*Event, error) {
	join, positionBound := "", ""
	args := []any{portfolioID, nullTime(effBound), nullTime(knowBound)}
	if after != nil {
		join = ` JOIN ledger_append_positions USING (tenant_id, portfolio_id, entry_id)`
		positionBound = ` AND position > $4`
		args = append(args, *after)
	}
	rows, err := p.q.Query(ctx, `
		SELECT entry_id, portfolio_id, venue_account_id, entry_type, instrument_id,
		       quantity, price, cash, cash_currency, action,
		       effective_time, knowledge_time, settlement_status, settlement_date, source_ref, execution_evidence
		FROM ledger_entries `+join+`
		WHERE portfolio_id = $1
		  AND ($2::timestamptz IS NULL OR effective_time <= $2)
		  AND ($3::timestamptz IS NULL OR knowledge_time <= $3) `+positionBound+`
		ORDER BY effective_time, knowledge_time, entry_id
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("query journal %s: %w", portfolioID, err)
	}
	defer rows.Close()

	var out []*Event
	for rows.Next() {
		var (
			e          Event
			qty, price *string
			cash       *string
			action     []byte
			// NULL settlement_date is "nobody asserted one", which is the Go zero
			// time — scanned through a pointer so the absence stays one value on
			// both sides rather than becoming a sentinel timestamp.
			settlementDate *time.Time
		)
		if err := rows.Scan(&e.EntryID, &e.PortfolioID, &e.VenueAccountID, &e.Type, &e.InstrumentID,
			&qty, &price, &cash, &e.CashCurrency, &action,
			&e.Effective, &e.Knowledge, &e.SettlementBasis, &settlementDate, &e.SourceRef, &e.ExecutionEvidence); err != nil {
			return nil, fmt.Errorf("scan entry %s: %w", portfolioID, err)
		}
		if settlementDate != nil {
			e.SettlementDate = *settlementDate
		}
		if e.Quantity, err = parseRat(qty); err != nil {
			return nil, fmt.Errorf("decode quantity %s: %w", e.EntryID, err)
		}
		if e.Price, err = parseRat(price); err != nil {
			return nil, fmt.Errorf("decode price %s: %w", e.EntryID, err)
		}
		if e.Cash, err = parseRat(cash); err != nil {
			return nil, fmt.Errorf("decode cash %s: %w", e.EntryID, err)
		}
		if e.Action, err = decodeAction(action); err != nil {
			return nil, fmt.Errorf("decode action %s: %w", e.EntryID, err)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if effBound.IsZero() && knowBound.IsZero() {
		position := int64(len(out))
		if after != nil {
			position += *after
		}
		for _, e := range out {
			e.journalReadPosition = position
		}
	}
	return out, nil
}

// SaveSnapshot upserts a portfolio's latest book snapshot (full replace).
//
// ledger_snapshots is deliberately OUTSIDE the WORM trigger 0004 puts on
// ledger_entries, and this UPSERT is why. The journal is the book of record and
// must never be rewritten; a snapshot is a DERIVED CACHE of a prefix of it,
// reproducible from the journal alone and carrying no fact the journal does
// not. Extending WORM here would forbid the only write shape a checkpoint has.
// Nothing is lost: an UPDATE cannot destroy history that lives in the journal.
func (p *Postgres) SaveSnapshot(ctx context.Context, snap *Snapshot) error {
	if snap == nil || snap.PortfolioID == "" {
		return errors.New("ledger: cannot save snapshot with empty portfolio_id")
	}
	if snap.MaxEffective.IsZero() {
		// See MemoryStore.SaveSnapshot: a fenceless checkpoint is one
		// MaterializeCurrent refuses, so writing it would buy a permanent
		// silent full scan and a row that looks like a working cache.
		return errors.New("ledger: cannot save snapshot with no MaxEffective fence")
	}
	positions, err := json.Marshal(encodePositions(snap.Positions))
	if err != nil {
		return fmt.Errorf("encode positions %s: %w", snap.PortfolioID, err)
	}
	cash, err := json.Marshal(encodeRatMap(snap.Cash))
	if err != nil {
		return fmt.Errorf("encode cash %s: %w", snap.PortfolioID, err)
	}
	accrued, err := json.Marshal(encodeRatMap(snap.Accrued))
	if err != nil {
		return fmt.Errorf("encode accrued %s: %w", snap.PortfolioID, err)
	}
	// THE SETTLED HALF OF THE CHECKPOINT (#1043). Written as SQL NULL when the
	// snapshot states no settled view, so the row is indistinguishable from one
	// written before the axis existed — that is the point: both mean "this
	// checkpoint cannot answer a settled question", and LoadSnapshot reads them
	// the same way.
	var settledPositions, settledCash any
	var unknownSettlement, pendingSettlement any
	if snap.SettlementStated {
		if settledPositions, err = json.Marshal(encodePositions(snap.SettledPositions)); err != nil {
			return fmt.Errorf("encode settled positions %s: %w", snap.PortfolioID, err)
		}
		if settledCash, err = json.Marshal(encodeRatMap(snap.SettledCash)); err != nil {
			return fmt.Errorf("encode settled cash %s: %w", snap.PortfolioID, err)
		}
		unknownSettlement = snap.UnknownSettlement
		pendingSettlement = snap.PendingSettlement
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(app_current_tenant()), hashtext($1))`, snap.PortfolioID); err != nil {
		return err
	}
	var actionCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE portfolio_id=$1 AND entry_type=$2 AND action IS NOT NULL`, snap.PortfolioID, int(EntryCorporateAction)).Scan(&actionCount); err != nil {
		return err
	}
	if actionCount != snap.ActionCount {
		return ErrStaleActionSnapshot
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_heads (tenant_id, portfolio_id, position)
		SELECT current_setting('app.tenant_id'), $1, count(*) FROM ledger_entries WHERE portfolio_id=$1
		ON CONFLICT (tenant_id, portfolio_id) DO NOTHING`, snap.PortfolioID); err != nil {
		return err
	}
	var head int64
	if err := tx.QueryRow(ctx, `SELECT position FROM ledger_heads WHERE portfolio_id=$1`, snap.PortfolioID).Scan(&head); err != nil {
		return err
	}
	if snap.JournalPosition > head || snap.JournalPosition <= 0 || (!snap.commitPrefix && snap.JournalPosition != head) {
		return ErrStaleSnapshot
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_snapshots
			(tenant_id, portfolio_id, positions, cash, accrued, through_time,
			 max_effective_time, settled_positions, settled_cash,
			 unknown_settlement, pending_settlement, corporate_action_version, action_count, next_effective_time, journal_position, updated_at)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 2, $11, $12, $13, now())
		ON CONFLICT (tenant_id, portfolio_id) DO UPDATE SET
			positions          = EXCLUDED.positions,
			cash               = EXCLUDED.cash,
			accrued            = EXCLUDED.accrued,
			through_time       = EXCLUDED.through_time,
			max_effective_time = EXCLUDED.max_effective_time,
			settled_positions  = EXCLUDED.settled_positions,
			settled_cash       = EXCLUDED.settled_cash,
			unknown_settlement = EXCLUDED.unknown_settlement,
			pending_settlement = EXCLUDED.pending_settlement,
			corporate_action_version = 2,
			journal_position = EXCLUDED.journal_position,
			action_count = EXCLUDED.action_count,
			next_effective_time = EXCLUDED.next_effective_time,
			updated_at         = now()
		-- A verified prefix may lag a busy head, but cannot replace newer progress.
		-- Financial knowledge metadata does not order commit prefixes.
		WHERE ledger_snapshots.journal_position IS NULL OR EXCLUDED.journal_position >= ledger_snapshots.journal_position
	`, snap.PortfolioID, positions, cash, accrued, snap.Through, snap.MaxEffective,
		settledPositions, settledCash, unknownSettlement, pendingSettlement, snap.ActionCount, nullTime(snap.NextEffective), snap.JournalPosition)
	if err != nil {
		return fmt.Errorf("save snapshot %s: %w", snap.PortfolioID, err)
	}
	return tx.Commit(ctx)
}

// LoadSnapshot returns a portfolio's latest snapshot, or ErrNoSnapshot.
func (p *Postgres) LoadSnapshot(ctx context.Context, portfolioID string) (*Snapshot, error) {
	var (
		positions, cash, accrued []byte
		through                  time.Time
	)
	// max_effective_time is nullable: NULL is "this checkpoint does not state
	// its backdating fence", which MaterializeCurrent refuses to resume from.
	// Scanning through *time.Time keeps that absence as the Go zero time, so
	// "unrecorded" is one value on both sides rather than a sentinel timestamp
	// whose comparison could be written backwards.
	var maxEffective *time.Time
	var actionVersion *int
	var actionCount int64
	var journalPosition *int64
	var nextEffective *time.Time
	// A NULL settled_positions is a checkpoint that STATES NO SETTLED VIEW — see
	// Snapshot.SettlementStated. Scanned into pointers so the absence survives as
	// an absence rather than as an empty map that looks like a computed answer.
	var settledPositions, settledCash []byte
	var unknownSettlement, pendingSettlement *int
	err := p.q.QueryRow(ctx, `
		SELECT positions, cash, accrued, through_time, max_effective_time,
		       settled_positions, settled_cash, unknown_settlement, pending_settlement, corporate_action_version, action_count, next_effective_time, journal_position
		FROM ledger_snapshots WHERE portfolio_id = $1
	`, portfolioID).Scan(&positions, &cash, &accrued, &through, &maxEffective,
		&settledPositions, &settledCash, &unknownSettlement, &pendingSettlement, &actionVersion, &actionCount, &nextEffective, &journalPosition)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, fmt.Errorf("load snapshot %s: %w", portfolioID, err)
	}
	snap := &Snapshot{PortfolioID: portfolioID, Through: through, ActionCount: actionCount}
	if nextEffective != nil {
		snap.NextEffective = *nextEffective
	}
	if journalPosition != nil {
		snap.JournalPosition = *journalPosition
	}
	if maxEffective != nil && actionVersion != nil && *actionVersion == 2 && journalPosition != nil {
		snap.MaxEffective = *maxEffective
		snap.commitPrefix = true
	}
	if snap.Positions, err = decodePositions(positions); err != nil {
		return nil, fmt.Errorf("decode positions %s: %w", portfolioID, err)
	}
	if snap.Cash, err = decodeRatMap(cash); err != nil {
		return nil, fmt.Errorf("decode cash %s: %w", portfolioID, err)
	}
	if snap.Accrued, err = decodeRatMap(accrued); err != nil {
		return nil, fmt.Errorf("decode accrued %s: %w", portfolioID, err)
	}
	snap.SettledPositions = make(map[string]*Position)
	snap.SettledCash = make(map[string]*big.Rat)
	if settledPositions != nil {
		snap.SettlementStated = true
		if snap.SettledPositions, err = decodePositions(settledPositions); err != nil {
			return nil, fmt.Errorf("decode settled positions %s: %w", portfolioID, err)
		}
		if snap.SettledCash, err = decodeRatMap(settledCash); err != nil {
			return nil, fmt.Errorf("decode settled cash %s: %w", portfolioID, err)
		}
		if unknownSettlement != nil {
			snap.UnknownSettlement = *unknownSettlement
		}
		if pendingSettlement != nil {
			snap.PendingSettlement = *pendingSettlement
		}
	}
	return snap, nil
}

// Ping checks store reachability for the readiness probe.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// --- exact-decimal / value encoding ---------------------------------------

// ratText marshals a *big.Rat to its lossless RatString, or nil (⇒ SQL NULL).
func ratText(r *big.Rat) any {
	if r == nil {
		return nil
	}
	return r.RatString()
}

// parseRat reconstructs a *big.Rat from a nullable RatString column.
func parseRat(s *string) (*big.Rat, error) {
	if s == nil {
		return nil, nil
	}
	r, ok := new(big.Rat).SetString(*s)
	if !ok {
		return nil, fmt.Errorf("ledger: malformed rational %q", *s)
	}
	return r, nil
}

// posJSON is the JSON shape of a stored Position (exact rationals as strings).
type posJSON struct {
	Qty      string `json:"qty"`
	Avg      string `json:"avg"`
	Realized string `json:"realized"`
}

func encodePositions(in map[string]*Position) map[string]posJSON {
	out := make(map[string]posJSON, len(in))
	for k, p := range in {
		out[k] = posJSON{
			Qty:      orZero(p.Qty).RatString(),
			Avg:      orZero(p.AvgCost).RatString(),
			Realized: orZero(p.Realized).RatString(),
		}
	}
	return out
}

func decodePositions(b []byte) (map[string]*Position, error) {
	out := make(map[string]*Position)
	if len(b) == 0 {
		return out, nil
	}
	var raw map[string]posJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for k, p := range raw {
		qty, err := mustRat(p.Qty)
		if err != nil {
			return nil, err
		}
		avg, err := mustRat(p.Avg)
		if err != nil {
			return nil, err
		}
		realized, err := mustRat(p.Realized)
		if err != nil {
			return nil, err
		}
		out[k] = &Position{Qty: qty, AvgCost: avg, Realized: realized}
	}
	return out, nil
}

func encodeRatMap(in map[string]*big.Rat) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = orZero(v).RatString()
	}
	return out
}

func decodeRatMap(b []byte) (map[string]*big.Rat, error) {
	out := make(map[string]*big.Rat)
	if len(b) == 0 {
		return out, nil
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for k, s := range raw {
		v, err := mustRat(s)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// actionJSON is the JSON shape of a corporate-action Action.
type actionJSON struct {
	ActionLifecycle
	Kind     int    `json:"kind"`
	Ratio    string `json:"ratio,omitempty"`
	PerUnit  string `json:"per_unit,omitempty"`
	Target   string `json:"target,omitempty"`
	Currency string `json:"currency,omitempty"`
}

func encodeAction(a *Action) (any, error) {
	if a == nil {
		return nil, nil
	}
	aj := actionJSON{ActionLifecycle: a.ActionLifecycle, Kind: int(a.Kind), Target: a.Target, Currency: a.Currency}
	if a.Ratio != nil {
		aj.Ratio = a.Ratio.RatString()
	}
	if a.PerUnit != nil {
		aj.PerUnit = a.PerUnit.RatString()
	}
	return json.Marshal(aj)
}

func decodeAction(b []byte) (*Action, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var aj actionJSON
	if err := json.Unmarshal(b, &aj); err != nil {
		return nil, err
	}
	a := &Action{ActionLifecycle: aj.ActionLifecycle, Kind: CorpActKind(aj.Kind), Target: aj.Target, Currency: aj.Currency}
	if aj.Ratio != "" {
		r, err := mustRat(aj.Ratio)
		if err != nil {
			return nil, err
		}
		a.Ratio = r
	}
	if aj.PerUnit != "" {
		r, err := mustRat(aj.PerUnit)
		if err != nil {
			return nil, err
		}
		a.PerUnit = r
	}
	return a, nil
}

func mustRat(s string) (*big.Rat, error) {
	if s == "" {
		return new(big.Rat), nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("ledger: malformed rational %q", s)
	}
	return r, nil
}

// nullTime maps the Go zero time to SQL NULL so an unbounded axis round-trips
// as NULL (the persist.nullTime stance).
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// Compile-time assertion that Postgres satisfies Store.
var _ Store = (*Postgres)(nil)
