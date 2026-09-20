package execution

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"sync"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// Close tracking preserves uncertainty until a venue confirms a terminal state.
// Timeouts authorize another observation, never an offsetting trade.

// CloseIntent records a close command the OMS dispatched to a venue but has not
// yet seen confirmed terminal. The reconciler's healing loop watches it.
//
// SweepSide/Leaves retain legacy exposure context. They never authorize a trade;
// any residual execution must pass normal OMS admission with fresh exposure.
type CloseIntent struct {
	// OrderID is the order being closed — the venue clOrdId used to query it.
	OrderID string
	// InstrumentID is the instrument, mapped to the venue symbol on query/sweep.
	InstrumentID string
	// SweepSide is historical exposure context, not execution authority.
	SweepSide orderpb.Side
	// Leaves is historical residual context, never a market-order quantity.
	Leaves *big.Rat
	// RequestedAt is when the close was dispatched — the timeout is measured from
	// here.
	RequestedAt time.Time
}

// Reasons a tracked close cannot be healed — the label the watchdog counts a
// dropped intent under, and the only two ways an intent reaches the exchange
// query as a question that cannot be asked.
//
// THESE ARE NOT THE SAME EVENT AND MUST NEVER SHARE A COUNTER (#1036). An intent
// with no instrument is MALFORMED: some writer recorded a close the watchdog was
// never able to attempt, and the fix is in the writer. An intent whose instrument
// this venue has no symbol for is a CONFIGURATION answer: the order is not
// tradeable here, which is a statement about the symbol map. Collapsing them is
// what hid the defect — every close the venue adapter tracked failed the symbol
// lookup with an empty id and was dropped as "untradeable here".
const (
	// CloseDropNoInstrument: the intent named no instrument at all.
	CloseDropNoInstrument = "no_instrument"
	// CloseDropUnmappedSymbol: the instrument is not in this venue's symbol map.
	CloseDropUnmappedSymbol = "unmapped_symbol"
)

// Unhealable reports why the healing watchdog could not even form a venue query
// for this intent, or "" when it can.
//
// It is on the intent rather than in each reconciler because BOTH reconcilers and
// the ONE production writer must agree on what a healable close is: the writer
// refuses to dispatch a close it could not later resolve, and the watchdogs count
// what reaches them anyway. Two copies of that rule is how the first one drifts.
//
// InstrumentID is the whole check today, and it is enough: it is the field both
// watchdogs map to a venue symbol before they can ask the exchange anything, so
// an empty one makes the query unformable. OrderID is not checked here because
// the RPC and the OMS both refuse an empty one earlier, where the caller is still
// nameable.
func (ci CloseIntent) Unhealable() string {
	if ci.InstrumentID == "" {
		return CloseDropNoInstrument
	}
	return ""
}

// PendingCloses is the reconciler's read/resolve seam over in-flight closes.
// Production binds this to the adapter's tenant-scoped Postgres store.
type PendingCloses interface {
	// DueCloses claims at most 100 due observations and schedules their next retry.
	DueCloses(ctx context.Context, now time.Time, timeout time.Duration) ([]CloseIntent, error)
	// Resolve removes only a close confirmed terminal by the venue.
	Resolve(ctx context.Context, orderID string) error
}

// CloseTracker is the WRITE half of the in-flight-close seam — the OMS's side.
// It records a close the moment the OMS decides to dispatch it and drops it once
// the venue confirms; PendingCloses is the reconciler's read half over the same
// registry. Both are satisfied by *CloseRegistry.
type CloseTracker interface {
	// Track records a close about to be dispatched. It MUST be called before the
	// venue call, never after: the window the healing seam exists to cover is
	// precisely the one where the dispatch hangs, times out ambiguously, or the
	// process dies mid-call. A close tracked after a successful ack covers nothing.
	Track(ctx context.Context, ci CloseIntent) error
	// Resolve drops a close the venue confirmed terminal — nothing left to heal.
	Resolve(ctx context.Context, orderID string) error
}

// CloseRegistry is an in-memory PendingCloses the OMS writes when it dispatches a
// close and the reconciler drains as it heals. Safe for concurrent use.
type CloseRegistry struct {
	mu       sync.Mutex
	pending  map[string]CloseIntent
	next     map[string]time.Time
	attempts map[string]int
}

// NewCloseRegistry returns an empty registry.
func NewCloseRegistry() *CloseRegistry {
	return &CloseRegistry{pending: map[string]CloseIntent{}, next: map[string]time.Time{}, attempts: map[string]int{}}
}

// Track records an intent once; redelivery preserves its identity and retry clock.
func (r *CloseRegistry) Track(ctx context.Context, ci CloseIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.pending[ci.OrderID]; ok {
		if existing.InstrumentID != ci.InstrumentID {
			return errors.New("close intent identity conflict")
		}
		return nil
	}
	if ci.RequestedAt.IsZero() {
		ci.RequestedAt = time.Now().UTC()
	}
	if ci.Leaves != nil {
		ci.Leaves = new(big.Rat).Set(ci.Leaves)
	}
	r.pending[ci.OrderID] = ci
	return nil
}

// DueCloses returns the closes past the timeout.
func (r *CloseRegistry) DueCloses(ctx context.Context, now time.Time, timeout time.Duration) ([]CloseIntent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var due []CloseIntent
	for _, ci := range r.pending {
		if now.Sub(ci.RequestedAt) >= timeout && !now.Before(r.next[ci.OrderID]) {
			due = append(due, ci)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].RequestedAt.Equal(due[j].RequestedAt) {
			return due[i].OrderID < due[j].OrderID
		}
		return due[i].RequestedAt.Before(due[j].RequestedAt)
	})
	if len(due) > 100 {
		due = due[:100]
	}
	for i := range due {
		ci := &due[i]
		if r.attempts[ci.OrderID] < 7 {
			r.attempts[ci.OrderID]++
		}
		r.next[ci.OrderID] = now.Add(CloseRetryDelay(r.attempts[ci.OrderID]))
		if ci.Leaves != nil {
			ci.Leaves = new(big.Rat).Set(ci.Leaves)
		}
	}
	return due, nil
}

// Resolve removes a close.
func (r *CloseRegistry) Resolve(ctx context.Context, orderID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, orderID)
	delete(r.next, orderID)
	delete(r.attempts, orderID)
	return nil
}

// Len reports the number of in-flight closes (observability).
func (r *CloseRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

var (
	_ PendingCloses = (*CloseRegistry)(nil)
	_ CloseTracker  = (*CloseRegistry)(nil)
)

// CloseRetryDelay caps the retry rate without ever expiring unresolved ownership.
func CloseRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		return time.Minute
	}
	return time.Duration(1<<(attempt-1)) * time.Second
}
