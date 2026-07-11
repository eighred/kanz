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

// ErrExists is returned by Store.Create when an order with that id already
// exists. It is the signal that THIS delivery lost the admission race and must
// not work the order — another delivery already owns it.
var ErrExists = errors.New("oms: order already exists")

// Store is the durable home of order aggregate state (PERS-01). The in-memory
// default below is the floor that preserves exact semantics for tests and a
// single replica; a durable, replayable backend (Postgres, the risk-engine
// persist.StateStore stance) plugs in behind this interface with no change to
// the aggregate or the command handler.
//
// Create vs Save is the whole admission contract, and the distinction is
// load-bearing:
//
//   - Create is the ATOMIC ADMISSION GATE. Exactly one caller can create a given
//     order_id; every other caller gets ErrExists. It exists because the handler
//     used to admit an order with Load()-then-Save() — a check-then-act — and the
//     thing that runs immediately after admission is `route the order to the
//     venue`. Two concurrent deliveries of one SubmitOrder both saw ErrNotFound,
//     both admitted, and both routed: the state converged (Save is an upsert, so
//     it LOOKED idempotent) while the fund traded twice. A durable backend must
//     enforce this at the engine — INSERT ... ON CONFLICT (order_id) DO NOTHING,
//     or a unique constraint — never a SELECT followed by an INSERT.
//   - Save is the upsert used for every state transition AFTER admission, where
//     the order's existence is already established and the bus partition_key
//     guarantees per-order ordering.
type Store interface {
	// Create inserts the initial state of one order. It returns ErrExists — and
	// writes nothing — if the order_id is already present. MUST be atomic: two
	// concurrent Creates of the same order_id must produce exactly one success.
	Create(ctx context.Context, st *orderpb.OrderState) error
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

// Create inserts st iff its order_id is absent. The check and the insert happen
// under one lock hold — that atomicity IS the guarantee, and splitting it back
// into Load-then-Save would restore the double-trade window.
func (m *MemoryStore) Create(_ context.Context, st *orderpb.OrderState) error {
	if st.GetOrderId() == "" {
		return errors.New("oms: cannot create order with empty order_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.orders[st.GetOrderId()]; ok {
		return ErrExists
	}
	m.orders[st.GetOrderId()] = proto.Clone(st).(*orderpb.OrderState)
	return nil
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
