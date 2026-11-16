package projection

// THE RESIDENT HISTORY IS BOUNDED; THE DURABLE ONE IS NOT (#809).
//
// #988 bounded the BOOT with a fold checkpoint. What it deliberately left is the
// HEAP: account.execs grows by one entry per fill, account.orders by one revision
// per status transition (each carrying a full cloned OrderState), and
// account.seenFills by one key per fill — for the life of the process, on a
// projection that never forgets an account. A tv-sync pod holding a busy fund
// grows until it is OOM-killed, and the operator interface disappears exactly
// when an incident makes somebody want it.
//
// # What is dropped, and what is not
//
// ONLY MEMORY. tv_facts keeps every fact forever and nothing in this file
// touches it. That distinction is the whole design: #809's own recommendation
// was retention on the fact log, and #988 rejected it because the log is the
// record a rebuild is defined against — deleting a row makes the pre-retention
// view unrebuildable rather than merely unresident, and
// TestTheReplayWindowWasNotReintroduced fails the build if anybody adds it. A
// pod restarted with a longer TV_SYNC_RETENTION rebuilds the longer window out
// of the log; nothing has been destroyed.
//
// # Why the live book does not change when history leaves
//
// An evicted execution is folded into account.baseline on its way out, so
// live == baseline plus fold(execs) holds at every instant. Positions, average
// cost, realized and unrealized P&L, open-position count — every number the
// Broker API reports for NOW — are identical with retention on and off. That is
// what separates this from the windowed rebuild #809 proposed and #988 refused:
// a window changes the answer, a baseline changes only where the answer is
// computed from.
//
// # Where an answer genuinely stops existing, and why it is refused
//
// A fold is not invertible, so the baseline can say what the book IS and not
// what it WAS. A read carrying ?as_of= earlier than the account's retainedFrom
// therefore returns ErrBeforeRetention. Refusing is the point: a partial history
// folds perfectly well and produces a plausible number for a book the fund never
// had, which is the one failure this projection's determinism contract exists to
// make impossible. The fact log still holds the answer; nothing in this process
// can serve it, and saying so is the honest report.
//
// # Why the read path does not rebuild it from the log
//
// It would have to replay the tenant's whole history into RAM per request — an
// unbounded allocation on a route any operator can call with any date, which is
// this file's own failure mode moved from the fold path to the read path and put
// under external control. Serving history from the log is real work with a bound
// of its own (a paged, per-account, streaming read) and it is not this change.

import (
	"errors"
	"fmt"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// ErrBeforeRetention: an as-of read reached behind the resident window. The fact
// log still holds those facts; this process does not, and answering from the
// window it does hold would report a book the fund never had.
var ErrBeforeRetention = errors.New("tv-sync: as-of read is older than the resident history")

// rehydrateEvictEvery bounds the BOOT heap as well as the steady-state one.
//
// A POD WITH NO CHECKPOINT REPLAYS FROM SEQ 0, and that is a legitimate posture:
// a fresh deployment, or a truncated checkpoint table. Without this the replay
// would materialise the fund's entire history in RAM and only then trim it, so
// the one boot most likely to be OOM-killed would be the boot after an OOM kill.
// Evicting every N facts keeps the rebuild inside the bound the steady state has.
//
// The number is a batch size, not a policy: a pass reallocates the execution
// slice, so evicting per fact would make the replay quadratic and evicting once
// at the end would not bound the replay at all.
const rehydrateEvictEvery = 10_000

// WithRetention bounds how much folded history stays resident.
//
// A NON-POSITIVE WINDOW MEANS KEEP EVERYTHING, which is the pre-#809 behaviour
// and what every memory-only test gets. It is not a safe production posture and
// nothing here pretends otherwise: config.Load refuses a non-positive
// TV_SYNC_RETENTION outright, so the only way to run unbounded is to build a
// Projection without this option — which is what the tests do, and which
// TestTheResidentHistoryIsBounded keeps the composition root from doing.
func WithRetention(window time.Duration) Option {
	return func(p *Projection) { p.retention = window }
}

// Retention reports the configured window (zero means unbounded).
func (p *Projection) Retention() time.Duration { return p.retention }

// EvictionStats is what one retention pass dropped. Returned rather than logged
// so the composition root can export it: a retention loop that runs and evicts
// nothing looks exactly like one that is not running, and the second is the leak
// coming back.
type EvictionStats struct {
	Executions int
	Orders     int
	Fills      int
}

// Resident is what the projection currently holds — the three collections that
// grow with lifetime traffic. A flat line here is #809's "Verified when"
// (resident set flat against total history) measured rather than assumed.
type Resident struct {
	Executions int
	Orders     int
	Fills      int
}

// Evict drops folded history older than the retention window measured back from
// now. It is a no-op without a window.
func (p *Projection) Evict(now time.Time) EvictionStats {
	if p.retention <= 0 {
		return EvictionStats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.evictLocked(now.Add(-p.retention))
}

// evictLocked runs one pass over every account. Callers hold the write lock.
func (p *Projection) evictLocked(cutoff time.Time) EvictionStats {
	var st EvictionStats
	for _, byAcct := range p.accounts {
		for _, a := range byAcct {
			e, o, f := a.evict(cutoff)
			st.Executions += e
			st.Orders += o
			st.Fills += f
		}
	}
	return st
}

// evict drops this account's history at or before cutoff, folding what leaves
// into the baseline.
//
// THE EXECUTIONS ARE A PREFIX, and that is a fact about the fold rather than a
// convenience: knowledge time is stamped when a FACT is folded and a replay
// reuses the stored one in seq order, so it is non-decreasing down the slice. A
// prefix is therefore exactly "everything at or before cutoff", and
// baseline plus fold(the rest) is exactly the whole fold — which is why the live
// book is unchanged by this call.
//
// THE FILL-ID DEDUP SET LEAVES WITH THE EXECUTION IT BELONGS TO, never on a
// schedule of its own. That set is the dedup for the DUAL fill path — the
// synchronous venue response and the asynchronous websocket echo of the same
// fill — so an id dropped before its echo arrives is a fill folded twice, which
// doubles a position the fund does not hold. Tying it to the execution makes the
// dedup horizon the retention window itself: one number, floored in config,
// rather than two that can drift apart.
//
// AN ORDER IS DROPPED ONLY WHEN IT IS TERMINAL. A working or scheduled order is
// retained however old it is — it is the blotter an operator acts from, and an
// order resting at a venue vanishing from the screen while it can still fill is
// a capital-facing defect rather than a memory saving.
func (a *account) evict(cutoff time.Time) (execs, orders, fills int) {
	n := 0
	for n < len(a.execs) && !a.execs[n].knowledge.After(cutoff) {
		n++
	}
	if n > 0 {
		for _, e := range a.execs[:n] {
			applyExecution(a.baselinePos(e.instrument), e)
			delete(a.seenFills, e.fillID)
		}
		// A FRESH BACKING ARRAY, NOT A RESLICE. a.execs[n:] keeps the original
		// array — and every execution in it — reachable, so the heap this exists
		// to bound would not actually shrink: the leak would be invisible rather
		// than absent, which is worse than leaving it alone.
		a.execs = append(make([]execution, 0, len(a.execs)-n), a.execs[n:]...)
		execs, fills = n, n
	}

	for id, revs := range a.orders {
		last := revs[len(revs)-1]
		if !isTerminal(last.status) || last.knowledge.After(cutoff) {
			continue
		}
		delete(a.orders, id)
		orders++
	}

	// THE HORIZON MOVES ON ANY EVICTION, an order-only one included. Positions
	// stay exact through the baseline, but the ORDER and EXECUTION lists are now
	// windows — so one horizon covering every read is a claim a reader can check,
	// where four per-collection horizons would be four chances to compare the
	// wrong one.
	if (execs > 0 || orders > 0) && cutoff.After(a.retainedFrom) {
		a.retainedFrom = cutoff
	}
	return execs, orders, fills
}

// isTerminal reports whether an order can still change. A terminal order emits no
// further FACT, so dropping it loses nothing the projection could still learn —
// which is not true of a working one.
func isTerminal(s orderpb.OrderStatus) bool {
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

// Resident reports what is held right now, across every tenant this process
// folds.
func (p *Projection) Resident() Resident {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var r Resident
	for _, byAcct := range p.accounts {
		for _, a := range byAcct {
			r.Executions += len(a.execs)
			r.Fills += len(a.seenFills)
			for _, revs := range a.orders {
				r.Orders += len(revs)
			}
		}
	}
	return r
}

// RetainedFrom is the knowledge instant an account's resident history begins at,
// or the zero time when nothing has been dropped.
//
// IT IS PART OF THE ANSWER, NOT DIAGNOSTICS. The order and execution lists are
// windows once this is non-zero, and a client that is not told so reads a short
// list as a complete one — "this fund placed four orders" rather than "four
// orders since Tuesday". The Broker API returns it beside those two lists for
// exactly that reason, and does not return it beside positions or state, which
// the baseline keeps exact.
func (p *Projection) RetainedFrom(tenant, accountID string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	a := p.readAcct(tenant, accountID)
	if a == nil {
		return time.Time{}, false
	}
	return a.retainedFrom, true
}

// readableAcct resolves an account for a read and refuses one it cannot answer.
//
// THE REFUSAL LIVES HERE RATHER THAN IN THE HTTP HANDLER on purpose. Every read
// on this type would otherwise have to remember the horizon check, and the one
// that forgot would fold a partial history into a number indistinguishable from
// a correct one. Callers hold at least the read lock.
func (p *Projection) readableAcct(tenant, accountID string, asOf time.Time) (*account, error) {
	a := p.readAcct(tenant, accountID)
	if a == nil {
		return nil, ErrAccountNotFound
	}
	if !asOf.IsZero() && !a.retainedFrom.IsZero() && asOf.Before(a.retainedFrom) {
		return nil, fmt.Errorf("%w: asked as of %s, this pod holds history from %s (the fact log "+
			"still has it, nothing in memory does)",
			ErrBeforeRetention, asOf.UTC().Format(time.RFC3339), a.retainedFrom.UTC().Format(time.RFC3339))
	}
	return a, nil
}
