package projection

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Fact is one order-lifecycle FACT exactly as the bus delivered it.
//
// The payload is the raw wire bytes, not a parse of them: a rebuild must produce the book
// the pod actually had, and a schema that gains a field must not silently drop it from
// every replay thereafter.
type Fact struct {
	// EventID is the envelope's UUIDv7 — the identity that makes the fold exactly-once.
	EventID   string
	EventType string
	Payload   []byte
	// Knowledge is when the projection FIRST folded this FACT, and replay must reuse it.
	// The projection is bitemporal ("as known at T"), so stamping the replay's clock on a
	// fill from last month would move history.
	Knowledge time.Time
}

// Log is tv-sync's durable record of its own input.
//
// It holds FACTS, never a book. tv-sync is zero-truth — it derives everything it serves
// from folded FACTs and holds no independent state — so persisting orders and positions
// would make it a SECOND BOOK that could disagree with the OMS's, with nobody able to say
// which was right. A log of the events it folded cannot drift from the truth, because it
// holds no opinion about it.
type Log interface {
	// Append records one FACT. It reports whether the FACT was FRESH: false means it was
	// already recorded — a redelivery — and must not be folded a second time.
	Append(ctx context.Context, f Fact) (fresh bool, err error)
	// Replay walks this tenant's facts in the order they were folded.
	Replay(ctx context.Context, fn func(Fact) error) error
}

// ErrFactHasNoIdentity: a FACT with no event_id cannot be recorded exactly-once, so it
// cannot be folded at all. Folding it would mean a redelivery folds it AGAIN — doubling a
// position the fund does not hold — and recording it under a null identity is not possible.
//
// The bus stamps a UUIDv7 event_id on every publish and the consumer validates the envelope,
// so this is unreachable in practice. It REFUSES rather than acking, because the one thing
// that must not happen is folding it silently.
var ErrFactHasNoIdentity = errors.New("projection: FACT has no event_id — it cannot be folded exactly once")

// PostgresLog is the durable fact log. The pool is tenant-scoped (internal/pg.NewTenantPool),
// so every row lands under, and every read is confined to, that tenant — enforced at the
// engine by RLS, not by this code remembering to filter.
type PostgresLog struct {
	pool *pgxpool.Pool
}

// NewPostgresLog wires a tenant-scoped pool.
func NewPostgresLog(pool *pgxpool.Pool) *PostgresLog { return &PostgresLog{pool: pool} }

var _ Log = (*PostgresLog)(nil)

// Append records the FACT, or reports it as already recorded.
//
// ON CONFLICT DO NOTHING on (tenant_id, event_id) is the exactly-once claim: the redelivery
// that follows a crash between folding and acking inserts nothing, RowsAffected is 0, and
// the caller skips the fold. Without it a lost ack doubles a fill.
func (l *PostgresLog) Append(ctx context.Context, f Fact) (bool, error) {
	if f.EventID == "" {
		return false, ErrFactHasNoIdentity
	}
	tag, err := l.pool.Exec(ctx, `
		INSERT INTO tv_facts (event_id, event_type, payload, knowledge_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, event_id) DO NOTHING`,
		f.EventID, f.EventType, f.Payload, f.Knowledge)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Replay walks the log in fold order (seq), which is the order the facts were originally
// folded. The fold is order-dependent — an average-cost basis depends on which fill came
// first — so a replay that reordered them would rebuild a DIFFERENT book than the pod had.
func (l *PostgresLog) Replay(ctx context.Context, fn func(Fact) error) error {
	rows, err := l.pool.Query(ctx, `
		SELECT event_id, event_type, payload, knowledge_at
		FROM tv_facts
		ORDER BY seq ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.EventID, &f.EventType, &f.Payload, &f.Knowledge); err != nil {
			return err
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return rows.Err()
}
