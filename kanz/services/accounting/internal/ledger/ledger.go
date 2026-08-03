// Package ledger is the event-sourced investment book-of-record (IBOR-01b): it
// folds OMS-01 fills and cash movements into a point-in-time book of positions
// and cash. The journal of LedgerEntry events is the source of truth; the Book is
// their fold, so the same journal always replays to the same book (and the same
// NAV) — the PERS-01 snapshot/replay stance applied to accounting state.
//
// # Double entry
//
// Every entry carries a signed position leg (quantity at price) and/or a signed
// cash leg. A trade is both: a BUY adds the position and removes cash; a cash
// movement is a pure cash leg. The book never invents the cash side — the caller
// (the fill adapter, a corporate-action processor) computes it, so the fold is a
// faithful double-entry accumulator.
//
// # Bitemporal
//
// Each entry has an effective time (when it economically happened) and a
// knowledge time (when the book learned of it). ReplayAsOf reconstructs the book
// as it was known at a (effective, knowledge) point, so a late-arriving entry
// restates history without destroying what was previously reported — the
// knowledge-time correctness IBOR-01c requires.
package ledger

import (
	"math/big"
	"sort"
	"time"
)

// EntryType classifies a journal entry by what produced it (mirrors
// accounting.v1.EntryType).
type EntryType int

const (
	EntryUnspecified EntryType = iota
	EntryTrade
	EntryCash
	EntryFee
	EntryCorporateAction
	EntryAccrual
)

// Event is one immutable journal entry — the internal working shape behind
// accounting.v1.LedgerEntry. Money and quantity are exact (*big.Rat); the journal
// is append-only, so a correction is a new offsetting Event, never a mutation.
type Event struct {
	EntryID     string
	PortfolioID string
	// VenueAccountID is the EXCHANGE ACCOUNT whose collateral this entry moved.
	//
	// The ledger has always kept cash per PORTFOLIO. An exchange does not: it margins
	// and LIQUIDATES per ACCOUNT. If two portfolios settle into one account, a
	// per-portfolio ledger reports cash that a liquidation in the other portfolio has
	// already consumed — books that are not imprecise but WRONG, in the direction that
	// loses money. So an entry records the account it settled against, and the engine
	// refuses to write it into an account the transaction did not declare.
	//
	// Set from the Fill (the account that actually executed). Empty for entries that
	// touch no exchange account: a manual cash movement, a corporate action.
	VenueAccountID string
	Type           EntryType
	InstrumentID   string

	// Quantity is the signed position change; Price the per-unit price. Both nil
	// for a pure cash entry.
	Quantity *big.Rat
	Price    *big.Rat

	// Cash is the signed cash leg (+ increases cash) in CashCurrency. Nil ⇒ no
	// cash effect.
	Cash         *big.Rat
	CashCurrency string

	// Action carries corporate-action parameters for an EntryCorporateAction
	// entry. Its effects (split scaling, dividend/coupon cash, merger conversion)
	// are evaluated against the POSITION-AT-EFFECTIVE-TIME during the fold, not
	// precomputed — so a restatement that changes the holding at the ex-date
	// changes the action's cash/position effect too (knowledge-time correctness).
	Action *Action

	// Effective is the domain time the event happened; Knowledge is when the book
	// learned of it. A later Knowledge with an earlier Effective is a restatement.
	Effective time.Time
	Knowledge time.Time

	SourceRef string
}

// CorpActKind is the kind of corporate action an Action describes.
type CorpActKind int

const (
	CorpActUnspecified CorpActKind = iota
	CorpActSplit
	CorpActDividend
	CorpActMerger
	CorpActCoupon
)

// Action is the parameters of a corporate action, applied at fold time against
// the affected position. The package corpact builds these from a CorporateAction.
type Action struct {
	Kind CorpActKind

	// Ratio is the share-conversion factor for a SPLIT (2.0 = 2:1) or a MERGER
	// exchange (target shares per source share).
	Ratio *big.Rat

	// PerUnit is the cash factor per unit held: dividend-per-share (DIVIDEND),
	// coupon cash per unit of face (COUPON), or cash-per-share (MERGER).
	PerUnit *big.Rat

	// Target is the instrument received in a MERGER.
	Target string

	// Currency stamps any cash leg the action produces.
	Currency string
}

// Position is the running state of one holding: signed quantity, the
// non-negative average cost of the open lot, and cumulative realized P&L.
type Position struct {
	Qty      *big.Rat
	AvgCost  *big.Rat
	Realized *big.Rat
}

func newPosition() *Position {
	return &Position{Qty: new(big.Rat), AvgCost: new(big.Rat), Realized: new(big.Rat)}
}

// Book is the folded state of one portfolio: positions by instrument, cash by
// currency, and accrued income by currency (earned-not-received, carried in NAV).
// It is the fold of the journal — replayable and snapshot-restorable.
type Book struct {
	PortfolioID string
	Positions   map[string]*Position
	Cash        map[string]*big.Rat
	Accrued     map[string]*big.Rat
	seen        map[string]bool
	// maxEffective is the latest effective_time this book has folded. It is the
	// fence a snapshot carries into Snapshot.MaxEffective: the fold is
	// order-sensitive (weighted-average cost realizes P&L in sequence), so
	// resuming from a checkpoint is only equal to a full replay while every
	// entry appended afterwards is effective at or after everything already in
	// it. See MaterializeCurrent, which refuses the checkpoint otherwise.
	maxEffective time.Time
}

// NewBook returns an empty book for a portfolio.
func NewBook(portfolioID string) *Book {
	return &Book{
		PortfolioID: portfolioID,
		Positions:   make(map[string]*Position),
		Cash:        make(map[string]*big.Rat),
		Accrued:     make(map[string]*big.Rat),
		seen:        make(map[string]bool),
	}
}

// Apply folds one entry into the book. It is idempotent on EntryID — a replay or
// redelivery of the same entry is a no-op, so the book is exactly-once over the
// at-least-once journal. An entry for another portfolio is ignored.
func (b *Book) Apply(e *Event) {
	if e == nil || e.EntryID == "" || (e.PortfolioID != "" && e.PortfolioID != b.PortfolioID) {
		return
	}
	if b.seen[e.EntryID] {
		return
	}
	b.seen[e.EntryID] = true
	if e.Effective.After(b.maxEffective) {
		b.maxEffective = e.Effective
	}

	if e.Type == EntryCorporateAction && e.Action != nil {
		b.foldCorpAct(e.InstrumentID, e.Action)
		return
	}

	if e.InstrumentID != "" && e.Quantity != nil && e.Quantity.Sign() != 0 {
		b.foldPosition(e.InstrumentID, e.Quantity, orZero(e.Price))
	}
	if e.Cash != nil && e.Cash.Sign() != 0 {
		ccy := e.CashCurrency
		if e.Type == EntryAccrual {
			add(b.Accrued, ccy, e.Cash)
		} else {
			add(b.Cash, ccy, e.Cash)
		}
	}
}

// foldCorpAct applies a corporate action against the position in instrument as it
// stands at this point in the fold. Splits scale the lot (conserving market
// value); dividends/coupons pay cash on the held quantity; a merger converts the
// holding into the target instrument, carrying the cost basis and paying any cash.
func (b *Book) foldCorpAct(instrument string, a *Action) {
	l := b.Positions[instrument]
	if l == nil || l.Qty.Sign() == 0 {
		return // nothing held at the ex-date: the action has no effect
	}
	switch a.Kind {
	case CorpActSplit:
		if a.Ratio == nil || a.Ratio.Sign() <= 0 {
			return
		}
		// qty × ratio, avg ÷ ratio → market value (qty × price) is conserved when
		// the price also divides by ratio.
		l.Qty = new(big.Rat).Mul(l.Qty, a.Ratio)
		l.AvgCost = new(big.Rat).Quo(l.AvgCost, a.Ratio)

	case CorpActDividend, CorpActCoupon:
		if a.PerUnit == nil {
			return
		}
		// Cash on the held quantity (absolute: a long receives, a short pays).
		cash := new(big.Rat).Mul(l.Qty, a.PerUnit)
		add(b.Cash, a.Currency, cash)

	case CorpActMerger:
		if a.Target == "" || a.Ratio == nil {
			return
		}
		q := new(big.Rat).Set(l.Qty)
		basis := new(big.Rat).Mul(new(big.Rat).Abs(q), l.AvgCost) // cost carried over
		// Source position closes with no realized P&L — the basis transfers.
		l.Qty = new(big.Rat)
		l.AvgCost = new(big.Rat)
		newQty := new(big.Rat).Mul(q, a.Ratio)
		b.foldContribution(a.Target, newQty, basis)
		if a.PerUnit != nil && a.PerUnit.Sign() != 0 {
			add(b.Cash, a.Currency, new(big.Rat).Mul(q, a.PerUnit))
		}
	}
}

// foldContribution adds a quantity carrying a given total cost basis into a
// holding, weighted-averaging with any existing lot. Used by a merger to open the
// target position at the source's carried basis (avg = totalCost / |qty|).
func (b *Book) foldContribution(instrument string, qty, totalCost *big.Rat) {
	if qty.Sign() == 0 {
		return
	}
	avg := new(big.Rat).Quo(new(big.Rat).Abs(totalCost), new(big.Rat).Abs(qty))
	b.foldPosition(instrument, qty, avg)
}

// foldPosition applies a signed quantity at price using weighted-average-cost
// accounting, booking realized P&L on the closed portion (the OMS position
// projector's accounting, kept identical so the IBOR and the OMS book agree).
func (b *Book) foldPosition(instrument string, signed, price *big.Rat) {
	l := b.Positions[instrument]
	if l == nil {
		l = newPosition()
		b.Positions[instrument] = l
	}
	cur := l.Qty.Sign()
	add := signed.Sign()

	// Flat or same-direction: increase the position, re-average the cost.
	if cur == 0 || cur == add {
		oldAbs := new(big.Rat).Abs(l.Qty)
		addAbs := new(big.Rat).Abs(signed)
		newQty := new(big.Rat).Add(l.Qty, signed)
		newAbs := new(big.Rat).Abs(newQty)
		// Zero divisor ⇒ big.Rat.Quo PANICS. DEFENSE IN DEPTH, NOT A LIVE BUG:
		// unlike the OMS position fold this mirrors, every caller here already
		// screens a zero quantity out — Apply at `e.Quantity.Sign() != 0`, and
		// foldContribution at `qty.Sign() == 0`. In the same-direction arm newAbs
		// can only be zero when BOTH the existing lot and the incoming quantity are
		// zero, so today this is unreachable.
		//
		// It is guarded anyway because the divisor is derived rather than validated
		// here, the failure mode is a panic rather than a wrong number, and this
		// function is a hand-copy of the OMS fold — where the same line WAS
		// reachable and did crash-loop the estate (#217). Two copies of one
		// calculation is the standing risk; the next edit to either should not have
		// to rediscover which one had the guard.
		//
		// Flat is a real state: a holding that nets to zero has no basis.
		if newAbs.Sign() == 0 {
			l.AvgCost = new(big.Rat)
			l.Qty = newQty
			return
		}
		cost := new(big.Rat).Mul(oldAbs, l.AvgCost)
		cost.Add(cost, new(big.Rat).Mul(addAbs, price))
		l.AvgCost = new(big.Rat).Quo(cost, newAbs)
		l.Qty = newQty
		return
	}

	// Opposite direction: realize P&L on the closed portion.
	closeAbs := new(big.Rat).Abs(signed)
	openAbs := new(big.Rat).Abs(l.Qty)
	if closeAbs.Cmp(openAbs) > 0 {
		closeAbs = openAbs
	}
	pnl := new(big.Rat).Mul(closeAbs, new(big.Rat).Sub(price, l.AvgCost))
	if cur < 0 {
		pnl.Neg(pnl)
	}
	l.Realized.Add(l.Realized, pnl)

	newQty := new(big.Rat).Add(l.Qty, signed)
	switch newQty.Sign() {
	case 0:
		l.Qty = new(big.Rat)
		l.AvgCost = new(big.Rat)
	default:
		if newQty.Sign() != cur {
			l.AvgCost = new(big.Rat).Set(price) // crossed zero: a fresh lot
		}
		l.Qty = newQty
	}
}

// CashBalance returns the cash held in a currency (zero if none).
func (b *Book) CashBalance(currency string) *big.Rat {
	if v := b.Cash[currency]; v != nil {
		return new(big.Rat).Set(v)
	}
	return new(big.Rat)
}

// AccruedBalance returns the accrued income in a currency (zero if none).
func (b *Book) AccruedBalance(currency string) *big.Rat {
	if v := b.Accrued[currency]; v != nil {
		return new(big.Rat).Set(v)
	}
	return new(big.Rat)
}

// Replay folds a journal into a fresh book in (effective, knowledge) order — the
// canonical reconstruction. Replaying the same journal yields the same book.
func Replay(portfolioID string, events []*Event) *Book {
	b := NewBook(portfolioID)
	for _, e := range sortedFor(events, time.Time{}, time.Time{}, false) {
		b.Apply(e)
	}
	return b
}

// ReplayAsOf reconstructs the book as it was known at knowledgeAsOf for economic
// effects up to effectiveAsOf — the bitemporal point-in-time read. A zero time on
// either axis means "no bound on that axis". This is what makes a restatement
// knowledge-time correct: a read as-of an early knowledge time never sees an entry
// the book had not yet learned of.
func ReplayAsOf(portfolioID string, events []*Event, effectiveAsOf, knowledgeAsOf time.Time) *Book {
	b := NewBook(portfolioID)
	for _, e := range sortedFor(events, effectiveAsOf, knowledgeAsOf, true) {
		b.Apply(e)
	}
	return b
}

// sortedFor returns the events, optionally filtered to those at or before the
// effective/knowledge bounds, in deterministic fold order (effective, then
// knowledge, then entry id).
func sortedFor(events []*Event, effBound, knowBound time.Time, filter bool) []*Event {
	out := make([]*Event, 0, len(events))
	for _, e := range events {
		if filter {
			if !effBound.IsZero() && e.Effective.After(effBound) {
				continue
			}
			if !knowBound.IsZero() && e.Knowledge.After(knowBound) {
				continue
			}
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Effective.Equal(out[j].Effective) {
			return out[i].Effective.Before(out[j].Effective)
		}
		if !out[i].Knowledge.Equal(out[j].Knowledge) {
			return out[i].Knowledge.Before(out[j].Knowledge)
		}
		return out[i].EntryID < out[j].EntryID
	})
	return out
}

func add(m map[string]*big.Rat, key string, v *big.Rat) {
	cur := m[key]
	if cur == nil {
		cur = new(big.Rat)
		m[key] = cur
	}
	cur.Add(cur, v)
}

func orZero(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return r
}
