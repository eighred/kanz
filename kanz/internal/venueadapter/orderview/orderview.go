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
	"time"

	"github.com/eighred/kanz/internal/execution"
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

// DefaultTerminalRetention is how long Memory keeps an order after it has gone
// terminal, and it is DERIVED rather than chosen: it is one full
// execution.DefaultReconcileInterval — the widest window this adapter operates
// on, the longest it tolerates its own view diverging from the exchange before
// something re-asks.
//
// WHY A TERMINAL ORDER CAN BE DROPPED AT ALL, which is the whole argument (#891).
// Work out what still reads one:
//
//   - Open, the healing watchdog's ExpectedOrders read, already EXCLUDES every
//     terminal order — Terminal() has always filtered its return value. So the
//     watchdog's view is bit-identical before and after this eviction, and no
//     order it can act on is reachable by it.
//   - HealClosures does not read this store at all: an in-flight close is
//     carried by execution.CloseRegistry, which the OMS writes at dispatch and
//     the watchdog force-clears on its own timeout.
//   - Get, through Seam.Lookup, is the one remaining reader — the user-data
//     ingester enriching an exchange execution report with the order's terms.
//
// So the ONLY thing a dropped entry can cost is the enrichment of a report that
// arrives after the order went terminal. There are exactly two ways an order
// goes terminal in this view, and in BOTH the exchange has already said it will
// not trade again: Server.recordStatus on a cancel the VENUE CONFIRMED, and
// Progress on a venue report of FILLED (#904 — before that, a filled order never
// went terminal here at all, which is why this eviction reached almost nothing).
// So a later report describes a trade that happened BEFORE that verdict and is
// still in flight on the websocket. Retention has to cover that lag, and a
// reconciliation pass is this adapter's own statement of the timescale on which
// venue truth is allowed to lag its view.
//
// THE RESIDUAL, STATED RATHER THAN GLOSSED. A report later than that finds
// Lookup empty and is skipped — the same answer the ingester already gives for
// an order this process never worked, which in THIS store is every order placed
// before the last restart. openView warns about exactly that: an in-memory order
// view loses the whole thing on restart and the watchdog goes blind across it.
// A bound measured in reconciliation passes sits well inside a failure envelope
// the posture already declares out loud, and unbounded growth does not.
const DefaultTerminalRetention = execution.DefaultReconcileInterval

// Compile-time assertion: retention must outlast the in-flight-close force-clear
// window, or an order could be evicted while the healing watchdog is still
// deciding what to do about the close that terminated it. Inverting the two
// makes this expression negative and the package stops compiling — the
// bus.DedupWindow / dedupClaimLease shape, for the same reason: an ordering
// between two intervals that only a comment enforces is one an edit breaks.
const _ = uint(DefaultTerminalRetention - execution.DefaultCloseTimeout - 1)

// Memory is the in-process Store — the test seam and the single-replica default.
//
// IT IS NOT PARITY-IDENTICAL TO Postgres, DELIBERATELY. The durable sibling
// keeps a terminal order's row forever (venue_orders is not pruned, and the DR
// posture in infra/dr counts on that row being there). This one keeps a terminal
// order for DefaultTerminalRetention and then forgets it, because a map in a
// process that must not be OOM-killed is not a table: it grew one permanent
// entry per order the adapter had ever worked, at whatever rate the strategy
// trades, and a venue adapter that dies of its heap is the process holding the
// exchange session for orders in flight. See DefaultTerminalRetention for why
// the entry it forgets is one nothing can still act on.
//
// A NON-TERMINAL ORDER IS NEVER EVICTED, at any age. That is the load-bearing
// half: the watchdog reconciles against exactly those, and an order the adapter
// still believes is working at the exchange is the one thing this store exists
// to be able to answer for.
type Memory struct {
	mu     sync.RWMutex
	orders map[string]memEntry
	// retain is how long a terminal order stays readable. <=0 disables eviction.
	retain time.Duration
	// lastSweep is when evictTerminal last walked the map. The sweep is
	// AMORTIZED — once per retain, on the write path — so Record stays O(1) at
	// the order rate rather than O(n) per order. The cost of amortizing is that
	// an entry lives between retain and 2*retain; the bound that matters is that
	// it is a function of the terminal-order RATE over a fixed window, and no
	// longer of how long the process has been up.
	lastSweep time.Time
	// now is the time source, swappable from this package's tests. The retention
	// is a full reconciliation pass, so expiry is not testable by sleeping — the
	// same seam and the same reason as bus.DedupWindow's.
	now func() time.Time
}

// memEntry is a recorded order plus the instant it FIRST went terminal (zero
// while it is still working).
type memEntry struct {
	state      *orderpb.OrderState
	terminalAt time.Time
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return &Memory{
		orders:    make(map[string]memEntry),
		retain:    DefaultTerminalRetention,
		lastSweep: time.Now(),
		now:       time.Now,
	}
}

func (m *Memory) Record(_ context.Context, st *orderpb.OrderState) error {
	if st.GetOrderId() == "" {
		return errors.New("orderview: cannot record an order with empty order_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	e := memEntry{state: proto.Clone(st).(*orderpb.OrderState)}
	if Terminal(st.GetStatus()) {
		// FIRST terminal sighting wins, and a re-record does not reset the clock.
		// A redelivered or retried close would otherwise refresh the entry
		// forever and the retention would never elapse — the same reasoning as
		// execution.CloseRegistry.Track preserving its original RequestedAt.
		e.terminalAt = now
		if prev, ok := m.orders[st.GetOrderId()]; ok && !prev.terminalAt.IsZero() {
			e.terminalAt = prev.terminalAt
		}
	}
	m.orders[st.GetOrderId()] = e
	m.evictTerminal(now)
	return nil
}

// evictTerminal drops orders that have been terminal for longer than the
// retention. Caller holds m.mu.
//
// It runs on the WRITE path on purpose: the map only grows in Record, so a sweep
// there is the one place a bound can be enforced without a goroutine the store
// would then have to own a lifecycle for. A store nobody writes to does not
// grow, so there is nothing for a background sweep to do that this misses.
func (m *Memory) evictTerminal(now time.Time) {
	if m.retain <= 0 || now.Sub(m.lastSweep) < m.retain {
		return
	}
	m.lastSweep = now
	for id, e := range m.orders {
		if e.terminalAt.IsZero() {
			continue // still working at the venue — never evicted, at any age
		}
		if now.Sub(e.terminalAt) >= m.retain {
			delete(m.orders, id)
		}
	}
}

func (m *Memory) Get(_ context.Context, orderID string) (*orderpb.OrderState, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.orders[orderID]
	if !ok {
		return nil, false, nil
	}
	return proto.Clone(e.state).(*orderpb.OrderState), true, nil
}

func (m *Memory) Open(_ context.Context) ([]*orderpb.OrderState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*orderpb.OrderState
	for _, e := range m.orders {
		if !Terminal(e.state.GetStatus()) {
			out = append(out, proto.Clone(e.state).(*orderpb.OrderState))
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
	_ Store                  = (*Memory)(nil)
	_ Store                  = (*Postgres)(nil)
	_ execution.OrderTracker = (*Seam)(nil)
)

// ErrNotInView is returned when Progress is asked to advance an order this
// adapter has no record of. It is a REFUSAL, not a failure to find something:
// the reported state is a partial reconstruction (the venue tells us a status
// and a filled size, not an order's terms), so writing it as a new entry would
// put an order into the view with its stop price, its GTD expiry, its parent,
// its leverage and its margin mode all absent — the #405/#240 shape, a field the
// system had and then quietly did not.
var ErrNotInView = errors.New("orderview: order is not in this adapter's view")

// ErrUnmappedStatus is returned when the reported status is UNSPECIFIED — the
// answer both connectors' status tables give for a venue string they do not
// know. UNSPECIFIED IS "THE VENUE SAID SOMETHING WE CANNOT READ", never "the
// order has no status", and overwriting a known ROUTED with it would replace
// what this adapter knows with what it does not.
var ErrUnmappedStatus = errors.New("orderview: refusing to record an order at an unmapped venue status")

// ErrTerminalNotReopened is returned when a non-terminal report arrives for an
// order already terminal here. See Progress.
var ErrTerminalNotReopened = errors.New("orderview: refusing to reopen a terminal order")

// Progress folds the venue's OWN report of an order's progress into this
// adapter's view — the state the user-data ingester has ALREADY published as an
// order.order.filled / order.order.partially_filled FACT (#904).
//
// WHY THIS EXISTS. Before it, the only writer that ever advanced an order to a
// terminal status was Server.recordStatus on a venue-confirmed cancel. A FILLED
// order stayed at the status the OMS handed Execute forever, so Open kept
// returning it, the reconciler kept spending REST weight on it, healedState kept
// finding permanent drift and re-emitting a StateHealed FACT for it every pass,
// and DefaultTerminalRetention could never evict it because it never went
// terminal. This is the write that makes all four stop.
//
// IT MERGES, IT DOES NOT REPLACE, and that is the load-bearing half. Both
// ingesters build their healed OrderState from scratch out of the venue report
// plus a handful of Kanz terms — twelve of the message's thirty-two fields.
// Recording it wholesale would silently drop stop_price, expire_at,
// arrival_price/arrival_at, execution_schedule, parent_order_id, venue_account_id,
// quarantine, leverage, margin_mode and the release_* marks from an order the
// adapter is still working. That is precisely the defect
// test/arch/submit_fields_reach_state_test.go exists to catch one layer up: a
// field the system accepted and then did not carry. So the stored order stays
// authoritative for its terms, and only what the EXCHANGE observed — status,
// filled and leaves quantities, average fill price, as_of — is taken from the
// report, and only where the report actually set it.
//
// A TERMINAL ORDER IS NEVER REOPENED. A late or out-of-order report — a trade
// executed before a cancel the venue then confirmed, arriving on the websocket
// after it — would otherwise move a CANCELLED or FILLED order back to
// PARTIALLY_FILLED, put it back into Open, restart the re-query and the
// duplicate FACTs, and reset the eviction it had become eligible for. Terminal
// to terminal IS allowed: the venue saying FILLED about an order we recorded
// CANCELLED is venue truth about a trade that happened, and Memory keeps the
// FIRST terminal sighting as the retention clock so this cannot refresh it.
//
// CONCURRENCY, STATED RATHER THAN IMPLIED, AND THE PREMISE HAS ALREADY MOVED
// ONCE. This is a read-modify-write across two Store calls and is NOT atomic;
// only the individual calls are. The single writer of fill progress is one
// ingester goroutine. The other two writers no longer overwrite unconditionally
// the way this paragraph used to say they did: Execute refuses outright when it
// finds a terminal order (#914), and Server.recordStatus keeps a terminal
// verdict and otherwise merges onto what it read (#921). So a lost update is no
// longer a certainty on the cancel path — it is the width of one Get-to-Record
// window, in which a confirmed cancel can still land on a pre-fill read and
// write CANCELLED over a FILLED this call had just established. Both are
// terminal, so Open and the eviction clock are unaffected and only the recorded
// verdict is; closing that window needs a conditional write neither backend has,
// which is #934. It cannot fabricate a status: every value written came from a
// venue report. -race does not run on the usual Windows box (no cgo), so CI is
// the detector for this path.
func Progress(ctx context.Context, store Store, reported *orderpb.OrderState) error {
	id := reported.GetOrderId()
	if id == "" {
		return errors.New("orderview: cannot record progress for an order with empty order_id")
	}
	next := reported.GetStatus()
	if next == orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED {
		return fmt.Errorf("%w: order %s", ErrUnmappedStatus, id)
	}
	cur, ok, err := store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("progress order %s: %w", id, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotInView, id)
	}
	if Terminal(cur.GetStatus()) && !Terminal(next) {
		return fmt.Errorf("%w: %s is %v and the venue now reports %v", ErrTerminalNotReopened, id, cur.GetStatus(), next)
	}
	merged, mok := proto.Clone(cur).(*orderpb.OrderState)
	if !mok {
		return fmt.Errorf("orderview: order %s did not clone", id)
	}
	merged.Status = next
	// Each guarded on the report having SET it: a nil here would blank a quantity
	// the view already holds, and a blanked filled_quantity reads downstream as an
	// order that traded nothing.
	if q := reported.GetFilledQuantity(); q != nil {
		merged.FilledQuantity = q
	}
	if q := reported.GetLeavesQuantity(); q != nil {
		merged.LeavesQuantity = q
	}
	if p := reported.GetAverageFillPrice(); p != nil {
		merged.AverageFillPrice = p
	}
	if t := reported.GetAsOf(); t != nil {
		merged.AsOf = t
	}
	return store.Record(ctx, merged)
}

// Seam adapts a Store to the connector's OrderTracker + ExpectedOrders interfaces,
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

// Progressed satisfies execution.OrderTracker's write half: it records the venue's
// own report of an order's progress, which is the state the caller has already
// published as a fill FACT (#904).
//
// Non-failing, like Lookup and OpenOrders and for the same reason — it is called
// from inside a websocket read loop. Every refusal and every store failure goes
// to onErr instead, and NONE of them is silent: an order that stops advancing
// here is one the reconciler will keep re-querying and re-healing forever, which
// is the whole defect this method closes.
func (s *Seam) Progressed(st *orderpb.OrderState) {
	if err := Progress(context.Background(), s.store, st); err != nil {
		s.report(err)
	}
}

func (s *Seam) report(err error) {
	if s.onErr != nil {
		s.onErr(err)
	}
}
