package order

import (
	"context"
	"errors"
	"sync"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

// ErrNotFound is returned by Store.Load when no order has the given id.
var ErrNotFound = errors.New("oms: order not found")

// Store is the durable home of order aggregate state (PERS-01). The in-memory
// default below is the floor that preserves exact semantics for tests and a
// single replica; a durable, replayable backend (Postgres, the risk-engine
// persist.StateStore stance) plugs in behind this interface with no change to
// the aggregate or the command handler — the order_id-keyed Save/Load is the
// whole contract. Save is upsert; it is the consumer's responsibility to call
// it under the per-order ordering the bus partition_key already guarantees.
type Store interface {
	// Save persists the latest state of one order (upsert by order_id).
	Save(ctx context.Context, st *orderpb.OrderState) error
	// Load returns the current state of one order, or ErrNotFound.
	Load(ctx context.Context, orderID string) (*orderpb.OrderState, error)
	// List returns a snapshot of all known orders, for bootstrap/inspection.
	List(ctx context.Context) ([]*orderpb.OrderState, error)
}

// MemoryStore is the in-process Store. Goroutine-safe; stores cloned protos so
// a caller mutating a returned state cannot corrupt the store.
type MemoryStore struct {
	mu     sync.RWMutex
	orders map[string]*orderpb.OrderState
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{orders: make(map[string]*orderpb.OrderState)}
}

func (m *MemoryStore) Save(_ context.Context, st *orderpb.OrderState) error {
	if st.GetOrderId() == "" {
		return errors.New("oms: cannot save order with empty order_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orders[st.GetOrderId()] = proto.Clone(st).(*orderpb.OrderState)
	return nil
}

func (m *MemoryStore) Load(_ context.Context, orderID string) (*orderpb.OrderState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.orders[orderID]
	if !ok {
		return nil, ErrNotFound
	}
	return proto.Clone(st).(*orderpb.OrderState), nil
}

func (m *MemoryStore) List(_ context.Context) ([]*orderpb.OrderState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*orderpb.OrderState, 0, len(m.orders))
	for _, st := range m.orders {
		out = append(out, proto.Clone(st).(*orderpb.OrderState))
	}
	return out, nil
}
