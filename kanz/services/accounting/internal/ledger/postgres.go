package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is the durable Store backed by the 0001_ledger.sql schema — the
// append-only journal plus the periodic book snapshot (PARITY-02a). It reuses
// the risk-engine persist.Postgres stance (pgxpool, RLS-scoped writes via the
// app.tenant_id GUC) and preserves exact fold semantics: the in-memory
// MemoryStore stays the test seam, and Replay/MaterializeCurrent over this
// store's Journal reconstruct byte-identical books.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a Postgres store over an existing pool. The caller owns
// the pool lifecycle (Close), matching persist.NewPostgres.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Append records one journal entry, idempotent on entry_id (ON CONFLICT DO
// NOTHING) — the journal is exactly-once over an at-least-once producer.
func (p *Postgres) Append(ctx context.Context, e *Event) error {
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

	if _, err := tx.Exec(ctx, `SELECT set_config('app.venue_account_id', $1, true)`, e.VenueAccountID); err != nil {
		return fmt.Errorf("append entry %s: declare venue account: %w", e.EntryID, err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries
			(tenant_id, entry_id, portfolio_id, venue_account_id, entry_type, instrument_id,
			 quantity, price, cash, cash_currency, action,
			 effective_time, knowledge_time, source_ref)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (tenant_id, entry_id) DO NOTHING
	`, e.EntryID, e.PortfolioID, e.VenueAccountID, int(e.Type), e.InstrumentID,
		ratText(e.Quantity), ratText(e.Price), ratText(e.Cash), e.CashCurrency, action,
		e.Effective, e.Knowledge, e.SourceRef)
	if err != nil {
		return fmt.Errorf("append entry %s: %w", e.EntryID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("append entry %s: commit: %w", e.EntryID, err)
	}
	return nil
}

// Journal returns a portfolio's entries in the canonical fold order
// (effective, knowledge, entry_id) — served by ledger_entries_bitemporal_idx,
// so Replay over the result reproduces the in-memory book exactly.
func (p *Postgres) Journal(ctx context.Context, portfolioID string) ([]*Event, error) {
	return p.journal(ctx, portfolioID, time.Time{}, time.Time{})
}

// JournalAsOf returns the entries economically effective at or before
// effectiveAsOf and known at or before knowledgeAsOf — the bitemporal
// point-in-time read pushed into Postgres as an indexed range scan. A zero time
// on either axis means "no bound on that axis". Replay over the result equals
// the in-memory ReplayAsOf.
func (p *Postgres) JournalAsOf(ctx context.Context, portfolioID string, effectiveAsOf, knowledgeAsOf time.Time) ([]*Event, error) {
	return p.journal(ctx, portfolioID, effectiveAsOf, knowledgeAsOf)
}

func (p *Postgres) journal(ctx context.Context, portfolioID string, effBound, knowBound time.Time) ([]*Event, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT entry_id, portfolio_id, entry_type, instrument_id,
		       quantity, price, cash, cash_currency, action,
		       effective_time, knowledge_time, source_ref
		FROM ledger_entries
		WHERE portfolio_id = $1
		  AND ($2::timestamptz IS NULL OR effective_time <= $2)
		  AND ($3::timestamptz IS NULL OR knowledge_time <= $3)
		ORDER BY effective_time, knowledge_time, entry_id
	`, portfolioID, nullTime(effBound), nullTime(knowBound))
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
		)
		if err := rows.Scan(&e.EntryID, &e.PortfolioID, &e.Type, &e.InstrumentID,
			&qty, &price, &cash, &e.CashCurrency, &action,
			&e.Effective, &e.Knowledge, &e.SourceRef); err != nil {
			return nil, fmt.Errorf("scan entry %s: %w", portfolioID, err)
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
	return out, rows.Err()
}

// SaveSnapshot upserts a portfolio's latest book snapshot (full replace).
func (p *Postgres) SaveSnapshot(ctx context.Context, snap *Snapshot) error {
	if snap == nil || snap.PortfolioID == "" {
		return errors.New("ledger: cannot save snapshot with empty portfolio_id")
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
	_, err = p.pool.Exec(ctx, `
		INSERT INTO ledger_snapshots
			(tenant_id, portfolio_id, positions, cash, accrued, through_time, updated_at)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5, now())
		ON CONFLICT (tenant_id, portfolio_id) DO UPDATE SET
			positions    = EXCLUDED.positions,
			cash         = EXCLUDED.cash,
			accrued      = EXCLUDED.accrued,
			through_time = EXCLUDED.through_time,
			updated_at   = now()
	`, snap.PortfolioID, positions, cash, accrued, snap.Through)
	if err != nil {
		return fmt.Errorf("save snapshot %s: %w", snap.PortfolioID, err)
	}
	return nil
}

// LoadSnapshot returns a portfolio's latest snapshot, or ErrNoSnapshot.
func (p *Postgres) LoadSnapshot(ctx context.Context, portfolioID string) (*Snapshot, error) {
	var (
		positions, cash, accrued []byte
		through                  time.Time
	)
	err := p.pool.QueryRow(ctx, `
		SELECT positions, cash, accrued, through_time
		FROM ledger_snapshots WHERE portfolio_id = $1
	`, portfolioID).Scan(&positions, &cash, &accrued, &through)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, fmt.Errorf("load snapshot %s: %w", portfolioID, err)
	}
	snap := &Snapshot{PortfolioID: portfolioID, Through: through}
	if snap.Positions, err = decodePositions(positions); err != nil {
		return nil, fmt.Errorf("decode positions %s: %w", portfolioID, err)
	}
	if snap.Cash, err = decodeRatMap(cash); err != nil {
		return nil, fmt.Errorf("decode cash %s: %w", portfolioID, err)
	}
	if snap.Accrued, err = decodeRatMap(accrued); err != nil {
		return nil, fmt.Errorf("decode accrued %s: %w", portfolioID, err)
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
	aj := actionJSON{Kind: int(a.Kind), Target: a.Target, Currency: a.Currency}
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
	a := &Action{Kind: CorpActKind(aj.Kind), Target: aj.Target, Currency: aj.Currency}
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
