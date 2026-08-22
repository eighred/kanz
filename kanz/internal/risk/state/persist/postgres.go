package persist

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

// Postgres is the durable StateStore backed by the 0001_state.sql schema.
// Reuses the schema-registry pgxpool pattern (EVT-16a).
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a Postgres store over an existing pool. The caller
// owns the pool lifecycle (Close), matching schema-registry.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Save persists one portfolio's committed state atomically.
//
// # Per-aggregate row locking
//
// The portfolio upsert acquires a row-level lock on that portfolio's row
// for the life of the transaction, so two concurrent Saves for the SAME
// portfolio serialize — the second blocks at the conflicting upsert until
// the first commits. This is the durable mirror of the in-memory
// per-portfolio mutex (RISK-05). Different portfolios never contend.
// Read-committed (the pool default) is sufficient: the row lock provides
// the per-aggregate ordering, and there is no cross-aggregate invariant
// to protect (unlike the registry's Serializable version assignment).
//
// # Full replace
//
// Positions and applied keys are deleted and re-inserted from the record,
// matching the snapshot hard-reset semantics (event-class-rules §3): a
// record is the whole truth for that portfolio, so a position dropped from
// rec.Positions is removed from the store. AppliedKeys is the bounded tail
// the writer (PERS-01c) computed; persisting exactly it keeps the durable
// dedup window the same shape as the in-memory one.
func (p *Postgres) Save(ctx context.Context, rec PortfolioRecord) error {
	if rec.ID == "" {
		return errors.New("persist: save with empty portfolio id")
	}
	cash, err := encode(rec.CashBalance)
	if err != nil {
		return fmt.Errorf("encode cash_balance: %w", err)
	}
	tmv, err := encode(rec.TotalMarketValue)
	if err != nil {
		return fmt.Errorf("encode total_market_value: %w", err)
	}
	topic, partition, offset := decomposeLogPosition(rec.LogPosition)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO portfolios
			(tenant_id, portfolio_id, display_name, base_currency, cash_balance,
			 total_market_value, position_count, as_of,
			 log_topic, log_partition, log_offset, updated_at)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
		ON CONFLICT (tenant_id, portfolio_id) DO UPDATE SET
			display_name       = EXCLUDED.display_name,
			base_currency      = EXCLUDED.base_currency,
			cash_balance       = EXCLUDED.cash_balance,
			total_market_value = EXCLUDED.total_market_value,
			position_count     = EXCLUDED.position_count,
			as_of              = EXCLUDED.as_of,
			log_topic          = EXCLUDED.log_topic,
			log_partition      = EXCLUDED.log_partition,
			log_offset         = EXCLUDED.log_offset,
			updated_at         = now()
	`, string(rec.ID), rec.DisplayName, string(rec.BaseCurrency), cash,
		tmv, int64(rec.PositionCount), nullTime(rec.AsOf),
		topic, partition, offset); err != nil {
		return fmt.Errorf("upsert portfolio %s: %w", rec.ID, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM positions WHERE portfolio_id = $1`, string(rec.ID)); err != nil {
		return fmt.Errorf("clear positions %s: %w", rec.ID, err)
	}
	for _, pos := range rec.Positions {
		if err := insertPosition(ctx, tx, rec.ID, pos); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM applied_keys WHERE portfolio_id = $1`, string(rec.ID)); err != nil {
		return fmt.Errorf("clear applied_keys %s: %w", rec.ID, err)
	}
	for _, key := range rec.AppliedKeys {
		if _, err := tx.Exec(ctx, `
			INSERT INTO applied_keys (tenant_id, portfolio_id, idempotency_key)
			VALUES (current_setting('app.tenant_id'), $1, $2)
			ON CONFLICT DO NOTHING
		`, string(rec.ID), key); err != nil {
			return fmt.Errorf("insert applied_key %s: %w", rec.ID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit save %s: %w", rec.ID, err)
	}
	return nil
}

func insertPosition(ctx context.Context, tx pgx.Tx, id v1.PortfolioID, pos domain.Position) error {
	cols := []struct {
		name string
		msg  proto.Message
	}{
		{"quantity", pos.Quantity},
		{"average_price", pos.AveragePrice},
		{"market_value", pos.MarketValue},
		{"market_value_uncertainty", pos.MarketValueUncertainty},
		{"realized_pnl", pos.RealizedPnL},
		{"unrealized_pnl", pos.UnrealizedPnL},
	}
	vals := make([][]byte, len(cols))
	for i, c := range cols {
		b, err := encode(c.msg)
		if err != nil {
			return fmt.Errorf("encode %s for %s/%s: %w", c.name, id, pos.InstrumentID, err)
		}
		vals[i] = b
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO positions
			(tenant_id, portfolio_id, instrument_id, quantity, average_price, market_value,
			 market_value_uncertainty, realized_pnl, unrealized_pnl, as_of)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, string(id), string(pos.InstrumentID),
		vals[0], vals[1], vals[2], vals[3], vals[4], vals[5], nullTime(pos.AsOf)); err != nil {
		return fmt.Errorf("insert position %s/%s: %w", id, pos.InstrumentID, err)
	}
	return nil
}

// Load returns the latest persisted record for one portfolio, or
// ErrNotFound. Three reads (portfolio row, positions, applied keys); the
// portfolio row is read first so a missing portfolio short-circuits.
func (p *Postgres) Load(ctx context.Context, id v1.PortfolioID) (PortfolioRecord, error) {
	rec, err := p.scanPortfolio(ctx, id)
	if err != nil {
		return PortfolioRecord{}, err
	}
	rec.Positions, err = p.loadPositions(ctx, id)
	if err != nil {
		return PortfolioRecord{}, err
	}
	rec.AppliedKeys, err = p.loadAppliedKeys(ctx, id)
	if err != nil {
		return PortfolioRecord{}, err
	}
	return rec, nil
}

func (p *Postgres) scanPortfolio(ctx context.Context, id v1.PortfolioID) (PortfolioRecord, error) {
	var (
		rec       PortfolioRecord
		baseCur   string
		cash, tmv []byte
		posCount  int64
		asOf      *time.Time
		topic     *string
		partition *int64
		offset    *int64
	)
	err := p.pool.QueryRow(ctx, `
		SELECT tenant_id, portfolio_id, display_name, base_currency, cash_balance,
		       total_market_value, position_count, as_of,
		       log_topic, log_partition, log_offset
		FROM portfolios WHERE portfolio_id = $1
	`, string(id)).Scan(
		&rec.TenantID, (*string)(&rec.ID), &rec.DisplayName, &baseCur, &cash,
		&tmv, &posCount, &asOf, &topic, &partition, &offset,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortfolioRecord{}, ErrNotFound
	}
	if err != nil {
		return PortfolioRecord{}, fmt.Errorf("load portfolio %s: %w", id, err)
	}
	rec.BaseCurrency = domain.CurrencyCode(baseCur)
	rec.PositionCount = uint32(posCount)
	rec.AsOf = derefTime(asOf)
	if rec.CashBalance, err = decodeMoney(cash); err != nil {
		return PortfolioRecord{}, fmt.Errorf("decode cash_balance %s: %w", id, err)
	}
	if rec.TotalMarketValue, err = decodeMoney(tmv); err != nil {
		return PortfolioRecord{}, fmt.Errorf("decode total_market_value %s: %w", id, err)
	}
	rec.LogPosition = composeLogPosition(topic, partition, offset)
	return rec, nil
}

func (p *Postgres) loadPositions(ctx context.Context, id v1.PortfolioID) ([]domain.Position, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT instrument_id, quantity, average_price, market_value,
		       market_value_uncertainty, realized_pnl, unrealized_pnl, as_of
		FROM positions WHERE portfolio_id = $1 ORDER BY instrument_id
	`, string(id))
	if err != nil {
		return nil, fmt.Errorf("query positions %s: %w", id, err)
	}
	defer rows.Close()

	var out []domain.Position
	for rows.Next() {
		var (
			instrument                          string
			qty, avg, mv, mvu, realized, unreal []byte
			asOf                                *time.Time
		)
		if err := rows.Scan(&instrument, &qty, &avg, &mv, &mvu, &realized, &unreal, &asOf); err != nil {
			return nil, fmt.Errorf("scan position %s: %w", id, err)
		}
		pos := domain.Position{InstrumentID: domain.InstrumentID(instrument), AsOf: derefTime(asOf)}
		if pos.Quantity, err = decodeDecimal(qty); err != nil {
			return nil, fmt.Errorf("decode quantity %s/%s: %w", id, instrument, err)
		}
		if pos.AveragePrice, err = decodeDecimal(avg); err != nil {
			return nil, fmt.Errorf("decode average_price %s/%s: %w", id, instrument, err)
		}
		if pos.MarketValue, err = decodeMoney(mv); err != nil {
			return nil, fmt.Errorf("decode market_value %s/%s: %w", id, instrument, err)
		}
		if pos.MarketValueUncertainty, err = decodeMoney(mvu); err != nil {
			return nil, fmt.Errorf("decode market_value_uncertainty %s/%s: %w", id, instrument, err)
		}
		if pos.RealizedPnL, err = decodeMoney(realized); err != nil {
			return nil, fmt.Errorf("decode realized_pnl %s/%s: %w", id, instrument, err)
		}
		if pos.UnrealizedPnL, err = decodeMoney(unreal); err != nil {
			return nil, fmt.Errorf("decode unrealized_pnl %s/%s: %w", id, instrument, err)
		}
		out = append(out, pos)
	}
	return out, rows.Err()
}

func (p *Postgres) loadAppliedKeys(ctx context.Context, id v1.PortfolioID) ([]string, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT idempotency_key FROM applied_keys
		WHERE portfolio_id = $1 ORDER BY applied_at, idempotency_key
	`, string(id))
	if err != nil {
		return nil, fmt.Errorf("query applied_keys %s: %w", id, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan applied_key %s: %w", id, err)
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// loadPageSize is how many portfolios one bootstrap page carries.
//
// IT TRADES ROUND TRIPS AGAINST PEAK MEMORY, and the trade is worth stating
// because this is the knob somebody will reach for.
//
// Peak is pageSize x record size, and a record is dominated by its positions. At
// 256 a page is a few megabytes even for a book carrying a hundred positions per
// portfolio, which is small enough to be uninteresting at boot. Round trips are
// 3 per page plus one for the empty page that ends the walk: 4 queries for an
// estate under 256 portfolios (against 3 before), and roughly 1,200 for 100,000
// portfolios — one or two seconds, once, on the slowest path this process has.
//
// Raising it to 1024 would cut those round trips fourfold and take peak into the
// tens of megabytes on a position-heavy book. That is the wrong direction for a
// recovery path: the seconds are paid once per boot and are visible, while the
// memory is what turned recovery time into a function of estate size in the first
// place (#674).
//
// It is a CONST. The page size must not become a runtime-mutable knob —
// paginateRecords takes it as a parameter so tests can page at 1 without one,
// which is the shape that keeps a test-only var off a production struct.
const loadPageSize = 256

// LoadEach streams every persisted record to fn, one at a time — the bootstrap
// read (PERS-01d).
//
// IT REPLACED LoadAll, WHICH RETURNED THE WHOLE ESTATE AS ONE SLICE (#674). That
// read `FROM positions` and `FROM applied_keys` with no WHERE and no LIMIT, so
// startup time and peak memory both scaled with total estate size — recovery
// time as a function of how successful the platform is. The signature change is
// the point: a caller can no longer receive every record at once, so the defect
// cannot be reintroduced by a caller that means well.
//
// Each PAGE still costs three queries rather than N+1 per portfolio, which is
// what the original grouped-scan shape was protecting and is worth keeping.
func (p *Postgres) LoadEach(ctx context.Context, fn func(PortfolioRecord) error) error {
	return paginateRecords(ctx, p.loadPage, loadPageSize, fn)
}

// loadPage returns up to limit records whose portfolio_id sorts after `after`,
// with their positions and applied keys attached.
func (p *Postgres) loadPage(ctx context.Context, after v1.PortfolioID, limit int) ([]PortfolioRecord, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT tenant_id, portfolio_id, display_name, base_currency, cash_balance,
		       total_market_value, position_count, as_of,
		       log_topic, log_partition, log_offset
		FROM portfolios
		WHERE portfolio_id > $1
		ORDER BY portfolio_id
		LIMIT $2
	`, string(after), limit)
	if err != nil {
		return nil, fmt.Errorf("query portfolios: %w", err)
	}
	byID := make(map[v1.PortfolioID]*PortfolioRecord)
	var order []v1.PortfolioID
	for rows.Next() {
		var (
			rec       PortfolioRecord
			baseCur   string
			cash, tmv []byte
			posCount  int64
			asOf      *time.Time
			topic     *string
			partition *int64
			offset    *int64
		)
		if err := rows.Scan(
			&rec.TenantID, (*string)(&rec.ID), &rec.DisplayName, &baseCur, &cash,
			&tmv, &posCount, &asOf, &topic, &partition, &offset,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan portfolio: %w", err)
		}
		rec.BaseCurrency = domain.CurrencyCode(baseCur)
		rec.PositionCount = uint32(posCount)
		rec.AsOf = derefTime(asOf)
		if rec.CashBalance, err = decodeMoney(cash); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode cash_balance %s: %w", rec.ID, err)
		}
		if rec.TotalMarketValue, err = decodeMoney(tmv); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode total_market_value %s: %w", rec.ID, err)
		}
		rec.LogPosition = composeLogPosition(topic, partition, offset)
		byID[rec.ID] = &rec
		order = append(order, rec.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate portfolios: %w", err)
	}
	rows.Close()

	if len(order) == 0 {
		return nil, nil
	}
	// The page's ids, passed to both child reads so each is bounded by the page
	// rather than by the estate.
	pageIDs := make([]string, 0, len(order))
	for _, id := range order {
		pageIDs = append(pageIDs, string(id))
	}
	if err := p.scanPositions(ctx, pageIDs, byID); err != nil {
		return nil, err
	}
	if err := p.scanAppliedKeys(ctx, pageIDs, byID); err != nil {
		return nil, err
	}

	out := make([]PortfolioRecord, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// scanPositions reads the positions of ONE PAGE of portfolios.
//
// The `= ANY($1)` is what bounds it. It used to be an unbounded `FROM positions`
// scan, which on a sharded replica decoded every other replica's positions too,
// only for bootstrap to discard them as ErrNotOwned (#674).
func (p *Postgres) scanPositions(ctx context.Context, pageIDs []string, byID map[v1.PortfolioID]*PortfolioRecord) error {
	rows, err := p.pool.Query(ctx, `
		SELECT portfolio_id, instrument_id, quantity, average_price, market_value,
		       market_value_uncertainty, realized_pnl, unrealized_pnl, as_of
		FROM positions
		WHERE portfolio_id = ANY($1)
		ORDER BY portfolio_id, instrument_id
	`, pageIDs)
	if err != nil {
		return fmt.Errorf("query positions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			pid, instrument                     string
			qty, avg, mv, mvu, realized, unreal []byte
			asOf                                *time.Time
		)
		if err := rows.Scan(&pid, &instrument, &qty, &avg, &mv, &mvu, &realized, &unreal, &asOf); err != nil {
			return fmt.Errorf("scan position: %w", err)
		}
		rec, ok := byID[v1.PortfolioID(pid)]
		if !ok {
			continue // orphan guarded by FK; defensive
		}
		pos := domain.Position{InstrumentID: domain.InstrumentID(instrument), AsOf: derefTime(asOf)}
		if pos.Quantity, err = decodeDecimal(qty); err != nil {
			return fmt.Errorf("decode quantity %s/%s: %w", pid, instrument, err)
		}
		if pos.AveragePrice, err = decodeDecimal(avg); err != nil {
			return fmt.Errorf("decode average_price %s/%s: %w", pid, instrument, err)
		}
		if pos.MarketValue, err = decodeMoney(mv); err != nil {
			return fmt.Errorf("decode market_value %s/%s: %w", pid, instrument, err)
		}
		if pos.MarketValueUncertainty, err = decodeMoney(mvu); err != nil {
			return fmt.Errorf("decode market_value_uncertainty %s/%s: %w", pid, instrument, err)
		}
		if pos.RealizedPnL, err = decodeMoney(realized); err != nil {
			return fmt.Errorf("decode realized_pnl %s/%s: %w", pid, instrument, err)
		}
		if pos.UnrealizedPnL, err = decodeMoney(unreal); err != nil {
			return fmt.Errorf("decode unrealized_pnl %s/%s: %w", pid, instrument, err)
		}
		rec.Positions = append(rec.Positions, pos)
	}
	return rows.Err()
}

// scanAppliedKeys reads the applied keys of ONE PAGE of portfolios. Bounded for
// scanPositions' reason (#674).
func (p *Postgres) scanAppliedKeys(ctx context.Context, pageIDs []string, byID map[v1.PortfolioID]*PortfolioRecord) error {
	rows, err := p.pool.Query(ctx, `
		SELECT portfolio_id, idempotency_key FROM applied_keys
		WHERE portfolio_id = ANY($1)
		ORDER BY portfolio_id, applied_at, idempotency_key
	`, pageIDs)
	if err != nil {
		return fmt.Errorf("query applied_keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pid, key string
		if err := rows.Scan(&pid, &key); err != nil {
			return fmt.Errorf("scan applied_key: %w", err)
		}
		if rec, ok := byID[v1.PortfolioID(pid)]; ok {
			rec.AppliedKeys = append(rec.AppliedKeys, key)
		}
	}
	return rows.Err()
}

// Ping checks store reachability for the readiness probe.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// --- value-type encoding ----------------------------------------------

// encode marshals a common.v1 value message to bytes, returning nil
// (⇒ SQL NULL) for a nil message. The reflect.IsNil guard handles the
// typed-nil-in-interface trap: a nil *commonpb.Money passed as a
// proto.Message is a non-nil interface wrapping a nil pointer, so the
// plain m == nil check is insufficient.
func encode(m proto.Message) ([]byte, error) {
	if m == nil || reflect.ValueOf(m).IsNil() {
		return nil, nil
	}
	return proto.Marshal(m)
}

func decodeMoney(b []byte) (*commonpb.Money, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m commonpb.Money
	if err := proto.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func decodeDecimal(b []byte) (*commonpb.Decimal, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var d commonpb.Decimal
	if err := proto.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// decomposeLogPosition splits a LogPosition into nullable column values;
// all three are nil when lp is nil (pre-snapshot state).
func decomposeLogPosition(lp *commonpb.LogPosition) (topic any, partition any, offset any) {
	if lp == nil {
		return nil, nil, nil
	}
	return lp.Topic, int64(lp.Partition), int64(lp.Offset)
}

// composeLogPosition rebuilds a LogPosition from nullable columns, or nil
// when unset. Topic presence is the discriminator — the three columns are
// written and cleared together.
func composeLogPosition(topic *string, partition, offset *int64) *commonpb.LogPosition {
	if topic == nil {
		return nil
	}
	lp := &commonpb.LogPosition{Topic: *topic}
	if partition != nil {
		lp.Partition = uint32(*partition)
	}
	if offset != nil {
		lp.Offset = uint64(*offset)
	}
	return lp
}

// nullTime maps the Go zero time to SQL NULL so "no state applied yet"
// round-trips as NULL rather than the proto epoch.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// Compile-time assertion that Postgres satisfies StateStore.
var _ StateStore = (*Postgres)(nil)
