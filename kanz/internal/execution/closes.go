package execution

import (
	"math/big"
	"sync"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// The in-flight-close healing seam (the "In-Flight Certainty" mandate). A close
// command (a cancel, or an IOC market order that flattens a position) races the
// venue: the acknowledgement can hang. If it stays unconfirmed past a hard
// timeout the ledger must NOT freeze — the reconciler queries the venue and, if
// the order is stuck or the venue is unresponsive, force-clears it and sweeps the
// residual exposure with an aggressive market order, emitting the correcting
// FACTs. These seams are untagged so both exchange reconcilers share them; the
// concrete in-memory registry is the default (and test) implementation.

// CloseIntent records a close command the OMS dispatched to a venue but has not
// yet seen confirmed terminal. The reconciler's healing loop watches it.
//
// SweepSide/Leaves describe the exposure that is left OPEN if the close does not
// land, and the two kinds of close differ sharply here:
//
//   - Flattening an open position (an IOC market close): the residual is the part
//     of the position the close failed to flatten, so SweepSide is the close's own
//     side (SELL to flatten a long) and Leaves is its unfilled remainder. A stuck
//     close leaves us still exposed ⇒ sweep it.
//   - Cancelling a resting order: the order has not traded, so withdrawing it
//     opens nothing. There is NO residual exposure and the sweep must be
//     suppressed (SweepSide UNSPECIFIED / Leaves nil) — sweeping here would open
//     a brand-new position in the opposite direction, the exact fabrication the
//     healing seam exists to prevent. If the cancel raced a fill, the watchdog's
//     venue query returns it terminal and StateHealed carries that truth instead.
type CloseIntent struct {
	// OrderID is the order being closed — the venue clOrdId used to query it.
	OrderID string
	// InstrumentID is the instrument, mapped to the venue symbol on query/sweep.
	InstrumentID string
	// SweepSide is the side an aggressive market order must take to flatten the
	// residual exposure if the close is stuck. Sweep is skipped when SweepSide is
	// UNSPECIFIED — see the type doc for which closes carry one.
	SweepSide orderpb.Side
	// Leaves is the residual open quantity to sweep if the close is stuck. A
	// non-positive value means there is nothing to sweep (just force-clear).
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
// Bound to CloseRegistry in the composition root.
type PendingCloses interface {
	// DueCloses returns the closes whose RequestedAt is older than now-timeout.
	DueCloses(now time.Time, timeout time.Duration) []CloseIntent
	// Resolve removes a close once it has been healed (confirmed terminal or
	// force-cleared) so it is not processed again.
	Resolve(orderID string)
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
	Track(ci CloseIntent)
	// Resolve drops a close the venue confirmed terminal — nothing left to heal.
	Resolve(orderID string)
}

// CloseRegistry is an in-memory PendingCloses the OMS writes when it dispatches a
// close and the reconciler drains as it heals. Safe for concurrent use.
type CloseRegistry struct {
	mu      sync.Mutex
	pending map[string]CloseIntent
}

// NewCloseRegistry returns an empty registry.
func NewCloseRegistry() *CloseRegistry {
	return &CloseRegistry{pending: map[string]CloseIntent{}}
}

// Track records a dispatched close. A second Track for the same order refreshes
// its intent (e.g. a re-issued close) but preserves the original RequestedAt so
// the timeout is not reset by retries.
func (r *CloseRegistry) Track(ci CloseIntent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.pending[ci.OrderID]; ok && !existing.RequestedAt.IsZero() {
		ci.RequestedAt = existing.RequestedAt
	}
	if ci.RequestedAt.IsZero() {
		ci.RequestedAt = time.Now().UTC()
	}
	r.pending[ci.OrderID] = ci
}

// DueCloses returns the closes past the timeout.
func (r *CloseRegistry) DueCloses(now time.Time, timeout time.Duration) []CloseIntent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var due []CloseIntent
	for _, ci := range r.pending {
		if now.Sub(ci.RequestedAt) >= timeout {
			due = append(due, ci)
		}
	}
	return due
}

// Resolve removes a close.
func (r *CloseRegistry) Resolve(orderID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, orderID)
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
