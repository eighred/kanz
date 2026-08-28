package order

import (
	"context"
	"errors"
	"fmt"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/outbox"
)

// Postgres is the durable Store backed by the 0001_orders.sql schema (EXEC-M7c).
// It is the production backend; MemoryStore stays the test seam.
//
// It exists to move the admission gate out of the process. MemoryStore.Create is
// atomic under a sync.Mutex, which makes it exactly-once within ONE replica and
// nothing across two — so a multi-pod OMS split the map, both pods admitted the
// same order_id, and both routed it to the venue. Here the guarantee is the
// PRIMARY KEY (tenant_id, order_id): the storage engine arbitrates, so it holds
// across every replica and the deployment no longer needs a single-replica pin.
//
// State is stored as marshaled order.v1.OrderState bytes — OrderState carries
// exact base-10 Money/Decimal and `double` is banned on a capital path, so there
// is no lossless native column (the risk-engine PERS-01 opaque-bytes stance).
type Postgres struct {
	pool *pgxpool.Pool
	// queue is the outbox over the SAME pool, which is what gives it the same
	// tenant scope and the same failure domain as the orders it announces.
	queue *outbox.Postgres
	// proposals is the durable home of orders held for a second signature
	// (#410), over the SAME pool and the SAME outbox — one tenant scope, one
	// failure domain, and a held order announced through the same relay that
	// announces an admitted one.
	proposals *PostgresProposals
}

// NewPostgres returns a Postgres store over an existing pool. The caller owns
// the pool lifecycle (Close).
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	q := outbox.NewPostgres(pool, "oms")
	return &Postgres{pool: pool, queue: q, proposals: NewPostgresProposals(pool, q)}
}

// Outbox is the durable queue Create enqueues into. See Store.Outbox for why the
// store hands it out rather than the composition root building one of its own.
func (p *Postgres) Outbox() outbox.Queue { return p.queue }

// Proposals is the durable home of orders held for a second signature. See
// Store.Proposals for why it is handed out through the store.
func (p *Postgres) Proposals() ProposalStore { return p.proposals }

// Create is the ATOMIC ADMISSION GATE, and since #292 it is also the point at
// which the order's announcement becomes as durable as the order.
//
// The gate is still one statement — an INSERT that the engine either applies or
// discards on conflict — and the RowsAffected it reports is still the verdict:
// 1 ⇒ this delivery won and owns the order; 0 ⇒ another delivery already created
// it, so this one lost and MUST NOT route to the venue (ErrExists is that
// signal).
//
// Never rewrite this as a SELECT followed by an INSERT. The check-then-act
// window between them is exactly the double-trade bug this store was built to
// close, and it is invisible in tests because Save is an upsert: the state still
// converges while the fund trades twice.
//
// # WHY THERE IS NOW A TRANSACTION AROUND ONE STATEMENT
//
// Because there are two. The outbox records ride the SAME transaction as the
// order row, which is the entire point of #292: admission used to be
// store.Create and then emitter.EmitAccepted, two independent writes, so a
// publish failure left a durable PENDING_NEW order that no downstream service
// had ever heard of. Risk carried no exposure for it, tv-sync never admitted it
// and therefore also dropped the ORDER_ROUTED a later re-drive emitted. #238
// added a marker and a sweep to find those orders afterwards. This makes them
// not exist: the estate's copy of the announcement is committed by the same
// COMMIT that admits the order.
//
// THE LOSER OF THE ADMISSION RACE ENQUEUES NOTHING. The ErrExists path rolls
// back, so a duplicate delivery cannot leave a second ACCEPTED record behind for
// the relay to publish. That is what the deferred Rollback is for, and it is not
// decoration — without it a lost race would announce the order twice.
//
// # WHY THE TRANSACTION IS NOT AN ISOLATION CHANGE
//
// It runs at the pool's default READ COMMITTED. Nothing here reads before it
// writes, so there is no snapshot to protect; the transaction exists purely to
// make the two INSERTs one durable unit. Raising the isolation level would buy
// nothing and would introduce 40001 serialization failures on the admission
// path, where MaxAttempts is 1 and there is nobody to retry them.
func (p *Postgres) Create(ctx context.Context, st *orderpb.OrderState, announce []outbox.Record) error {
	if st.GetOrderId() == "" {
		return errors.New("oms: cannot create order with empty order_id")
	}
	blob, err := proto.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal order %s: %w", st.GetOrderId(), err)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("create order %s: begin: %w", st.GetOrderId(), err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

	// portfolio_id is denormalized out of the blob (#399), like order_id and
	// status before it, and parent_order_id joins them (#435). The blob stays
	// authoritative; these are index keys.
	tag, err := tx.Exec(ctx, `
		INSERT INTO orders (tenant_id, order_id, status, state, portfolio_id, parent_order_id)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, order_id) DO NOTHING
	`, st.GetOrderId(), int32(st.GetStatus()), blob, st.GetPortfolioId(), st.GetParentOrderId())
	if err != nil {
		return fmt.Errorf("create order %s: %w", st.GetOrderId(), err)
	}
	if tag.RowsAffected() == 0 {
		return ErrExists // the deferred Rollback discards the announcement with it
	}
	if err := outbox.Enqueue(ctx, tx, announce...); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("create order %s: commit: %w", st.GetOrderId(), err)
	}
	return nil
}

// Save is the post-admission compare-and-swap: every state transition after
// Create, applied only if the order is still at the version the caller loaded.
//
// THE PREDICATE IS THE WHOLE POINT, AND RowsAffected IS THE VERDICT — the same
// shape Create uses two functions above. 1 ⇒ this writer won and the version
// advanced; 0 ⇒ somebody else moved the order since this caller read it, and
// applying this write would DISCARD their transition, so it is refused with
// ErrConflict.
//
// This used to be a blind upsert, justified by the claim that "the bus
// partition_key serializes transitions per order_id". THAT WAS NEVER TRUE.
// partition_key is stamped by producers (pkg/bus/producer.go) and read by the
// consumer only to copy onto a DLQ republish (pkg/bus/consumer.go); nothing
// anywhere serializes deliveries by it, and submit, amend and cancel arrive on
// three separate durables with three cursors and three dispatch goroutines
// (pkg/bus/nats.go). The consumer's only exclusion is its dedup claim, keyed on
// idempotency_key — which differs between a submit and a cancel. Believing that
// sentence is how a cancel came to be overwritten by a fill that never saw it.
// Do not reintroduce a serialization claim here.
//
// The service's per-order lock (internal/order/orderlock.go) excludes writers
// within one process. It cannot reach across pods by construction, and
// oms-deploy.yaml runs replicas: 2 — so the version is what makes a multi-replica
// OMS safe, exactly as the ON CONFLICT DO NOTHING in Create is what makes
// admission safe (#122).
//
// The INSERT arm still exists because Save must remain callable for an order
// this process created; on the insert path the row lands at version 0 and no
// predicate applies, because there is nothing yet to conflict with.
//
// # WHY A TRANSACTION ONLY WHEN THERE IS SOMETHING TO ANNOUNCE
//
// announce rides the SAME transaction as the state change, which is the whole of
// #292 applied to the fill fold: work() and adopt() hand this the ORDER_FILLED /
// ORDER_PARTIALLY_FILLED FACT with the state that records it, so a crash or a
// broker refusal between them is not reachable. A fill is the one FACT no
// compensator can rebuild — completeTerminalOutcome says so in its own comment —
// so "the write landed and the FACT did not" had no recovery at all.
//
// Most Saves announce nothing (a marker stamp, a quarantine freeze, the venue
// ack), and wrapping those in BEGIN/COMMIT would add two round trips per
// transition on the capital path to protect an empty set. So the announce-less
// path stays a single statement — but THE STATEMENT AND ITS VERDICT ARE SHARED,
// not copied. saveSQL exists once and cas() interprets RowsAffected once; two
// copies of a compare-and-swap is how one of them quietly stops refusing.
//
// THE CAS PREDICATE IS UNCHANGED BY ANY OF THIS (#122). Same SQL, same
// `WHERE orders.version = $4`, same 0-rows-means-ErrConflict verdict, whether it
// runs on the pool or inside the transaction — and a refused CAS returns before
// the enqueue, so a loser announces nothing. cas_test.go pins it.
func (p *Postgres) Save(ctx context.Context, st *orderpb.OrderState, expectedVersion int64, announce []outbox.Record, fillID string) error {
	if st.GetOrderId() == "" {
		return errors.New("oms: cannot save order with empty order_id")
	}
	blob, err := proto.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal order %s: %w", st.GetOrderId(), err)
	}
	// THE POOL FAST PATH IS ONLY AVAILABLE WHEN THERE IS NOTHING TO COMMIT
	// ALONGSIDE. A fill claim needs the transaction for the same reason the
	// outbox record does: a claim that commits apart from the fold loses the fill
	// if the fold then fails, and a fold that commits apart from its claim
	// double-counts on the next redelivery (#782).
	if len(announce) == 0 && fillID == "" {
		return p.cas(ctx, p.pool, st, blob, expectedVersion)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("save order %s: begin: %w", st.GetOrderId(), err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit
	// THE CLAIM FIRST OF ALL (#782). A fill this order already contains means
	// this write must not land at all — not the state, not the FACT — and
	// claiming before the CAS means the rollback has nothing to undo rather than
	// something to be trusted to undo. It also puts the cheapest refusal first.
	if fillID != "" {
		claimed, cerr := p.claimFill(ctx, tx, st.GetOrderId(), fillID)
		if cerr != nil {
			return cerr
		}
		if !claimed {
			return ErrFillApplied
		}
	}
	// THE CAS SECOND, THE ANNOUNCEMENT THIRD. A conflict means another writer
	// moved the order, so this delivery's FACT describes a transition that never
	// happened — returning before the enqueue keeps it out of the table rather
	// than relying on the rollback to take it out again.
	if err := p.cas(ctx, tx, st, blob, expectedVersion); err != nil {
		return err
	}
	if err := outbox.Enqueue(ctx, tx, announce...); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("save order %s: commit: %w", st.GetOrderId(), err)
	}
	return nil
}

// claimFillSQL is 0003's stance applied to the order aggregate: the insert IS
// the claim, and RowsAffected is the verdict. A SELECT-then-INSERT would
// reintroduce the check-then-act window two replicas reconciling the same order
// would drive straight through.
const claimFillSQL = `
	INSERT INTO order_fills (tenant_id, order_id, fill_id)
	VALUES (current_setting('app.tenant_id'), $1, $2)
	ON CONFLICT (tenant_id, fill_id) DO NOTHING
`

// claimFill reports whether THIS write owns the fill: 1 row ⇒ it is new and the
// fold may proceed, 0 ⇒ the aggregate already contains it.
//
// THE CONFLICT TARGET IS (tenant_id, fill_id) AND NOT order_id, deliberately. A
// venue fill belongs to exactly one order; if the same fill_id ever arrived
// against a SECOND order that would be a venue or routing defect, and silently
// folding it into both aggregates is the worst available outcome. Keyed this
// way the second order's fold is refused and the operator gets an order that
// will not advance, which is the failure that gets looked at.
func (p *Postgres) claimFill(ctx context.Context, db execer, orderID, fillID string) (bool, error) {
	tag, err := db.Exec(ctx, claimFillSQL, orderID, fillID)
	if err != nil {
		return false, fmt.Errorf("claim fill %s for order %s: %w", fillID, orderID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// saveSQL is the compare-and-swap, written ONCE. Both Save paths execute this
// exact text; a second copy is how the pool path and the transaction path would
// come to disagree about what a conflict is.
const saveSQL = `
	INSERT INTO orders (tenant_id, order_id, status, state, portfolio_id, parent_order_id)
	VALUES (current_setting('app.tenant_id'), $1, $2, $3, $5, $6)
	ON CONFLICT (tenant_id, order_id) DO UPDATE SET
		status          = EXCLUDED.status,
		state           = EXCLUDED.state,
		portfolio_id    = EXCLUDED.portfolio_id,
		parent_order_id = EXCLUDED.parent_order_id,
		version         = orders.version + 1,
		updated_at      = now()
	WHERE orders.version = $4
`

// execer is everything the compare-and-swap needs from its connection, and it is
// satisfied by both *pgxpool.Pool and pgx.Tx. It exists so the announce-less
// single statement and the announcing transaction run the SAME code, not the
// same-looking code.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// cas runs saveSQL and turns RowsAffected into the verdict: 1 ⇒ this writer won
// and the version advanced, 0 ⇒ somebody else moved the order since this caller
// read it and applying this write would discard their transition.
func (p *Postgres) cas(ctx context.Context, db execer, st *orderpb.OrderState, blob []byte, expectedVersion int64) error {
	// EVERY SAVE REWRITES portfolio_id AND parent_order_id, which is what backfills
	// a pre-0007 or pre-0008 order the moment anything touches it: the columns are
	// derived from the blob, and the blob is in hand here. They cannot drift,
	// because there is no path that writes state without writing these.
	tag, err := db.Exec(ctx, saveSQL,
		st.GetOrderId(), int32(st.GetStatus()), blob, expectedVersion,
		st.GetPortfolioId(), st.GetParentOrderId())
	if err != nil {
		return fmt.Errorf("save order %s: %w", st.GetOrderId(), err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// Load returns the current state of one order and its version, or ErrNotFound.
//
// The state and the version come from ONE row read, so they cannot disagree.
// Reading them in two queries would hand the caller a version that had already
// moved, and its next Save would be refused for no reason a reader could see.
func (p *Postgres) Load(ctx context.Context, orderID string) (*orderpb.OrderState, int64, error) {
	var (
		blob    []byte
		version int64
	)
	err := p.pool.QueryRow(ctx,
		`SELECT state, version FROM orders WHERE order_id = $1`, orderID,
	).Scan(&blob, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("load order %s: %w", orderID, err)
	}
	st, err := unmarshalState(blob, orderID)
	if err != nil {
		return nil, 0, err
	}
	return st, version, nil
}

// List returns a snapshot of all known orders, for bootstrap/inspection.
func (p *Postgres) List(ctx context.Context) ([]*orderpb.OrderState, error) {
	rows, err := p.pool.Query(ctx, `SELECT order_id, state FROM orders ORDER BY order_id`)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	defer rows.Close()

	var out []*orderpb.OrderState
	for rows.Next() {
		var (
			id   string
			blob []byte
		)
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		st, err := unmarshalState(blob, id)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ListByPortfolio selects one portfolio's orders, newest first.
//
// It reads the DENORMALIZED portfolio_id column (migration 0007), which
// orders_portfolio_idx covers together with created_at DESC — so a portfolio
// with a long history returns its newest page from an index scan rather than
// sorting the whole set. Before that column existed this query was a full scan
// plus a proto decode per row, which is why it did not exist.
//
// THE SECOND RETURN IS NOT A DIAGNOSTIC. Orders admitted before 0007 carry an
// empty portfolio_id — no SQL can decode the blob that holds their real one — so
// they cannot appear in the result above however it is written. Reporting the
// list alone would present a partial history as a complete one. The count is
// taken in the SAME query, so it describes the same snapshot as the rows.
//
// Tenant scoping is NOT applied here and must not be: RLS is FORCEd on this
// table and the policy filters on current_setting('app.tenant_id'), exactly as
// it does for List, Load and ListByStatus.
func (p *Postgres) ListByPortfolio(ctx context.Context, portfolioID string, limit int) ([]*orderpb.OrderState, int64, error) {
	if portfolioID == "" {
		// The empty id is 0007's NOT-INDEXED marker, never a portfolio. Querying
		// for it would return precisely the orders whose portfolio is unknown, as
		// if they belonged to a portfolio named "". Refuse rather than answer.
		return nil, 0, nil
	}
	if limit <= 0 || limit > maxOrderPage {
		limit = maxOrderPage
	}
	rows, err := p.pool.Query(ctx, `
		SELECT state FROM orders
		 WHERE portfolio_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2
	`, portfolioID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list orders for portfolio: %w", err)
	}
	defer rows.Close()

	var out []*orderpb.OrderState
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, 0, fmt.Errorf("scan order: %w", err)
		}
		var st orderpb.OrderState
		if err := proto.Unmarshal(blob, &st); err != nil {
			return nil, 0, fmt.Errorf("unmarshal order state: %w", err)
		}
		out = append(out, &st)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list orders for portfolio: %w", err)
	}

	// TWO QUERIES, NOT ONE CLEVER ONE. Counting the unindexed rows in the same
	// statement — a windowed FILTER over a widened WHERE — makes the planner sort
	// every pre-0007 order in the tenant to return one page of a portfolio's
	// history, and produces SQL whose correctness a reader has to work out. Both
	// of these are index scans on orders_portfolio_idx; the second is a count
	// over one key.
	var unindexed int64
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM orders WHERE portfolio_id = ''`).Scan(&unindexed); err != nil {
		return nil, 0, fmt.Errorf("count unindexed orders: %w", err)
	}
	return out, unindexed, nil
}

// maxOrderPage bounds one page of order history.
//
// A page is a network payload of marshaled OrderStates, and an unbounded LIMIT
// on a fund's whole history is a way for one request to exhaust the gateway's
// memory rather than a feature. The number is a page size, not a policy about
// how much history exists.
const maxOrderPage = 200

// ListByStatus selects the orders currently in one of the given statuses.
//
// It reads the DENORMALIZED status column, which is what orders_status_idx
// covers, so the startup sweep costs one index scan over the open orders rather
// than a full scan over every order the fund has ever placed. The authoritative
// state is still the marshaled proto in `state` — status is the index key, not
// the truth, and the two are written in the same statement so they cannot drift.
//
// Tenant scoping is NOT applied here and must not be: RLS is FORCEd on this
// table (migrations/0001_orders.sql) and the policy filters on
// current_setting('app.tenant_id'), exactly as it does for List and Load.
func (p *Postgres) ListByStatus(ctx context.Context, statuses ...orderpb.OrderStatus) ([]*orderpb.OrderState, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	codes := make([]int32, 0, len(statuses))
	for _, s := range statuses {
		codes = append(codes, int32(s))
	}
	rows, err := p.pool.Query(ctx,
		`SELECT order_id, state FROM orders WHERE status = ANY($1) ORDER BY order_id`, codes)
	if err != nil {
		return nil, fmt.Errorf("list orders by status: %w", err)
	}
	defer rows.Close()

	var out []*orderpb.OrderState
	for rows.Next() {
		var (
			id   string
			blob []byte
		)
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		st, err := unmarshalState(blob, id)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ListByParent returns the children of one working parent order (#435).
//
// THE `parent_order_id <> ”` CLAUSE IS LOAD-BEARING, NOT REDUNDANT. It is
// implied by the equality above it for any non-empty parameter, and a reader
// tidying it away would be right about the logic and wrong about the plan:
// orders_parent_idx is a PARTIAL index carrying that predicate
// (migrations/0008_orders_parent.sql), and Postgres will only choose a partial
// index when it can PROVE the predicate holds. It cannot prove anything about a
// parameter it has not seen, so without this clause every driver tick becomes a
// sequential scan of the entire order book.
//
// The index is partial because almost every order on a real book has no parent;
// a plain index would carry one entry per order ever placed to answer a question
// that only ever concerns children.
//
// The empty parent is refused BEFORE the query, not by it. ” is the value on
// every ordinary order, so a literal answer would be the fund's whole order
// history handed to a caller that has lost track of which parent it meant.
//
// RLS scopes the read; no tenant predicate is written here, for the same reason
// List and Load write none.
func (p *Postgres) ListByParent(ctx context.Context, parentID string) ([]*orderpb.OrderState, error) {
	if parentID == "" {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT order_id, state FROM orders
		WHERE parent_order_id = $1 AND parent_order_id <> ''
		ORDER BY order_id
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list children of order %s: %w", parentID, err)
	}
	defer rows.Close()

	var out []*orderpb.OrderState
	for rows.Next() {
		var (
			id   string
			blob []byte
		)
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("scan child order: %w", err)
		}
		st, err := unmarshalState(blob, id)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func unmarshalState(blob []byte, orderID string) (*orderpb.OrderState, error) {
	var st orderpb.OrderState
	if err := proto.Unmarshal(blob, &st); err != nil {
		return nil, fmt.Errorf("decode order %s: %w", orderID, err)
	}
	return &st, nil
}

// Compile-time assertion that Postgres satisfies Store.
var _ Store = (*Postgres)(nil)
