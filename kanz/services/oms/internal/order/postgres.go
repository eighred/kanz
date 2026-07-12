package order

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
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

// Save is the post-admission upsert: every state transition after Create. Safe
// as an upsert precisely because existence is already established and the bus
// partition_key serializes transitions per order_id.
func (p *Postgres) Save(ctx context.Context, st *orderpb.OrderState) error {
	if st.GetOrderId() == "" {
		return errors.New("oms: cannot save order with empty order_id")
	}
	blob, err := proto.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal order %s: %w", st.GetOrderId(), err)
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO orders (tenant_id, order_id, status, state)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3)
		ON CONFLICT (tenant_id, order_id) DO UPDATE SET
			status     = EXCLUDED.status,
			state      = EXCLUDED.state,
			updated_at = now()
	`, st.GetOrderId(), int32(st.GetStatus()), blob)
	if err != nil {
		return fmt.Errorf("save order %s: %w", st.GetOrderId(), err)
	}
	return nil
}

// Load returns the current state of one order, or ErrNotFound.
func (p *Postgres) Load(ctx context.Context, orderID string) (*orderpb.OrderState, error) {
	var blob []byte
	err := p.pool.QueryRow(ctx,
		`SELECT state FROM orders WHERE order_id = $1`, orderID,
	).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load order %s: %w", orderID, err)
	}
	return unmarshalState(blob, orderID)
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

func unmarshalState(blob []byte, orderID string) (*orderpb.OrderState, error) {
	var st orderpb.OrderState
	if err := proto.Unmarshal(blob, &st); err != nil {
		return nil, fmt.Errorf("decode order %s: %w", orderID, err)
	}
	return &st, nil
}

// Compile-time assertion that Postgres satisfies Store.
var _ Store = (*Postgres)(nil)
