package ledger

import (
	"math/big"
	"sort"
	"time"
)

// THE TRANSACTION GRAIN OF A CUSTODY COMPARISON (#1049).
//
// # What the fold alone cannot answer
//
// Book is a FOLD: it carries what the portfolio holds, not what produced it. A
// comparison against a custodian statement built only from it is netted by
// construction, and a netted comparison is blind to the internal composition of a
// correct-looking total. It cannot distinguish a genuine break from two
// offsetting errors of equal size, nor a wrongly-booked execution from a missing
// one when both touch the same instrument — twelve fills on one instrument, one
// of them booked against an execution the custodian never settled and one the
// custodian settled that the book never heard of, produce identical position and
// cash totals and reconcile CLEAN.
//
// So the comparison basis carries the executions behind the fold as well as the
// fold, and recon matches them by external reference against the statement's
// trade lines.
//
// # Why they come out of the same filter as the book
//
// foldForAccounts is the ONE answer to "which entries are in a custodian's
// comparison basis" (#1073). Deriving the executions anywhere else would be a
// second answer to that question, and a second answer is the shape #1073 was
// filed on: two rules disagreeing about the same entry, each documented as
// correct. They are returned by the same call, so an execution is in the
// transaction pass exactly when its entry is in the position pass — and the
// custodian scoping guard over that one hop covers both.
//
// # Why they are NOT on Book
//
// Book is snapshot-restorable and MaterializeCurrent folds only the journal TAIL
// onto a checkpoint. A book restored that way would carry the executions of the
// tail and none of the prefix, and a transaction pass over it would report every
// execution the checkpoint absorbed as one the custodian never saw — a screen of
// fabricated breaks, in the direction that buries the real one. The custody folds
// deliberately do not resume from a checkpoint (see MaterializeForAccounts), so
// the executions they return are complete BY CONSTRUCTION rather than by a flag
// somebody has to remember to read.

// Execution is one TRADE entry in the basis that carries an external reference —
// the book's side of a transaction-grain comparison.
//
// THE REFERENCE IS THE IDENTITY, and it is Event.SourceRef: the venue fill id,
// stamped by ledger.FromFill. It is what a custodian's trade line is matched
// against, so a trade carrying none cannot participate in the transaction pass at
// all — which is why the basis counts those separately rather than letting them
// vanish.
type Execution struct {
	// Ref is the external reference (Event.SourceRef) this execution is matched
	// on.
	Ref string
	// EntryID is the journal entry it came from, so a break can be traced back
	// into the book without re-reading the journal by reference.
	EntryID string

	InstrumentID string
	// Quantity is the signed position leg and Price the per-unit price; both nil
	// for a pure cash entry.
	Quantity *big.Rat
	Price    *big.Rat
	// Cash is the signed cash leg in CashCurrency.
	Cash         *big.Rat
	CashCurrency string
	// VenueAccountID is the exchange account it settled against — non-empty for
	// every execution here, because an entry carrying none is not in any
	// custodian's basis (see foldForAccounts).
	VenueAccountID string

	// TradeDate is when it economically happened (Event.Effective).
	// SettlementBasis and SettlementDate are the settlement axis #1043 put on the
	// journal: whether these legs have changed hands, and when.
	TradeDate       time.Time
	SettlementBasis SettlementBasis
	SettlementDate  time.Time
}

// CustodyDate is the day this execution belongs to for the purpose of matching a
// custodian statement, normalized to UTC midnight.
//
// IT PREFERS THE SETTLEMENT DATE AND FALLS BACK TO THE TRADE DATE, because a
// custody statement's business date is a SETTLEMENT date for most feeds — the
// custodian reports what moved through the account that day — and the platform's
// own journal now carries both dates (#1043). Falling back rather than refusing
// is correct for an entry whose producer asserted no settlement date: its trade
// date is the only economic day anybody stated about it, and windowing on a date
// nobody set would silently drop the entry out of every comparison.
//
// It is a lower-bound convention and it is stated rather than assumed: every
// venue adapter in this estate is crypto spot and ledger.fillSettlement asserts
// settlement AT the execution time, so the two dates are the same day for every
// fill this book folds today. The distinction becomes load-bearing the moment a
// T+n instrument arrives, which is exactly when the settled/traded split does.
func (e Execution) CustodyDate() time.Time {
	d := e.SettlementDate
	if d.IsZero() {
		d = e.TradeDate
	}
	u := d.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// CustodyBasis is everything ONE custodian's statement is compared against: the
// folded holdings, the executions behind them, and what the fold left out.
//
// IT IS ONE VALUE BECAUSE IT IS ONE ANSWER. The position pass and the transaction
// pass must be over the same slice of the journal or they disagree about what
// they are reconciling, and two return paths is how that divergence gets
// introduced by a change that looks local.
type CustodyBasis struct {
	// Book is the fold of the entries in the basis.
	Book *Book
	// Executions are the entries in the basis that carry an external reference,
	// sorted by (Ref, EntryID) so a run reads the same way every time.
	Executions []Execution

	// Unreferenced is how many value-moving TRADE entries in the basis carry NO
	// external reference and are therefore in the POSITION pass and not the
	// TRANSACTION one.
	//
	// IT IS COUNTED RATHER THAN DROPPED, and that is not decoration. An entry
	// with no SourceRef cannot be matched against a custodian trade line, so a
	// producer that stops stamping one would silently shrink the transaction
	// pass — and a shrinking transaction pass reports FEWER breaks, which reads
	// exactly like a book that has come into agreement. "Nothing configured" and
	// "checked, and fine" must never look the same, and the count is what
	// separates them.
	Unreferenced int

	// Accounts is the exchange-account scope actually folded — declared for
	// MaterializeForAccounts, derived from the journal for MaterializeAttributed.
	Accounts AccountScope
	// Unmapped names non-empty accounts the journal touched that no custodian
	// claims. A caller must refuse the run rather than compare a partial book.
	Unmapped []string
	// Residue is the value the fold deliberately excluded: entries that settled
	// against no exchange account, which no custodian statement can report.
	Residue UnattributedResidue
}

// executionsOf builds the transaction grain of a set of kept entries.
//
// DEDUPED ON EntryID the way Book.Apply is, and for the same reason: the journal
// legitimately returns a restated entry alongside the original, and counting both
// would put two executions into the pass for one economic event — the second of
// which no custodian statement will ever match, so it would report as an
// execution the custodian never saw on every run forever.
//
// The entries are expected in the fold's canonical order, so the copy that wins a
// duplicate is the same one Book.Apply folded.
func executionsOf(kept []*Event) ([]Execution, int) {
	seen := make(map[string]bool, len(kept))
	out := make([]Execution, 0, len(kept))
	unreferenced := 0
	for _, e := range kept {
		if e == nil || e.EntryID == "" || seen[e.EntryID] {
			continue
		}
		seen[e.EntryID] = true
		if e.Type != EntryTrade {
			// ONLY A TRADE IS AN EXECUTION. A custodian's transaction lines are
			// TRADE lines, so putting a cash movement or a fee into this pass —
			// both of which legitimately carry a SourceRef — would report every
			// one of them as an execution the custodian never settled, on every
			// run. That is a break queue with routine false positives in it,
			// which is the queue an operations team stops reading.
			continue
		}
		if e.SourceRef == "" {
			// Only an entry that MOVES something is a missing execution when it
			// is missing: a bookkeeping artefact with no legs is not a trade the
			// custodian should have reported, and counting it would make the
			// unreferenced figure unreadable.
			if movesValue(e) {
				unreferenced++
			}
			continue
		}
		out = append(out, Execution{
			Ref:             e.SourceRef,
			EntryID:         e.EntryID,
			InstrumentID:    e.InstrumentID,
			Quantity:        cloneRat(e.Quantity),
			Price:           cloneRat(e.Price),
			Cash:            cloneRat(e.Cash),
			CashCurrency:    e.CashCurrency,
			VenueAccountID:  e.VenueAccountID,
			TradeDate:       e.Effective,
			SettlementBasis: e.SettlementBasis,
			SettlementDate:  e.SettlementDate,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ref != out[j].Ref {
			return out[i].Ref < out[j].Ref
		}
		return out[i].EntryID < out[j].EntryID
	})
	return out, unreferenced
}

func cloneRat(r *big.Rat) *big.Rat {
	if r == nil {
		return nil
	}
	return new(big.Rat).Set(r)
}
