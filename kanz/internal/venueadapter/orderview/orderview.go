// Package orderview is this adapter's OWN record of the orders it has been asked
// to work (INFRA-M7a-2).
//
// It exists because the connector's background workers need order context and the
// OMS is now in another process. The reconciler receives an exchange execution
// report carrying only a clOrdId and a symbol, and must enrich it into a Kanz
// fill FACT; the healing watchdog must know which orders it believes are still
// open at the venue. Both used to read the OMS order store directly, across an
// in-process pointer.
//
// Three ways to restore that across the split, and only one is right:
//
//   - Read the OMS's orders table. Cheapest, and a distributed monolith: a change
//     to the OMS's schema silently breaks the venue adapter.
//   - Call back into the OMS. Circular service dependency, and it puts the OMS on
//     this adapter's reconciliation hot path.
//   - Own the state. venue.v1.ExecuteRequest already carries the FULL OrderState —
//     the adapter is handed everything it needs about every order it works. It
//     never has to ask anyone. That is this package.
//
// The store is DURABLE, not a map, because it is exactly the state a restart must
// not lose: an adapter that reboots with an empty order view cannot enrich a fill
// arriving on the websocket for an order it worked a second ago, and cannot tell
// the reconciler which orders should be open. It would go quiet precisely when it
// matters.
package orderview

import (
	"context"
	"errors"
	"fmt"
	"sync"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// Store records the orders this adapter is working.
type Store interface {
	// Record stores (or refreshes) an order this adapter has been asked to work.
	Record(ctx context.Context, st *orderpb.OrderState) error
	// Get returns one order by id.
	Get(ctx context.Context, orderID string) (*orderpb.OrderState, bool, error)
	// Open returns the orders this adapter believes are still working at the venue.
	Open(ctx context.Context) ([]*orderpb.OrderState, error)
}

// Terminal reports whether an order is finished, and so no longer "open at the
// venue" for reconciliation purposes.
func Terminal(s orderpb.OrderStatus) bool {
	switch s {
	case orderpb.OrderStatus_ORDER_STATUS_FILLED,
		orderpb.OrderStatus_ORDER_STATUS_CANCELLED,
		orderpb.OrderStatus_ORDER_STATUS_REJECTED,
		orderpb.OrderStatus_ORDER_STATUS_EXPIRED:
		return true
	default:
		return false
	}
}

// Memory is the in-process Store — the test seam and the single-replica default.
type Memory struct {
	mu     sync.RWMutex
	orders map[string]*orderpb.OrderState
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return &Memory{orders: make(map[string]*orderpb.OrderState)}
}

func (m *Memory) Record(_ context.Context, st *orderpb.OrderState) error {
	if st.GetOrderId() == "" {
		return errors.New("orderview: cannot record an order with empty order_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orders[st.GetOrderId()] = proto.Clone(st).(*orderpb.OrderState)
	return nil
}

func (m *Memory) Get(_ context.Context, orderID string) (*orderpb.OrderState, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.orders[orderID]
	if !ok {
		return nil, false, nil
	}
	return proto.Clone(st).(*orderpb.OrderState), true, nil
}

func (m *Memory) Open(_ context.Context) ([]*orderpb.OrderState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*orderpb.OrderState
	for _, st := range m.orders {
		if !Terminal(st.GetStatus()) {
			out = append(out, proto.Clone(st).(*orderpb.OrderState))
		}
	}
	return out, nil
}

// Postgres is the durable Store, backed by 0001_orders.sql. State is marshaled
// order.v1.OrderState bytes: it carries exact base-10 Money/Decimal and `double`
// is banned on a capital path, so there is no lossless native column (the PERS-01
// opaque-bytes stance, same as the OMS order store).
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a Postgres store over an existing pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Record upserts. Unlike the OMS's Store.Create this is NOT an admission gate —
// admission already happened, in the OMS, before this adapter was ever called.
// This is a record of "I was asked to work this", and re-working the same order
// (a retry, a redelivery) must refresh it, not be rejected.
func (p *Postgres) Record(ctx context.Context, st *orderpb.OrderState) error {
	if st.GetOrderId() == "" {
		return errors.New("orderview: cannot record an order with empty order_id")
	}
	blob, err := proto.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal order %s: %w", st.GetOrderId(), err)
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO venue_orders (tenant_id, order_id, status, state)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3)
		ON CONFLICT (tenant_id, order_id) DO UPDATE SET
			status     = EXCLUDED.status,
			state      = EXCLUDED.state,
			updated_at = now()
	`, st.GetOrderId(), int32(st.GetStatus()), blob)
	if err != nil {
		return fmt.Errorf("record order %s: %w", st.GetOrderId(), err)
	}
	return nil
}

func (p *Postgres) Get(ctx context.Context, orderID string) (*orderpb.OrderState, bool, error) {
	var blob []byte
	err := p.pool.QueryRow(ctx,
		`SELECT state FROM venue_orders WHERE order_id = $1`, orderID).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get order %s: %w", orderID, err)
	}
	st, err := decode(blob, orderID)
	if err != nil {
		return nil, false, err
	}
	return st, true, nil
}

func (p *Postgres) Open(ctx context.Context) ([]*orderpb.OrderState, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT order_id, state FROM venue_orders
		WHERE status NOT IN ($1, $2, $3, $4)
		ORDER BY order_id
	`,
		int32(orderpb.OrderStatus_ORDER_STATUS_FILLED),
		int32(orderpb.OrderStatus_ORDER_STATUS_CANCELLED),
		int32(orderpb.OrderStatus_ORDER_STATUS_REJECTED),
		int32(orderpb.OrderStatus_ORDER_STATUS_EXPIRED),
	)
	if err != nil {
		return nil, fmt.Errorf("open orders: %w", err)
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
		st, err := decode(blob, id)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func decode(blob []byte, orderID string) (*orderpb.OrderState, error) {
	var st orderpb.OrderState
	if err := proto.Unmarshal(blob, &st); err != nil {
		return nil, fmt.Errorf("decode order %s: %w", orderID, err)
	}
	return &st, nil
}

var (
	_ Store = (*Memory)(nil)
	_ Store = (*Postgres)(nil)
)

// Seam adapts a Store to the connector's OrderLookup + ExpectedOrders interfaces,
// which are synchronous and non-failing by design (they sit inside websocket and
// reconciler loops that must not block on an error path). A store failure here
// degrades to "I don't know about this order" — the reconciler then treats it as
// unknown rather than inventing context for it. It never fabricates.
type Seam struct {
	store Store
	onErr func(error)
}

// NewSeam wraps store. onErr is called on a store failure (log it — a silently
// empty order view makes the healing watchdog blind).
func NewSeam(store Store, onErr func(error)) *Seam {
	return &Seam{store: store, onErr: onErr}
}

// Lookup satisfies execution.OrderLookup.
func (s *Seam) Lookup(orderID string) (*orderpb.OrderState, bool) {
	st, ok, err := s.store.Get(context.Background(), orderID)
	if err != nil {
		s.report(err)
		return nil, false
	}
	return st, ok
}

// OpenOrders satisfies execution.ExpectedOrders.
func (s *Seam) OpenOrders() []*orderpb.OrderState {
	open, err := s.store.Open(context.Background())
	if err != nil {
		s.report(err)
		return nil
	}
	return open
}

func (s *Seam) report(err error) {
	if s.onErr != nil {
		s.onErr(err)
	}
}
