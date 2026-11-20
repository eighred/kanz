package projection

import (
	"bytes"
	"context"
	"errors"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fact is one order-lifecycle FACT exactly as the bus delivered it.
//
// The payload is the raw wire bytes, not a parse of them: a rebuild must produce the book
// the pod actually had, and a schema that gains a field must not silently drop it from
// every replay thereafter.
type Fact struct {
	// priorFill resolves an evicted identity from earlier immutable entries.
	priorFill func(string, *orderpb.Fill) (bool, error)

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
	// already recorded. This does not establish whether this process folded it.
	// It also returns the seq the fact was recorded at, which is the watermark a
	// checkpoint records (#809). On a redelivery (fresh=false) the seq is zero and
	// meaningless. Replay after the local fold watermark resolves missing folds.
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
	db postgresQueries
	// Only the live log can begin writes. Snapshot verifiers receive db alone.
	writer *pgxpool.Pool
}

// A snapshot verifier supplies one read-only transaction; the live log supplies
// its tenant pool. Both use the same replay/checkpoint SQL.
type postgresQueries interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// NewPostgresLog wires a tenant-scoped pool.
func NewPostgresLog(pool *pgxpool.Pool) *PostgresLog {
	return &PostgresLog{db: pool, writer: pool}
}

var _ Log = (*PostgresLog)(nil)

// Append records the FACT, or reports it as already recorded.
//
// The tenant transaction lock orders allocation and commit before another append
// can proceed. A duplicate only proves durability, not that this process folded
// the fact; Handle drains the stored prefix after every successful Append.
func (l *PostgresLog) Append(ctx context.Context, f Fact) (bool, int64, error) {
	if f.EventID == "" {
		return false, 0, ErrFactHasNoIdentity
	}
	if l.writer == nil {
		return false, 0, errors.New("projection: read-only fact log")
	}
	tx, err := l.writer.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Take the tenant lock BEFORE nextval, in a separate statement. Sequence
	// allocation alone is not commit order: a later connection could otherwise
	// commit a higher sequence while an earlier insert is still unresolved.
	// Hash collisions only serialize unrelated tenants; RLS still scopes rows.
	if _, err := tx.Exec(ctx, factAppendLockSQL); err != nil {
		return false, 0, err
	}
	// RETURNING seq, so the caller can checkpoint at a fact it has actually folded
	// (#809). ON CONFLICT DO NOTHING returns NO ROW on a redelivery, which is
	// exactly the fresh=false signal this already reported — pgx.ErrNoRows is the
	// duplicate, not a failure.
	alias, key := factLookupKeys(f)
	var seq int64
	err = tx.QueryRow(ctx, `
		INSERT INTO tv_facts (event_id, event_type, payload, knowledge_at,fill_alias_digest,execution_key_digest)
		VALUES ($1, $2, $3, $4,$5,$6)
		ON CONFLICT (tenant_id, event_id) DO NOTHING
		RETURNING seq`,
		f.EventID, f.EventType, f.Payload, f.Knowledge, alias, key).Scan(&seq)
	fresh := !errors.Is(err, pgx.ErrNoRows)
	if err != nil && fresh {
		return false, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		// The outcome can be unknown. A subsequent append (including duplicate)
		// takes the same lock before the projection drains its durable prefix.
		return false, 0, err
	}
	return fresh, seq, nil
}

const factAppendLockSQL = `SELECT pg_advisory_xact_lock(hashtext('tv-sync.fact-append'), hashtext(app_current_tenant()))`

// Replay walks the log in fold order (seq), which is the order the facts were originally
// folded. The fold is order-dependent — an average-cost basis depends on which fill came
// first — so a replay that reordered them would rebuild a DIFFERENT book than the pod had.
func (l *PostgresLog) Replay(ctx context.Context, afterSeq int64, fn func(Fact) error) error {
	var high int64
	if err := l.db.QueryRow(ctx, "SELECT COALESCE(MAX(seq),0) FROM tv_facts").Scan(&high); err != nil {
		return err
	}
	for afterSeq < high {
		// Close each bounded read before consulting earlier fill rows. A replay
		// verifier may be using a single read-only repeatable-read transaction.
		rows, err := l.db.Query(ctx, `
			WITH candidates AS (
				SELECT event_id,event_type,payload,knowledge_at,seq,fill_alias_digest,execution_key_digest,
				       octet_length(payload)::bigint+octet_length(event_id)+octet_length(event_type) AS size,
				       ($3::boolean OR (octet_length(payload)<=$4 AND octet_length(event_id)<=1024 AND octet_length(event_type)<=1024)) AS allowed
				FROM tv_facts WHERE seq > $1 AND seq <= $2 ORDER BY seq LIMIT 64
			), page AS (
				SELECT *, SUM(size) OVER (ORDER BY seq) AS page_bytes,ROW_NUMBER() OVER (ORDER BY seq) AS ordinal FROM candidates
			)
			SELECT CASE WHEN allowed THEN event_id ELSE '' END,CASE WHEN allowed THEN event_type ELSE '' END,
			       CASE WHEN allowed THEN payload END,knowledge_at,seq,fill_alias_digest,execution_key_digest,allowed
			FROM page WHERE ordinal=1 OR page_bytes<=1048576 ORDER BY seq`, afterSeq, high, l.writer != nil, ReplayMaxFactBytes)
		if err != nil {
			return err
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Fact, error) {
			var f Fact
			var alias, key []byte
			var allowed bool
			err := row.Scan(&f.EventID, &f.EventType, &f.Payload, &f.Knowledge, &f.Seq, &alias, &key, &allowed)
			if err == nil && !allowed {
				return Fact{}, ErrReplayLimit
			}
			if err == nil && (alias != nil || key != nil) {
				wantAlias, wantKey := factLookupKeys(f)
				if !bytes.Equal(alias, wantAlias) || !bytes.Equal(key, wantKey) {
					return Fact{}, ErrUnresolvedFillAlias
				}
			}
			return f, err
		})
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return errors.New("projection: captured fact watermark disappeared")
		}
		for _, f := range batch {
			f.priorFill = func(portfolio string, fill *orderpb.Fill) (bool, error) {
				return l.priorExecution(ctx, f.Seq, portfolio, fill)
			}
			if err := fn(f); err != nil {
				return err
			}
			afterSeq = f.Seq
		}
	}
	return nil
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
// seq names a committed prefix, not merely an allocated BIGSERIAL. The caller
// takes it from a fact it has folded, and folding happens after Append.
func (l *PostgresLog) SaveCheckpoint(ctx context.Context, seq int64, payload []byte) error {
	_, err := l.db.Exec(ctx, `
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
	err := l.db.QueryRow(ctx, `
		SELECT payload, seq FROM tv_checkpoints`).Scan(&payload, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	return payload, seq, true, nil
}
