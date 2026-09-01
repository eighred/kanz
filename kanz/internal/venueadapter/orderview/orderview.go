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
//
// THERE IS EXACTLY ONE READ, AND IT MINTS THE TOKEN THE CONDITIONAL WRITE NEEDS
// (#934). Get could have kept its old three-value shape with a separate
// read-for-update beside it, and that would have been a trap of the kind this
// repository keeps paying for: a test decorator that fakes Get would then not be
// consulted by the write path at all, and the test would go green while
// asserting nothing about it. One read method cannot be half-overridden.
type Store interface {
	// Record stores (or refreshes) an order this adapter has been asked to work.
	// It is UNCONDITIONAL, and that is the seed/refresh contract: an adapter
	// asked to work an order it already holds must refresh it, not be rejected.
	// A read-modify-write must use Get + RecordIf (or Update) instead.
	//
	// NO PRODUCTION WRITER CALLS IT AFTER #944. Server.Execute was the last one,
	// and its unconditional upsert is what erased the venue's own partial-fill
	// quantities on a re-dispatch; it goes through Dispatch now. This stays
	// because it IS the contract — the one primitive that seeds a row and the
	// thing RecordIf is defined against — and because #905 pins it. Reaching for
	// it from a new write path is how that erasure comes back.
	Record(ctx context.Context, st *orderpb.OrderState) error
	// Get returns one order by id, together with the Revision of the EXACT value
	// returned — including when there is no row, which is itself a value a
	// conditional write can be made against.
	Get(ctx context.Context, orderID string) (*orderpb.OrderState, Revision, bool, error)
	// RecordIf writes st only if the store is still at the value `at` was minted
	// from, and reports whether it applied. applied == false is not a failure: it
	// means another writer got there first and the caller must re-read and
	// re-decide. An `at` this store did not mint is refused with an error rather
	// than guessed at.
	RecordIf(ctx context.Context, st *orderpb.OrderState, at Revision) (bool, error)
	// Open returns the orders this adapter believes are still working at the venue.
	Open(ctx context.Context) ([]*orderpb.OrderState, error)
}

// Revision identifies the exact stored value one Get returned. It is the token a
// RecordIf is made against, and it is OPAQUE on purpose: the two backends
// establish "still the same value" by different means and neither is a thing a
// caller should be reasoning about.
//
// WHY NOT A COMPARE-AND-SET ON THE STATUS, which is the shape that suggests
// itself first and is the one this issue had to reject. Both writers that need
// this write a status CHANGE, so a status predicate does catch the interleaving
// where the racing writer also moved the status. It does not catch the ordinary
// one: a partially filling order takes another fill, and the venue's own report
// writes PARTIALLY_FILLED over PARTIALLY_FILLED with a LARGER filled_quantity. A
// status CAS succeeds against that, the merge is still built on the pre-fill
// read, and the quantity the exchange reported is lost exactly as before — with
// a green test next to it. The condition has to be the value that was read, not
// a projection of it.
//
// WHY NOT A version COLUMN, which is the clearer of the two exact shapes. It
// needs a migration to venue_orders in BOTH deployed venue services, kept
// byte-identical (TestBothDeployedAdaptersRunTheSameOrderViewSchema), on a table
// both adapters already run in production. The stored state bytes are already an
// exact revision token and cost no schema change at all.
//
// THE BYTES ARE THE ONES THAT WERE READ, NEVER RE-MARSHALED. proto.Marshal is
// explicitly not guaranteed to be deterministic, so comparing a re-marshal of
// the value a caller is holding could refuse a write that should have applied —
// intermittently, and more often the larger the message. Get carries the row's
// own bytes out with it and RecordIf compares those.
//
// THE ZERO Revision IS NOT A WILDCARD. It is what a caller has when it never
// read anything, and a conditional write that cannot establish what it would be
// overwriting must refuse rather than proceed — the critical-unknown rule, on the
// capital path's own record of what the venue did.
type Revision struct {
	// source is the backend that minted this token. A token from the other one
	// means the caller mixed two stores up, and the zero value means it was
	// never minted at all; both are refused.
	source revisionSource
	// present is whether there was a row. A conditional write made against an
	// absent revision applies only while the row is STILL absent, so two writers
	// racing to seed the same order cannot both win.
	present bool
	// state is the stored bytes exactly as Postgres returned them.
	state []byte
	// seq is Memory's per-entry write counter, drawn from a store-wide monotonic
	// source so an entry that was evicted and re-recorded never reuses one.
	seq uint64
}

type revisionSource uint8

const (
	revisionUnminted revisionSource = iota
	revisionMemory
	revisionPostgres
)

// ErrUnmintedRevision is returned when RecordIf is handed a Revision this store
// did not produce — the zero value, or one from the other backend. It is a
// REFUSAL and never a lost race: the store cannot establish what the write would
// be overwriting, and on this path an unknown fails closed.
var ErrUnmintedRevision = errors.New("orderview: refusing a conditional write against a revision this store did not mint")

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
	// seq is the store-wide write counter every entry's revision is stamped
	// from. STORE-WIDE rather than per entry, so an order that is evicted and
	// later re-recorded cannot be handed a revision a stale reader is still
	// holding — a per-entry counter restarting at zero would make exactly that
	// collision, and the losing write would silently apply.
	seq uint64
}

// memEntry is a recorded order, the instant it FIRST went terminal (zero while
// it is still working), and the revision of this exact value.
type memEntry struct {
	state      *orderpb.OrderState
	terminalAt time.Time
	seq        uint64
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
	m.recordLocked(st)
	return nil
}

// RecordIf writes st only while the entry is still at the value `at` was minted
// from. Under the same mutex Get read it, so "still" is exact.
func (m *Memory) RecordIf(_ context.Context, st *orderpb.OrderState, at Revision) (bool, error) {
	if st.GetOrderId() == "" {
		return false, errors.New("orderview: cannot record an order with empty order_id")
	}
	if at.source != revisionMemory {
		return false, fmt.Errorf("%w: order %s", ErrUnmintedRevision, st.GetOrderId())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, ok := m.orders[st.GetOrderId()]
	// Presence is half the condition. A revision read from an absent entry
	// applies only while it is still absent, so a seed cannot overwrite an order
	// another writer inserted in the meantime.
	if ok != at.present {
		return false, nil
	}
	if ok && prev.seq != at.seq {
		return false, nil
	}
	m.recordLocked(st)
	return true, nil
}

// recordLocked is the one write body both Record and RecordIf go through, so the
// terminal-clock rule and the eviction sweep cannot drift between them. Caller
// holds m.mu.
func (m *Memory) recordLocked(st *orderpb.OrderState) {
	now := m.now()
	m.seq++
	e := memEntry{state: proto.Clone(st).(*orderpb.OrderState), seq: m.seq}
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

func (m *Memory) Get(_ context.Context, orderID string) (*orderpb.OrderState, Revision, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.orders[orderID]
	if !ok {
		return nil, Revision{source: revisionMemory}, false, nil
	}
	rev := Revision{source: revisionMemory, present: true, seq: e.seq}
	return proto.Clone(e.state).(*orderpb.OrderState), rev, true, nil
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

func (p *Postgres) Get(ctx context.Context, orderID string) (*orderpb.OrderState, Revision, bool, error) {
	var blob []byte
	err := p.pool.QueryRow(ctx,
		`SELECT state FROM venue_orders WHERE order_id = $1`, orderID).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, Revision{source: revisionPostgres}, false, nil
	}
	if err != nil {
		return nil, Revision{}, false, fmt.Errorf("get order %s: %w", orderID, err)
	}
	st, err := decode(blob, orderID)
	if err != nil {
		return nil, Revision{}, false, err
	}
	// The revision is the row's OWN bytes, carried out of the read. Re-marshaling
	// st here would look equivalent and would not be: proto.Marshal is not
	// guaranteed deterministic, so the comparison could refuse a write nothing
	// had raced.
	return st, Revision{source: revisionPostgres, present: true, state: blob}, true, nil
}

// RecordIf writes st only while venue_orders still holds the exact bytes `at` was
// read from, and reports whether it applied.
//
// TWO STATEMENTS, BECAUSE ABSENT IS A VALUE. Against a row that was there, the
// predicate is `state = $4` — an equality on the bytes Get returned, evaluated by
// the engine under the row lock the UPDATE takes, which is what makes the whole
// read-decide-write atomic across replicas rather than only across goroutines.
// Against a row that was NOT there, the conditional insert is ON CONFLICT DO
// NOTHING: zero rows affected means somebody else seeded the order first, which
// is a lost race and not a failure.
//
// NEITHER STATEMENT CARRIES A TENANT PREDICATE, exactly like Get and Open. The
// tenant_isolation policy is FORCE ROW LEVEL SECURITY and is enforced at the
// engine on both the USING and the WITH CHECK side, so a conditional write can
// no more reach another tenant's row than an unconditional one can — and it
// cannot silently match zero rows for the wrong reason either, because
// app_current_tenant() RAISES on an unscoped session (MT-01e).
func (p *Postgres) RecordIf(ctx context.Context, st *orderpb.OrderState, at Revision) (bool, error) {
	if st.GetOrderId() == "" {
		return false, errors.New("orderview: cannot record an order with empty order_id")
	}
	if at.source != revisionPostgres {
		return false, fmt.Errorf("%w: order %s", ErrUnmintedRevision, st.GetOrderId())
	}
	blob, err := proto.Marshal(st)
	if err != nil {
		return false, fmt.Errorf("marshal order %s: %w", st.GetOrderId(), err)
	}
	if !at.present {
		tag, ierr := p.pool.Exec(ctx, `
			INSERT INTO venue_orders (tenant_id, order_id, status, state)
			VALUES (current_setting('app.tenant_id'), $1, $2, $3)
			ON CONFLICT (tenant_id, order_id) DO NOTHING
		`, st.GetOrderId(), int32(st.GetStatus()), blob)
		if ierr != nil {
			return false, fmt.Errorf("conditionally seed order %s: %w", st.GetOrderId(), ierr)
		}
		return tag.RowsAffected() == 1, nil
	}
	tag, uerr := p.pool.Exec(ctx, `
		UPDATE venue_orders
		   SET status = $2, state = $3, updated_at = now()
		 WHERE order_id = $1 AND state = $4
	`, st.GetOrderId(), int32(st.GetStatus()), blob, at.state)
	if uerr != nil {
		return false, fmt.Errorf("conditionally record order %s: %w", st.GetOrderId(), uerr)
	}
	return tag.RowsAffected() == 1, nil
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

// ErrTerminalNotRedispatched is returned when Dispatch is asked to record an
// order this adapter's view already holds at a TERMINAL status. See Dispatch.
//
// IT IS THE REFUSAL OF A PLACEMENT, not a bookkeeping complaint, and that is why
// it is a sentinel rather than a plain error: Server.Execute matches it to answer
// the OMS codes.AlreadyExists — distinct from the codes.Internal any other
// failure to record gets — and the exchange is never called. Update returns a
// decide error UNWRAPPED for exactly this reason.
var ErrTerminalNotRedispatched = errors.New("orderview: refusing to work an order the venue has already finished")

// ErrContended is returned when Update could not land its write inside
// UpdateAttempts rounds because another writer changed the order every time.
//
// IT IS A REFUSAL, NOT A LOST UPDATE, and that distinction is the whole reason
// the bound is allowed to exist: the caller is told the write did not happen,
// loudly, instead of a merge built on a stale read being applied quietly. On the
// cancel path the RPC still succeeds and the view stays non-terminal, so the
// healing watchdog re-reads venue truth on its next pass; on the fill path the
// ingester reports it and the reconciler heals the same way.
var ErrContended = errors.New("orderview: gave up on a conditional write after repeated concurrent changes to the same order")

// UpdateAttempts bounds Update's retry loop.
//
// A BOUND RATHER THAN A SPIN, because the loop's exit condition is another
// writer losing interest. Every round is a real store round-trip, and the callers
// here are a websocket read loop and a gRPC handler the OMS is waiting on: an
// unbounded retry turns one hot order into an unbounded stall in the one process
// holding the exchange session.
//
// THE NUMBER IS MEASURED, NOT PICKED. TestConcurrentUpdatesLoseNoWrites is that
// measurement: two writers saturating ONE order — forty updates back to back with
// no gap, which is far past anything the venue path produces, where per order
// there is one ingester goroutine and the occasional cancel. The average round
// count there is 1.4, and the tail is what matters: over forty runs of the
// saturated workload the worst single Update took 21 rounds against Memory and
// Postgres alike. There is no backoff between rounds, so the tail is a real
// losing streak rather than a queue; 64 is three times the worst observed, and
// leaves the bound reachable only by contention an order of magnitude past
// saturation. Reaching it is ErrContended — a refusal, never a silent overwrite.
const UpdateAttempts = 64

// Decide is Update's read-decide-write body: given what the view currently holds
// (cur, and whether there was an entry at all), it returns the state to write, or
// (nil, nil) to write nothing.
//
// It is called AGAIN on every retry, against the re-read value, and that is the
// property the whole design turns on. A conditional write that lost the race must
// not re-apply the decision it made against the old value — for a confirmed
// cancel, re-deciding against a view that has since gone terminal means keeping
// the venue's verdict and writing NOTHING, which is exactly what would be lost by
// retrying blindly.
type Decide func(cur *orderpb.OrderState, found bool) (*orderpb.OrderState, error)

// Update is the store's atomic read-modify-write: it reads one order, asks decide
// what to write, and applies it only if nothing changed underneath — re-reading
// and re-deciding when something did (#934).
//
// WHY THIS IS NOT OPTIONAL SUGAR OVER Get + RecordIf. Both callers are a merge
// onto what the view already holds, and both had the same defect: a fill landing
// between the read and the write was overwritten by a merge built on the pre-fill
// read, and the exchange's own report of what it filled was gone from a table
// that is never pruned. That is an attribution loss on the capital path — "what
// the venue returned, what filled" stops being answerable — and it is invisible
// to -race, because it is a correct-looking interleaving and not a data race.
//
// A decide error is returned UNWRAPPED so callers can still match the package's
// sentinels with errors.Is.
func Update(ctx context.Context, store Store, orderID string, decide Decide) error {
	if orderID == "" {
		return errors.New("orderview: cannot update an order with empty order_id")
	}
	for range UpdateAttempts {
		cur, rev, found, err := store.Get(ctx, orderID)
		if err != nil {
			return fmt.Errorf("could not establish what the view already holds for order %s: %w", orderID, err)
		}
		next, derr := decide(cur, found)
		if derr != nil {
			return derr
		}
		if next == nil {
			return nil
		}
		applied, werr := store.RecordIf(ctx, next, rev)
		if werr != nil {
			return fmt.Errorf("record order %s: %w", orderID, werr)
		}
		if applied {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrContended, orderID)
}

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
// CONCURRENCY, STATED RATHER THAN IMPLIED. This read-modify-write IS atomic
// (#934): it goes through Update, which applies the merge only while the view
// still holds the exact value the merge was built on, and re-reads and
// re-decides when it does not. The condition is the VALUE, not its status — a
// status compare-and-set would have succeeded against the ordinary case of a
// partially filling order taking another fill, PARTIALLY_FILLED over
// PARTIALLY_FILLED with a larger filled_quantity, and lost the quantity anyway.
// It cannot fabricate a status either way: every value written came from a venue
// report.
//
// THE RESIDUALS, AND THERE ARE TWO. They are different in kind, so they are not
// stated as one.
//
//   - THIS CALL can still decline to write. Update gives up after
//     UpdateAttempts contended rounds and returns ErrContended, which
//     Seam.Progressed reports to onErr. That is not a lost update: nothing was
//     overwritten, the order is still non-terminal, so Open keeps returning it
//     and the reconciler re-reads venue truth on the next pass.
//   - ANOTHER WRITER can still write this entry, and as of #944 it can no longer
//     overwrite what this call recorded. Server.Execute's seed/refresh was a
//     plain Store.Record, so a re-dispatch of a PARTIALLY_FILLED order wrote the
//     OMS's OrderState over the quantities this call had folded in; it now goes
//     through Dispatch, which is this same Update and carries the status, the
//     filled and leaves quantities, the average fill price and the as-of forward
//     from whatever the view holds at the instant it lands. Store.Record is
//     still an unconditional upsert — deliberately, because a redelivered
//     ExecuteRequest must refresh rather than be rejected — but nothing in
//     production calls it any more, and a new write path that reached for it
//     rather than Update would reopen exactly this.
//
// -race does not run on the usual Windows box (no cgo) and would not have found
// this anyway — a lost update is a correct-looking interleaving, not a data race,
// and only a deterministic test finds it. There is one, in this package and in
// internal/venueadapter/server.
func Progress(ctx context.Context, store Store, reported *orderpb.OrderState) error {
	id := reported.GetOrderId()
	if id == "" {
		return errors.New("orderview: cannot record progress for an order with empty order_id")
	}
	next := reported.GetStatus()
	if next == orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED {
		return fmt.Errorf("%w: order %s", ErrUnmappedStatus, id)
	}
	return Update(ctx, store, id, func(cur *orderpb.OrderState, found bool) (*orderpb.OrderState, error) {
		if !found {
			return nil, fmt.Errorf("%w: %s", ErrNotInView, id)
		}
		if Terminal(cur.GetStatus()) && !Terminal(next) {
			return nil, fmt.Errorf("%w: %s is %v and the venue now reports %v", ErrTerminalNotReopened, id, cur.GetStatus(), next)
		}
		merged, mok := proto.Clone(cur).(*orderpb.OrderState)
		if !mok {
			return nil, fmt.Errorf("orderview: order %s did not clone", id)
		}
		// The venue's own observations onto the view's terms. Dispatch applies
		// the SAME projection in the other direction (#944), so the field set the
		// exchange is authoritative for is written once rather than twice.
		carryVenueObserved(merged, reported)
		return merged, nil
	})
}

// carryVenueObserved copies onto dst the fields the EXCHANGE is the authority on,
// where src has them.
//
// IT EXISTS ONCE BECAUSE IT IS APPLIED IN BOTH DIRECTIONS. Progress folds a venue
// report onto the view's terms; Dispatch folds the OMS's terms onto the view's
// venue observations (#944). Those are the same partition of the message read
// from opposite ends, and written out twice they would drift: a field the
// exchange becomes authoritative for would be added to one merge and not the
// other, and the writer that missed it would go back to overwriting the
// exchange's own number — the defect both #921 and #944 are. The partition is
// stated once, here.
//
// EACH FIELD IS GUARDED ON src HAVING SET IT. A nil would otherwise blank a value
// the destination already holds, and a blanked filled_quantity reads downstream
// as an order that traded nothing — the same sentence, whichever direction the
// merge runs in. UNSPECIFIED is the status's nil: it is "we cannot read what was
// said", never "no status", so it never overwrites a status somebody could read.
func carryVenueObserved(dst, src *orderpb.OrderState) {
	if s := src.GetStatus(); s != orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED {
		dst.Status = s
	}
	if q := src.GetFilledQuantity(); q != nil {
		dst.FilledQuantity = q
	}
	if q := src.GetLeavesQuantity(); q != nil {
		dst.LeavesQuantity = q
	}
	if p := src.GetAverageFillPrice(); p != nil {
		dst.AverageFillPrice = p
	}
	if t := src.GetAsOf(); t != nil {
		dst.AsOf = t
	}
}

// Dispatch records an order this adapter has been ASKED TO WORK — the OMS's own
// OrderState, off a venue.v1.ExecuteRequest — without letting that copy overwrite
// what the venue itself has already observed about the order (#944), and REFUSES
// with ErrTerminalNotRedispatched when the view already holds the order finished,
// so that the terminal check and the write are one operation (#947).
//
// IT IS Progress READ FROM THE OTHER END, and that symmetry is the design. The
// OMS is the authority on an order's TERMS: it is the only party that knows the
// limit price, the GTD expiry, the parent, the leverage, the margin mode, the
// venue account. The EXCHANGE is the authority on what happened to it: the
// status, the filled and leaves quantities, the average fill price, the as-of.
// Progress writes the second set onto the first; this writes the first onto the
// second. Neither writer can take a field the other owns, and the partition is
// carryVenueObserved, stated once for both.
//
// WHAT IT REPLACED, AND WHY THAT WAS WRONG. Server.Execute called Store.Record —
// an unconditional upsert — so a re-dispatch of an order the view held
// PARTIALLY_FILLED wrote the OMS's OrderState straight over the venue's own
// filled_quantity and leaves_quantity. The OMS's copy is BEHIND the venue by
// construction on this path: the adapter learns of a fill on the exchange's
// websocket and the OMS learns of it from the adapter, so the state arriving on
// an ExecuteRequest cannot be fresher than the one already in the view. It is the
// same erasure #921 and #934 closed on the cancel path, through the one writer
// neither of them changed.
//
// THE BOUND, STATED RATHER THAN INFLATED. The status the old write left was the
// OMS's (ROUTED), which is NOT terminal, so Open kept returning the order and the
// healing watchdog re-queried the exchange and healed the quantities on its next
// pass. No fill was lost, and no FACT was wrong — both user-data ingesters build
// the healed OrderState they publish from the REPORT's quantities, never from the
// view. What was not self-correcting is venue_orders, which is never pruned:
// between the overwrite and the next reconciliation pass this adapter's answer to
// "what did the venue report filled" was the OMS's stale guess, and Seam.Lookup
// hands that answer to the user-data ingester enriching any execution report that
// arrives in the window. That is the attribution the platform owes every order,
// and it is what this closes.
//
// A REFRESH IS STILL A REFRESH, which is why this merges rather than declining to
// write. Leaving the entry alone whenever one exists is simpler and drops the
// legitimate reason a re-dispatch writes at all: a redelivered or re-worked
// ExecuteRequest carries the order's terms as the OMS holds them NOW, and an
// adapter working an order off terms it has since been corrected on enriches its
// fills wrong. Store.Record's seed/refresh contract (#905) is unchanged and
// unconditional; what changes is that the refresh no longer reaches five fields
// the OMS is not the authority on.
//
// A TERMINAL PRIOR REFUSES THE WHOLE DISPATCH, and that refusal lives HERE
// rather than in the caller (#947). Server.Execute used to check it with its own
// Get on the line above and throw the value away, which made the guard a
// check-then-act: an execution report landing between that read and the
// placement left the order FILLED in the view AFTER the guard had waved it
// through, and the adapter placed it again at the exchange. The exchange dedups
// a resubmitted client order id only WHILE the original is open — once it has
// filled, the id is free again, and that is precisely the case this refuses. In
// here the read, the refusal and the write are one operation and the window does
// not exist: the decide that refuses is the decide the conditional write is made
// against, and it is re-run against the re-read value on every contended round,
// so a fill that lands mid-flight is seen by the retry rather than missed by it.
//
// THE VERDICT IS ALSO NOT REOPENED, which used to be this function's whole
// answer to a terminal prior (#944): the merge carried the venue's status and
// quantities forward, so the record survived even though the placement did not.
// Refusing subsumes that — nothing is written at all — and the record is
// asserted afterwards either way, because "the entry is untouched" is the half
// an operator reads.
//
// THE COST OF REFUSING, TAKEN DELIBERATELY. The caller gets an error for an order
// that did in fact finish, and past the venue adapter that is a quarantine or a
// DLQ entry — a human looking at something that needs no repair. The alternative
// is a second trade with the fund's money. Fail closed.
//
// THE READ AND THE WRITE ARE ONE OPERATION (#934), through the same Update both
// other writers go through, for the same reason and with the same re-decide: a
// fill landing between the read and the write must not be overwritten by a merge
// built on the pre-fill read, and a retry that re-applied the old decision would
// do exactly that. The condition is the VALUE, not its status — a re-dispatch
// over a partially filling order is the case a status compare-and-set passes
// while losing the quantity.
//
// NO ENTRY MEANS SEED, and the seed goes through the same conditional write. Two
// dispatches racing to seed one order therefore cannot both win: the second finds
// the row present and merges onto it rather than clobbering it.
//
// THE RESIDUAL. Update gives up after UpdateAttempts contended rounds and returns
// ErrContended — a refusal, never a silent overwrite. Server.Execute treats that
// like any other failure to record and does NOT place the order: working an order
// this adapter has no record of leaves its fills unenrichable and invisible to
// the reconciler, which is the trade Execute already took for a failed Record.
func Dispatch(ctx context.Context, store Store, requested *orderpb.OrderState) error {
	id := requested.GetOrderId()
	if id == "" {
		return errors.New("orderview: cannot record a dispatch for an order with empty order_id")
	}
	return Update(ctx, store, id, func(cur *orderpb.OrderState, found bool) (*orderpb.OrderState, error) {
		if found && Terminal(cur.GetStatus()) {
			return nil, fmt.Errorf("%w: %s is already %v in this adapter's view", ErrTerminalNotRedispatched, id, cur.GetStatus())
		}
		next, ok := proto.Clone(requested).(*orderpb.OrderState)
		if !ok {
			return nil, fmt.Errorf("orderview: order %s did not clone", id)
		}
		if found {
			carryVenueObserved(next, cur)
		}
		return next, nil
	})
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
	st, _, ok, err := s.store.Get(context.Background(), orderID)
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
