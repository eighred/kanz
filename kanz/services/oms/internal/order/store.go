package order

import (
	"context"
	"errors"
	"sort"
	"sync"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/outbox"
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

// ErrFillApplied reports that this order aggregate already contains the fill the
// write was folding, so NOTHING was written — not the state, not the FACT, not
// the claim (#782).
//
// IT IS A SKIP, NOT A FAILURE. A caller that gets it should move to the next
// fill: the aggregate it holds already reflects this one, which is the correct
// end state and the whole point of the claim. Treating it as an error would
// quarantine an order for being reconciled twice, which is the ordinary case
// after a redelivery.
//
// The alternative — letting the fold apply and hoping nobody folded it before —
// silently double-counts filled_quantity and re-weights average_fill_price, and
// the stored OrderState keeps only the cumulative aggregate, so there is nothing
// downstream that could notice.
var ErrFillApplied = errors.New("oms: fill already folded into this order")

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
	// Create inserts the initial state of one order AND the FACTs announcing it,
	// in ONE transaction. It returns ErrExists — and writes nothing at all,
	// including no outbox record — if the order_id is already present. MUST be
	// atomic: two concurrent Creates of the same order_id must produce exactly
	// one success.
	//
	// THE announce PARAMETER IS THE POINT (#292). Admission used to be
	// store.Create followed by emitter.EmitAccepted: two independent writes, so
	// a publish failure left a durable order the estate had never heard of, and
	// the repair was a marker column plus a compensator that had to notice
	// (#238). Passing the FACT to the write makes that state unreachable —
	// either the order exists and its announcement is queued, or neither is
	// true. Nothing is announced by Create itself; the outbox relay publishes.
	//
	// nil announces nothing, which is a legitimate answer for a write that has
	// no FACT of its own. It is not a default: every caller states it.
	Create(ctx context.Context, st *orderpb.OrderState, announce []outbox.Record) error
	// Save persists the latest state of one order AND the FACTs announcing it,
	// in ONE transaction, iff the order is still at expectedVersion. It returns
	// ErrConflict — and writes nothing at all, including no outbox record — if it
	// is not. expectedVersion is the value Load returned alongside the state this
	// write was derived from.
	//
	// There is deliberately NO blind variant. A Save that cannot refuse is the
	// defect this replaced, and leaving one alongside would mean the next writer
	// reaches for it.
	//
	// THE announce PARAMETER IS ON THIS METHOD RATHER THAN ON A SECOND ONE, AND
	// THAT IS THE DECISION (#292). A `SaveAnnouncing` alongside a bare `Save`
	// would have been a smaller diff and exactly the wrong shape: two ways to
	// persist a transition is the drift that produced three separate
	// `*_announced_at` markers by instalment, and the plain one would stay the
	// obvious choice for the next transition somebody adds. One method, and every
	// caller states its answer — CLAUDE.md's "one implementation per concept",
	// and the same signature Create already carries.
	//
	// nil announces nothing, which is a legitimate answer for a write that has no
	// FACT of its own (a marker stamp, a quarantine freeze). It is NOT a default:
	// the parameter is a slice rather than a variadic precisely so that omitting
	// it does not compile, and so converting the next transition is an edit to one
	// call site rather than an argument nobody notices is missing.
	//
	// THE FILL FOLDS GO THROUGH HERE (#292). work() and adopt() pass the
	// ORDER_FILLED / ORDER_PARTIALLY_FILLED FACT with the state that records it,
	// which is the pair that most needed it: a fill is money that moved, it has
	// no marker, and completeTerminalOutcome cannot rebuild the FACT from the
	// stored aggregate. EVERY OTHER TRANSITION THAT WRITES NOW GOES THROUGH HERE
	// TOO — routing, the amend outcome, both terminal rejects, the cancellation
	// and its outcome, and the trailing outcome of a submit that filled. What
	// still publishes directly is what has no state change to commit alongside:
	// the pre-write refusals, and the #238 compensators repairing rows that have
	// no outbox record behind them. test/arch/oms_outbox_test.go names each one
	// and fails if a new pair appears.
	// THE fillID PARAMETER CLAIMS THE FILL THIS WRITE FOLDS (#782), in the same
	// transaction as the state and the FACT.
	//
	// "" means this write folds no fill, which is the answer for every transition
	// that is not an execution — routing, a cancel, a marker stamp. It is a plain
	// parameter rather than an option precisely so that omitting it does not
	// compile: a fill fold that forgot to claim would double-count on the next
	// redelivery, and the caller who forgot is exactly the one who would not
	// notice.
	//
	// A fill already claimed returns ErrFillApplied and writes NOTHING. That is
	// the guarantee ApplyFill cannot give on its own: it folds a fill into a
	// state, and the state records only the cumulative result, so the aggregate
	// itself cannot tell a first fold from a second.
	Save(ctx context.Context, st *orderpb.OrderState, expectedVersion int64, announce []outbox.Record, fillID string) error
	// Load returns the current state of one order and its version, or
	// ErrNotFound. The version is opaque to the caller: its only use is to be
	// handed back to Save.
	Load(ctx context.Context, orderID string) (*orderpb.OrderState, int64, error)
	// List returns a snapshot of all known orders, for bootstrap/inspection.
	List(ctx context.Context) ([]*orderpb.OrderState, error)
	// Outbox is the queue Create enqueues into.
	//
	// IT IS ON THIS INTERFACE RATHER THAN OBTAINED SEPARATELY, and that is a
	// safety decision (#292). The store and its outbox are one thing: they share
	// a transaction, a tenant scope and a failure domain. A composition root
	// that could get a store without its queue could wire the relay over a
	// DIFFERENT queue, or over none — and an OMS whose outbox nothing drains
	// admits orders, commits their FACTs and tells nobody, with every health
	// check green. Handing the queue out through the store makes the two
	// impossible to separate.
	Outbox() outbox.Queue

	// Proposals is the store of orders HELD for a second signature (#410).
	//
	// IT IS ON THIS INTERFACE FOR THE SAME REASON Outbox() IS. The three share a
	// transaction, a tenant scope and a failure domain: a held order's row and
	// the FACT announcing it commit together, and an approved proposal becomes a
	// row in this same `orders` table. A composition root that could obtain a
	// store WITHOUT its proposals home could arm OMS_REQUIRE_DUAL_CONTROL with
	// nowhere to put a held order — which is precisely the state config.Load
	// refused outright until this table existed, and the state that would reject
	// every large order while claiming a control.
	//
	// It also makes the two degrade TOGETHER. With no OMS_DATABASE_URL the
	// orders, the FACTs and the pending proposals are all in-process, so a crash
	// loses all three and they cannot come back disagreeing about which orders
	// were admitted and which were merely proposed.
	Proposals() ProposalStore

	// ListByPortfolio returns one portfolio's orders, newest first, at most
	// limit of them. It is the read behind the web app's order history (#399).
	//
	// unindexed IS PART OF THE ANSWER, NOT A DIAGNOSTIC. Orders admitted before
	// migration 0007 carry no portfolio_id — the portfolio lives only inside the
	// state blob, and no SQL can decode a protobuf — so they cannot appear in this
	// result however the query is written. Returning the list alone would present
	// a partial history as a complete one, which is the failure this repository
	// refuses everywhere else. The caller is told how many orders it could not
	// index so it can say so.
	//
	// It is a COUNT rather than the rows themselves because they are unreachable
	// by this query by construction: knowing there are 400 of them is actionable
	// (run the backfill), and inventing a way to return them here would be
	// building the scan this column exists to avoid.
	ListByPortfolio(ctx context.Context, portfolioID string, limit int) (orders []*orderpb.OrderState, unindexed int64, err error)

	// ListByStatus returns every order currently in one of the given statuses.
	// It exists for the startup sweep, which wants the OPEN orders and must not
	// load every order the fund has ever placed to find them. A durable backend
	// must answer it with a selection, not a scan — migrations/0001_orders.sql
	// carries orders_status_idx on (tenant_id, status) for exactly this.
	// Passing no statuses returns nothing.
	ListByStatus(ctx context.Context, statuses ...orderpb.OrderStatus) ([]*orderpb.OrderState, error)

	// ListByParent returns the children of one working parent order (#435).
	//
	// IT IS HOW THE SCHEDULE DRIVER KNOWS WHAT IT HAS ALREADY SENT, and it is a
	// query rather than a counter for a reason worth restating: a counter would
	// have to be advanced in the same breath as creating a child, which nothing
	// can do atomically. Advance it first and a crash loses a slice; advance it
	// second and a crash duplicates one. A child IS an order, so the children
	// that exist are exactly the ones that were sent, and asking is always right.
	//
	// It must be answered by a SELECTION, never a scan —
	// migrations/0008_orders_parent.sql carries orders_parent_idx for it, and
	// that index is PARTIAL, so a durable backend's query must repeat the
	// predicate or fall back to scanning the whole book on every driver tick.
	//
	// An empty parentID returns nothing rather than every unparented order: ""
	// is the value on every ordinary order in the book, and answering it
	// literally would return the entire order history to a caller that has
	// almost certainly lost track of which parent it meant.
	ListByParent(ctx context.Context, parentID string) ([]*orderpb.OrderState, error)
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
	// outbox is written under the SAME lock hold as the map, which is this
	// store's equivalent of Postgres.Create's transaction. It is owned here
	// rather than injected so a MemoryStore cannot be constructed without one:
	// a store with a nil outbox would silently drop every FACT handed to Create.
	outbox *outbox.Memory
	// proposals is owned here, over the SAME outbox, for the same reason: a
	// pending-approval FACT and an acceptance must not land in two queues.
	proposals *MemoryProposals
	// appliedFills is order_fills (#782): the fills already folded into an
	// aggregate. Held under the SAME lock as the map and the outbox, which is
	// this store's equivalent of the Postgres transaction — a claim that could
	// commit apart from the fold has two failure modes and both are real.
	appliedFills map[string]bool
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	q := outbox.NewMemory()
	return &MemoryStore{
		orders:       make(map[string]*versioned),
		appliedFills: make(map[string]bool),
		outbox:       q,
		proposals:    NewMemoryProposals(q),
	}
}

// Outbox is the in-process queue this store enqueues into. Never nil.
func (m *MemoryStore) Outbox() outbox.Queue { return m.outbox }

// Proposals is the in-process home of orders held for a second signature. Never
// nil, so an armed gate always has somewhere to put one.
func (m *MemoryStore) Proposals() ProposalStore { return m.proposals }

// Create inserts st iff its order_id is absent, and enqueues its FACTs in the
// same lock hold. The check, the insert and the enqueue happen under ONE lock —
// that atomicity IS the guarantee, and it is this store's whole equivalent of
// the transaction Postgres.Create opens. Splitting it back into Load-then-Save
// would restore the double-trade window; splitting the enqueue out of it would
// restore the lost-FACT window (#292).
func (m *MemoryStore) Create(_ context.Context, st *orderpb.OrderState, announce []outbox.Record) error {
	if st.GetOrderId() == "" {
		return errors.New("oms: cannot create order with empty order_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.orders[st.GetOrderId()]; ok {
		return ErrExists
	}
	// THE OUTBOX FIRST, THE ORDER SECOND. A failure here must leave NOTHING
	// behind — the same all-or-nothing the Postgres transaction gives — and the
	// map write cannot fail, so the only ordering that can honour that is the
	// fallible write first.
	if len(announce) > 0 {
		if err := m.outbox.Append(announce...); err != nil {
			return err
		}
	}
	// Version 0 matches the column default in migrations/0005: an order starts
	// unversioned and the first Save moves it to 1.
	m.orders[st.GetOrderId()] = &versioned{st: proto.Clone(st).(*orderpb.OrderState)}
	return nil
}

// Save applies st iff the order is still at expectedVersion, and enqueues its
// FACTs in the same lock hold. The compare, the write and the enqueue happen
// under ONE lock; splitting the compare from the write would reintroduce the
// check-then-act window this is here to close, and splitting the enqueue out of
// it would restore the lost-FACT window (#292) — for the fill fold, which is
// the pair with no marker behind it.
//
// THE COMPARE FIRST, THEN THE FALLIBLE ENQUEUE, THEN THE MAP WRITE. A refused
// CAS must leave NOTHING behind, so the version check precedes the enqueue; and
// the map write cannot fail, so it goes last. That is this store's whole
// equivalent of the transaction Postgres.Save opens.
func (m *MemoryStore) Save(_ context.Context, st *orderpb.OrderState, expectedVersion int64, announce []outbox.Record, fillID string) error {
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
	// THE CLAIM BEFORE ANYTHING ELSE IS WRITTEN, mirroring Postgres: a fill this
	// aggregate already contains means this write must not land at all, and
	// returning after the outbox append would leave a FACT for a fold that did
	// not happen.
	if fillID != "" && m.appliedFills[fillID] {
		return ErrFillApplied
	}
	if len(announce) > 0 {
		if err := m.outbox.Append(announce...); err != nil {
			return err
		}
	}
	if fillID != "" {
		m.appliedFills[fillID] = true
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

// ListByPortfolio filters the map. unindexed is always 0: this store keeps whole
// OrderStates rather than a denormalized column, so there is no such thing here
// as an order whose portfolio could not be indexed — the concept belongs to the
// Postgres schema, and reporting a non-zero count would be inventing a state
// this store cannot be in.
func (m *MemoryStore) ListByPortfolio(_ context.Context, portfolioID string, limit int) ([]*orderpb.OrderState, int64, error) {
	if portfolioID == "" {
		// An empty id is the NOT-INDEXED marker in Postgres, never a portfolio.
		// Answering it here would make the two stores disagree about what an
		// empty id means, which is exactly what MemoryStore exists to avoid.
		return nil, 0, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*orderpb.OrderState
	for _, v := range m.orders {
		if v.st.GetPortfolioId() == portfolioID {
			out = append(out, proto.Clone(v.st).(*orderpb.OrderState))
		}
	}
	// Newest first, matching the Postgres ordering. AsOf is the order's own
	// domain time; ties break on id so the order is total and a test is stable.
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].GetAsOf().AsTime(), out[j].GetAsOf().AsTime()
		if ti.Equal(tj) {
			return out[i].GetOrderId() > out[j].GetOrderId()
		}
		return ti.After(tj)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, 0, nil
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

// ListByParent filters the map (#435).
//
// THE EMPTY PARENT RETURNS NOTHING, matching Postgres — not because this store
// could not answer it, but because a seam that answered a question the durable
// store refuses would certify behaviour production does not have. Here "" would
// return every ordinary order in the book, which is the opposite of what any
// caller asking for a parent's children could possibly want.
func (m *MemoryStore) ListByParent(_ context.Context, parentID string) ([]*orderpb.OrderState, error) {
	if parentID == "" {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*orderpb.OrderState
	for _, v := range m.orders {
		if v.st.GetParentOrderId() == parentID {
			out = append(out, proto.Clone(v.st).(*orderpb.OrderState))
		}
	}
	return out, nil
}
