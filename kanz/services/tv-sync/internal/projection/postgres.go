package projection

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
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
	// Seq is this fact's position in the tenant's fold order. It is the watermark a
	// checkpoint records, so a restored pod replays strictly after the last fact it
	// already holds (#809). Zero on a Fact built for Append — the database assigns
	// it — and set on every Fact a replay hands back.
	Seq int64
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
	// It also returns the seq the fact was recorded at, which is the watermark a
	// checkpoint records (#809). On a redelivery (fresh=false) the seq is zero and
	// meaningless: nothing was folded, so nothing advanced.
	Append(ctx context.Context, f Fact) (fresh bool, seq int64, err error)
	// Replay walks this tenant's facts in the order they were folded, starting
	// strictly AFTER afterSeq. Zero replays everything, which is the no-checkpoint
	// boot and the behaviour before #809.
	Replay(ctx context.Context, afterSeq int64, fn func(Fact) error) error
	// SaveCheckpoint records the fold's state at seq, replacing this tenant's
	// previous checkpoint.
	SaveCheckpoint(ctx context.Context, seq int64, payload []byte) error
	// LoadCheckpoint returns the tenant's checkpoint, or ok=false when there is
	// none — which is a normal state (a fresh deployment, a truncated table) and
	// means "replay everything", never "the account is empty".
	LoadCheckpoint(ctx context.Context) (payload []byte, seq int64, ok bool, err error)
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
func (l *PostgresLog) Append(ctx context.Context, f Fact) (bool, int64, error) {
	if f.EventID == "" {
		return false, 0, ErrFactHasNoIdentity
	}
	// RETURNING seq, so the caller can checkpoint at a fact it has actually folded
	// (#809). ON CONFLICT DO NOTHING returns NO ROW on a redelivery, which is
	// exactly the fresh=false signal this already reported — pgx.ErrNoRows is the
	// duplicate, not a failure.
	var seq int64
	err := l.pool.QueryRow(ctx, `
		INSERT INTO tv_facts (event_id, event_type, payload, knowledge_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, event_id) DO NOTHING
		RETURNING seq`,
		f.EventID, f.EventType, f.Payload, f.Knowledge).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return true, seq, nil
}

// Replay walks the log in fold order (seq), which is the order the facts were originally
// folded. The fold is order-dependent — an average-cost basis depends on which fill came
// first — so a replay that reordered them would rebuild a DIFFERENT book than the pod had.
func (l *PostgresLog) Replay(ctx context.Context, afterSeq int64, fn func(Fact) error) error {
	rows, err := l.pool.Query(ctx, `
		SELECT event_id, event_type, payload, knowledge_at, seq
		FROM tv_facts
		WHERE seq > $1
		ORDER BY seq ASC`, afterSeq)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.EventID, &f.EventType, &f.Payload, &f.Knowledge, &f.Seq); err != nil {
			return err
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return rows.Err()
}

// SaveCheckpoint replaces this tenant's fold checkpoint (#809).
//
// ONE ROW PER TENANT, UPSERTED. A history of checkpoints would be a retention
// decision of its own and answers no question the fact log cannot — the log IS the
// history, and this is only a shortcut to its end.
//
// IT IS NOT IN A TRANSACTION WITH THE FOLD, and does not need to be. A checkpoint
// is a claim about a PREFIX of an append-only log: if the process dies between
// folding fact N+1 and writing the checkpoint at N, the next boot restores N and
// replays N+1 onward, which is exactly correct. The only ordering that matters is
// that seq names a fact already durably in tv_facts, which it does — the caller
// takes it from a fact it has folded, and folding happens after Append.
func (l *PostgresLog) SaveCheckpoint(ctx context.Context, seq int64, payload []byte) error {
	_, err := l.pool.Exec(ctx, `
		INSERT INTO tv_checkpoints (seq, payload, taken_at)
		VALUES ($1, $2, now())
		ON CONFLICT (tenant_id) DO UPDATE
		SET seq = EXCLUDED.seq, payload = EXCLUDED.payload, taken_at = EXCLUDED.taken_at`,
		seq, payload)
	return err
}

// LoadCheckpoint returns this tenant's checkpoint, or ok=false when there is none.
//
// NO CHECKPOINT IS NOT AN ERROR AND NOT AN EMPTY BOOK. A fresh deployment has
// none, and so does one whose checkpoint table was truncated — both mean "replay
// everything from seq 0", which is the pre-#809 boot and reaches the identical
// view. The distinction that would be dangerous is the opposite one: treating a
// missing checkpoint as a restored empty account, which is EXEC-M21's defect.
func (l *PostgresLog) LoadCheckpoint(ctx context.Context) ([]byte, int64, bool, error) {
	var payload []byte
	var seq int64
	err := l.pool.QueryRow(ctx, `
		SELECT payload, seq FROM tv_checkpoints`).Scan(&payload, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	return payload, seq, true, nil
}
