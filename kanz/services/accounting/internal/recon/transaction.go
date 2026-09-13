package recon

import (
	"math/big"
	"sort"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// THE TRANSACTION LEG (#1049): the pass that names WHICH execution.
//
// # What the netted pass cannot see
//
// Reconcile's position and cash arms compare TOTALS. A total is blind to its own
// composition, so a netted comparison cannot distinguish:
//
//   - a genuine break from two offsetting errors of equal size;
//   - a wrongly-booked execution from a missing one, when both touch the same
//     instrument.
//
// Twelve fills on one instrument, one of them booked against an execution the
// custodian never settled and one the custodian settled that the book never heard
// of, produce identical position and cash totals. The netted pass reports the line
// as fully correct — and the estate's own alert conceded the inference it was left
// with: "missing_in_ibor is the direction that MOST OFTEN means a fill never
// reached the book". Most often, deduced from a balance rather than observed.
//
// # It is a second PASS, not a second reconciler
//
// The breaks it emits go into the SAME lifecycle: the same stable cross-run id,
// the same ageing, the same assignment and explanation, the same gauges and the
// same alerts. A parallel break queue would be a second answer to "what does an
// operator have to chase", and the one thing worse than an unchased break is two
// lists of them.
//
// # Why it does not re-check quantities
//
// The pass asserts IDENTITY — did both sides see this execution — and leaves
// agreement about amount to the netted pass, which already covers it at the grain
// an operator books against. Inventing a second tolerance regime here would give
// the same disagreement two break kinds, two ages and two owners.

// Grain is what a custodian's feed supplies. It mirrors
// accounting.v1.StatementGrain; custody.wireGrain is the one place they are
// joined.
//
// UNKNOWN IS THE ZERO VALUE AND IT IS A REAL THIRD VALUE, exactly as
// ledger.SettlementUnknown is. "This custodian sends balances only" and "this
// custodian sends trade lines and there were none that day" are the same EMPTY
// LIST, and reading the first as the second reports every execution the book
// holds as one the custodian never saw. So an empty list decides nothing; the
// grain does.
type Grain int

const (
	// GrainUnknown: nobody asserted what this feed supplies. The transaction leg
	// does not run, and the run says so rather than reporting a clean pass it
	// never performed.
	GrainUnknown Grain = iota
	// GrainBalancesOnly: the custodian supplies positions and cash and no trade
	// lines. A positive, legitimate claim — and one an operator can now READ,
	// which is the difference between "no execution is being matched anywhere"
	// and a control that looks complete.
	GrainBalancesOnly
	// GrainTransactions: the statement's trade lines are the COMPLETE set for its
	// business date. Completeness is what makes an absent reference a break
	// rather than a gap.
	GrainTransactions
)

// Grains returns every grain a statement can declare, in wire order.
//
// DERIVED-FROM RATHER THAN COPIED-INTO, for the reason BreakKinds() is: the
// composition root seeds one posture series per grain, and a list retyped there
// would leave a newly added grain ABSENT rather than zero — indistinguishable
// from a deployment that never had it.
func Grains() []Grain { return []Grain{GrainUnknown, GrainBalancesOnly, GrainTransactions} }

func (g Grain) String() string {
	switch g {
	case GrainBalancesOnly:
		return "balances_only"
	case GrainTransactions:
		return "transactions"
	default:
		return "unknown"
	}
}

// Transaction is one trade line a custodian reports for the statement's business
// date — the independent side of the transaction pass.
type Transaction struct {
	// ExternalRef is the custodian's identity for the execution, in the same
	// reference space the journal stamps on the entry (ledger.Event.SourceRef).
	// It is the whole of the match.
	ExternalRef string

	TradeDate      time.Time
	SettlementDate time.Time
	InstrumentID   string
	Quantity       *big.Rat
	Price          *big.Rat
	Cash           *big.Rat
	CurrencyCode   string
}

// LegStatus is whether the transaction pass RAN, and why not when it did not.
//
// IT IS RETURNED RATHER THAN LOGGED, because a caller that cannot tell "the
// transaction pass found nothing" from "the transaction pass did not happen" is
// back in the state this whole control exists to abolish, one grain in. A clean
// run under GrainBalancesOnly is a real, positive result about BALANCES and says
// nothing at all about executions, and the run must be able to say so.
type LegStatus int

const (
	// LegRan: the pass compared the statement's trade lines against the book's
	// executions for the business date.
	LegRan LegStatus = iota
	// LegGrainUnknown: the statement asserted no grain. NOT the same as
	// balances-only — nobody said, so nothing is concluded.
	LegGrainUnknown
	// LegBalancesOnly: the custodian states it supplies no trade lines. A stated
	// absence, and the honest reading of an empty list.
	LegBalancesOnly
	// LegNoBusinessDate: the statement names no business date, so there is no
	// window to compare within. The ad-hoc reconcile route is deliberately here:
	// its statement comes from a request body and names no date, and windowing a
	// whole journal against it would report every execution the fund has ever
	// made as one the custodian never saw.
	LegNoBusinessDate
)

func (s LegStatus) String() string {
	switch s {
	case LegRan:
		return "ran"
	case LegGrainUnknown:
		return "grain_unknown"
	case LegBalancesOnly:
		return "balances_only"
	case LegNoBusinessDate:
		return "no_business_date"
	default:
		return "unspecified"
	}
}

// Ran reports whether the transaction pass actually compared anything.
func (s LegStatus) Ran() bool { return s == LegRan }

// businessDay normalizes t to the UTC midnight naming its business date. It is
// custody.BusinessDay's rule, applied here so this package can window without
// depending on the control that schedules it.
func businessDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// transactionLeg compares the statement's trade lines against the book's
// executions for the statement's business date, and returns the breaks and
// whether it ran.
//
// # The window, and why there has to be one
//
// The book side is the WHOLE custody comparison basis — every execution in the
// journal, because the custody folds deliberately do not resume from a checkpoint.
// A statement covers ONE business date. Matching all of one against one day of the
// other would report every execution older than the statement as missing at the
// custodian: the entire trading history as breaks, on the first run. So the book
// side is windowed to the statement's business date by ledger.Execution.CustodyDate
// — the settlement date when the entry asserts one (#1043), the trade date
// otherwise.
//
// # Why an absent reference is a break in both directions
//
// A statement at GrainTransactions asserts its trade lines are COMPLETE for the
// date, exactly as an absent instrument in positions is a positive claim of zero.
// That is what makes each direction meaningful:
//
//	the book holds it, the custodian's lines do not  -> the execution never
//	  reached the custodian, or reached it under a reference nobody mapped;
//	the custodian reports it, the book has no entry  -> A FILL NEVER REACHED THE
//	  BOOK. Observed, on a reference, rather than inferred from a balance.
func transactionLeg(executions []ledger.Execution, stmt Statement) ([]Break, LegStatus) {
	switch stmt.Grain {
	case GrainTransactions:
	case GrainBalancesOnly:
		return nil, LegBalancesOnly
	default:
		return nil, LegGrainUnknown
	}
	if stmt.BusinessDate.IsZero() {
		return nil, LegNoBusinessDate
	}
	window := businessDay(stmt.BusinessDate)

	// The book's executions for the date, by reference. A reference the journal
	// carries twice within one window is folded to the FIRST occurrence, matching
	// the fold's own dedup: two book rows for one reference is one execution as
	// far as a custodian is concerned, and reporting the second as unmatched
	// would be a break about the book's own bookkeeping wearing a custodian's
	// name.
	inBook := map[string]ledger.Execution{}
	for _, e := range executions {
		if e.Ref == "" || !e.CustodyDate().Equal(window) {
			continue
		}
		if _, dup := inBook[e.Ref]; !dup {
			inBook[e.Ref] = e
		}
	}

	inStatement := map[string]Transaction{}
	for _, tx := range stmt.Transactions {
		if tx.ExternalRef == "" {
			continue
		}
		if _, dup := inStatement[tx.ExternalRef]; !dup {
			inStatement[tx.ExternalRef] = tx
		}
	}

	var breaks []Break
	for ref, e := range inBook {
		if _, matched := inStatement[ref]; matched {
			continue
		}
		qty := e.Quantity
		var diff *big.Rat
		if qty != nil {
			diff = new(big.Rat).Set(qty)
		}
		breaks = append(breaks, Break{
			Kind: BreakExecutionMissingAtCustodian, Key: ref,
			IBOR: qty, Custodian: new(big.Rat), Diff: diff,
		})
	}
	for ref, tx := range inStatement {
		if _, matched := inBook[ref]; matched {
			continue
		}
		qty := tx.Quantity
		var diff *big.Rat
		if qty != nil {
			diff = new(big.Rat).Neg(qty)
		}
		breaks = append(breaks, Break{
			Kind: BreakExecutionMissingInIBOR, Key: ref,
			IBOR: new(big.Rat), Custodian: qty, Diff: diff,
		})
	}
	sort.SliceStable(breaks, func(i, j int) bool {
		if breaks[i].Kind != breaks[j].Kind {
			return breaks[i].Kind < breaks[j].Kind
		}
		return breaks[i].Key < breaks[j].Key
	})
	return breaks, LegRan
}
