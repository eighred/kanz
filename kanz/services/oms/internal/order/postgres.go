package order

import (
	"context"
	"errors"
	"fmt"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
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
}

// NewPostgres returns a Postgres store over an existing pool. The caller owns
// the pool lifecycle (Close).
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Create is the ATOMIC ADMISSION GATE. It is one statement — an INSERT that the
// engine either applies or discards on conflict — and the RowsAffected it
// reports is the verdict: 1 ⇒ this delivery won and owns the order; 0 ⇒ another
// delivery already created it, so this one lost and MUST NOT route to the venue
// (ErrExists is that signal).
//
// Never rewrite this as a SELECT followed by an INSERT. The check-then-act
// window between them is exactly the double-trade bug this store was built to
// close, and it is invisible in tests because Save is an upsert: the state still
// converges while the fund trades twice.
func (p *Postgres) Create(ctx context.Context, st *orderpb.OrderState) error {
	if st.GetOrderId() == "" {
		return errors.New("oms: cannot create order with empty order_id")
	}
	blob, err := proto.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal order %s: %w", st.GetOrderId(), err)
	}
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO orders (tenant_id, order_id, status, state)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3)
		ON CONFLICT (tenant_id, order_id) DO NOTHING
	`, st.GetOrderId(), int32(st.GetStatus()), blob)
	if err != nil {
		return fmt.Errorf("create order %s: %w", st.GetOrderId(), err)
	}
	if tag.RowsAffected() == 0 {
		return ErrExists
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
func (p *Postgres) Save(ctx context.Context, st *orderpb.OrderState, expectedVersion int64) error {
	if st.GetOrderId() == "" {
		return errors.New("oms: cannot save order with empty order_id")
	}
	blob, err := proto.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal order %s: %w", st.GetOrderId(), err)
	}
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO orders (tenant_id, order_id, status, state)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3)
		ON CONFLICT (tenant_id, order_id) DO UPDATE SET
			status     = EXCLUDED.status,
			state      = EXCLUDED.state,
			version    = orders.version + 1,
			updated_at = now()
		WHERE orders.version = $4
	`, st.GetOrderId(), int32(st.GetStatus()), blob, expectedVersion)
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

func unmarshalState(blob []byte, orderID string) (*orderpb.OrderState, error) {
	var st orderpb.OrderState
	if err := proto.Unmarshal(blob, &st); err != nil {
		return nil, fmt.Errorf("decode order %s: %w", orderID, err)
	}
	return &st, nil
}

// Compile-time assertion that Postgres satisfies Store.
var _ Store = (*Postgres)(nil)
