package order

import (
	"context"
	"errors"
	"sync"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

// ErrNotFound is returned by Store.Load when no order has the given id.
var ErrNotFound = errors.New("oms: order not found")

// ErrExists is returned by Store.Create when an order with that id already
// exists. It is the signal that THIS delivery lost the admission race and must
// not work the order — another delivery already owns it.
var ErrExists = errors.New("oms: order already exists")

// ErrConflict is returned by Store.Save when the order has moved since the
// caller loaded it — another writer, in this process or another replica, got
// there first (#122).
//
// IT IS NOT AN ERROR IN THE USUAL SENSE. It is the store telling this caller
// that the state it computed was derived from a version that no longer exists,
// so applying it would discard somebody else's transition. Before this existed,
// Save was a blind upsert and that discard happened silently: a CANCELLED order
// came back as FILLED and nothing recorded it.
//
// Every call site must DECIDE what a conflict means rather than propagate it
// blindly. The two honest answers are "re-read and retry" (the state was
// derived from the order and can be recomputed) and "quarantine" (this delivery
// holds a fact — a fill — that the winner did not see, so the order's truth is
// now unknown and a human must look).
var ErrConflict = errors.New("oms: order changed since load")

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
//   - Save is the COMPARE-AND-SWAP used for every state transition AFTER
//     admission. It takes the version the caller loaded and applies its write
//     only if the order is still at that version, returning ErrConflict
//     otherwise. It used to be a blind upsert justified by the claim that the
//     bus partition_key guaranteed per-order ordering — it does not, nothing in
//     the consumer reads it for ordering, and submit, amend and cancel arrive on
//     three separate durables with three dispatch goroutines. The service's
//     per-order lock (orderlock.go) excludes writers within a process; the
//     version excludes them ACROSS replicas, which is the half the lock cannot
//     reach by construction (#122).
type Store interface {
	// Create inserts the initial state of one order. It returns ErrExists — and
	// writes nothing — if the order_id is already present. MUST be atomic: two
	// concurrent Creates of the same order_id must produce exactly one success.
	Create(ctx context.Context, st *orderpb.OrderState) error
	// Save persists the latest state of one order iff it is still at
	// expectedVersion, and returns ErrConflict if it is not. expectedVersion is
	// the value Load returned alongside the state this write was derived from.
	//
	// There is deliberately NO blind variant. A Save that cannot refuse is the
	// defect this replaced, and leaving one alongside would mean the next writer
	// reaches for it.
	Save(ctx context.Context, st *orderpb.OrderState, expectedVersion int64) error
	// Load returns the current state of one order and its version, or
	// ErrNotFound. The version is opaque to the caller: its only use is to be
	// handed back to Save.
	Load(ctx context.Context, orderID string) (*orderpb.OrderState, int64, error)
	// List returns a snapshot of all known orders, for bootstrap/inspection.
	List(ctx context.Context) ([]*orderpb.OrderState, error)
	// ListByStatus returns every order currently in one of the given statuses.
	// It exists for the startup sweep, which wants the OPEN orders and must not
	// load every order the fund has ever placed to find them. A durable backend
	// must answer it with a selection, not a scan — migrations/0001_orders.sql
	// carries orders_status_idx on (tenant_id, status) for exactly this.
	// Passing no statuses returns nothing.
	ListByStatus(ctx context.Context, statuses ...orderpb.OrderStatus) ([]*orderpb.OrderState, error)
}

// versioned couples an order's state to its version.
//
// ONE MAP ENTRY, not two parallel maps. A separate versions map could be updated
// without the state (or the reverse) and the divergence would only surface as a
// CAS that wrongly succeeds — the exact failure this type exists to prevent.
type versioned struct {
	st  *orderpb.OrderState
	ver int64
}

// MemoryStore is the in-process Store. Goroutine-safe; stores cloned protos so
// a caller mutating a returned state cannot corrupt the store.
//
// It implements the SAME compare-and-swap contract as Postgres, including
// ErrConflict on a stale version. That is not decoration: every service-level
// test in this package runs against this store, so a seam that accepted writes
// Postgres refuses would certify behaviour production does not have. fakeBus
// already taught this repository what a permissive test double costs.
type MemoryStore struct {
	mu     sync.RWMutex
	orders map[string]*versioned
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{orders: make(map[string]*versioned)}
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
	// Version 0 matches the column default in migrations/0005: an order starts
	// unversioned and the first Save moves it to 1.
	m.orders[st.GetOrderId()] = &versioned{st: proto.Clone(st).(*orderpb.OrderState)}
	return nil
}

// Save applies st iff the order is still at expectedVersion. The compare and the
// write happen under ONE lock hold; splitting them would reintroduce the
// check-then-act window this is here to close.
func (m *MemoryStore) Save(_ context.Context, st *orderpb.OrderState, expectedVersion int64) error {
	if st.GetOrderId() == "" {
		return errors.New("oms: cannot save order with empty order_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.orders[st.GetOrderId()]
	if !ok {
		return ErrNotFound
	}
	if cur.ver != expectedVersion {
		return ErrConflict
	}
	m.orders[st.GetOrderId()] = &versioned{
		st:  proto.Clone(st).(*orderpb.OrderState),
		ver: cur.ver + 1,
	}
	return nil
}

func (m *MemoryStore) Load(_ context.Context, orderID string) (*orderpb.OrderState, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.orders[orderID]
	if !ok {
		return nil, 0, ErrNotFound
	}
	return proto.Clone(v.st).(*orderpb.OrderState), v.ver, nil
}

func (m *MemoryStore) List(_ context.Context) ([]*orderpb.OrderState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*orderpb.OrderState, 0, len(m.orders))
	for _, v := range m.orders {
		out = append(out, proto.Clone(v.st).(*orderpb.OrderState))
	}
	return out, nil
}

func (m *MemoryStore) ListByStatus(_ context.Context, statuses ...orderpb.OrderStatus) ([]*orderpb.OrderState, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	want := make(map[orderpb.OrderStatus]bool, len(statuses))
	for _, s := range statuses {
		want[s] = true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*orderpb.OrderState
	for _, v := range m.orders {
		if want[v.st.GetStatus()] {
			out = append(out, proto.Clone(v.st).(*orderpb.OrderState))
		}
	}
	return out, nil
}
