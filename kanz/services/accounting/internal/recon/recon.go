// Package recon reconciles the IBOR book against a custodian / administrator
// statement (IBOR-01e) and detects breaks. It mirrors the DATA-05 reconciler
// stance — two independent views of the same truth keyed on a shared identity
// (here the instrument / currency rather than a transport's idempotency_key), a
// difference beyond tolerance is a break, and detection is reported, not
// silently absorbed. A break feeds the same investigation queue a settlement
// fail (POST-01d) does.
package recon

import (
	"math/big"
	"sort"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// BreakKind classifies a reconciliation break.
type BreakKind int

const (
	// BreakQuantity: both sides hold the instrument but the quantities differ
	// beyond tolerance.
	BreakQuantity BreakKind = iota
	// BreakMissingAtCustodian: the IBOR holds it, the custodian does not.
	BreakMissingAtCustodian
	// BreakMissingInIBOR: the custodian holds it, the IBOR does not.
	BreakMissingInIBOR
	// BreakCash: a currency cash balance differs beyond tolerance.
	BreakCash

	// THE TWO BELOW ARE THE TRANSACTION GRAIN (#1049). The four above compare
	// TOTALS, and a total is blind to its own composition: a wrongly-booked
	// execution and a missing one on the same instrument produce the same
	// correct-looking position. These are the only kinds that can name WHICH
	// execution, and their Key is an external reference rather than an
	// instrument or a currency.

	// BreakExecutionMissingAtCustodian: the book holds an execution whose
	// reference appears on no trade line the custodian reported for the business
	// date.
	BreakExecutionMissingAtCustodian
	// BreakExecutionMissingInIBOR: the custodian reported a trade line whose
	// reference matches no entry in the comparison basis. THE DIRECTION THAT
	// MEANS A FILL NEVER REACHED THE BOOK — observed on a reference, rather than
	// inferred from a balance.
	BreakExecutionMissingInIBOR
)

// BreakKinds returns every kind this engine can produce, in declaration order.
//
// IT EXISTS SO NOTHING DOWNSTREAM RETYPES THE SET. The custody control seeds a
// gauge label per kind and the wire enum mirrors them one-to-one; both derive
// from here, and test/arch derives the proto's set from this one. When the defect
// class is "somebody enumerated a set by hand and missed a member" (#806, #803),
// a list written a second time in the consumer is a further copy of the thing
// that broke — so a kind added above reaches the metric, the wire and the guard
// without anyone remembering a second edit.
func BreakKinds() []BreakKind {
	return []BreakKind{BreakQuantity, BreakMissingAtCustodian, BreakMissingInIBOR, BreakCash,
		BreakExecutionMissingAtCustodian, BreakExecutionMissingInIBOR}
}

func (k BreakKind) String() string {
	switch k {
	case BreakQuantity:
		return "quantity"
	case BreakMissingAtCustodian:
		return "missing_at_custodian"
	case BreakMissingInIBOR:
		return "missing_in_ibor"
	case BreakCash:
		return "cash"
	case BreakExecutionMissingAtCustodian:
		return "execution_missing_at_custodian"
	case BreakExecutionMissingInIBOR:
		return "execution_missing_in_ibor"
	default:
		return "unknown"
	}
}

// Break is one detected discrepancy between the IBOR and the custodian.
type Break struct {
	Kind BreakKind
	// Key is the instrument id (position break), the currency code (cash break),
	// or the execution's EXTERNAL REFERENCE for the two transaction-grain kinds —
	// the venue fill id, which is the identifier an operator takes straight to
	// the OMS and the venue.
	Key       string
	IBOR      *big.Rat // the IBOR figure
	Custodian *big.Rat // the custodian figure
	Diff      *big.Rat // IBOR − Custodian
}

// Statement is a custodian / administrator statement — the independent view the
// IBOR is reconciled against, at two grains.
type Statement struct {
	PortfolioID string
	Positions   map[string]*big.Rat // instrument -> quantity
	Cash        map[string]*big.Rat // currency -> balance

	// BusinessDate is the close-of-business date the statement speaks for. It is
	// the WINDOW the transaction leg compares within: the book side is the whole
	// comparison basis, and matching it against one day of trade lines without a
	// window would report the fund's entire trading history as executions the
	// custodian never saw. Zero means no window, and the transaction leg does not
	// run.
	BusinessDate time.Time
	// Transactions are the custodian's trade lines for BusinessDate.
	Transactions []Transaction
	// Grain says what the feed supplies, and it is what makes an EMPTY
	// Transactions list readable. Empty under GrainTransactions means "no trades
	// that day"; empty under GrainBalancesOnly means "this feed sends none";
	// empty under GrainUnknown means nobody said. Only the first is a comparison.
	Grain Grain
}

// Reconcile compares the book against the statement and returns the breaks,
// deterministically ordered. A difference within tolerance (inclusive) matches;
// a non-positive/nil tolerance means an exact match is required. A zero quantity
// on one side is treated as not-held, so a flat IBOR position the custodian also
// does not hold is not a break.
//
// # Two passes, one result (#1049)
//
// The netted pass over positions and cash, and the TRANSACTION pass over
// executions — matched by external reference against the statement's trade lines
// for its business date. They are one call and one break slice because they are
// one verdict about one comparison basis: a caller that could run the first
// without the second would be back at the netted grain with nothing saying so.
//
// executions are the book side of the transaction pass and MUST come from the
// same ledger.CustodyBasis the book did. They are a separate parameter rather
// than a field of Book because Book is snapshot-restorable and a restored one
// carries only the tail's executions — see ledger.CustodyBasis.
//
// THE SECOND RETURN IS NOT OPTIONAL READING. It says whether the transaction
// pass ran, and a caller that ignores it cannot tell "no execution breaks" from
// "no execution comparison happened", which is the netted-grain blindness this
// issue is about with a new coat of paint.
func Reconcile(b *ledger.Book, executions []ledger.Execution, stmt Statement, tolerance *big.Rat) ([]Break, LegStatus) {
	if tolerance == nil {
		tolerance = new(big.Rat)
	}
	var breaks []Break

	// Positions: union of both sides' held instruments.
	insts := map[string]struct{}{}
	for inst, p := range b.Positions {
		if p.Qty.Sign() != 0 {
			insts[inst] = struct{}{}
		}
	}
	for inst, q := range stmt.Positions {
		if q != nil && q.Sign() != 0 {
			insts[inst] = struct{}{}
		}
	}
	for inst := range insts {
		ibor := bookQty(b, inst)
		cust := orZero(stmt.Positions[inst])
		iborHeld := ibor.Sign() != 0
		custHeld := cust.Sign() != 0
		switch {
		case iborHeld && !custHeld:
			breaks = append(breaks, Break{Kind: BreakMissingAtCustodian, Key: inst, IBOR: ibor, Custodian: new(big.Rat), Diff: new(big.Rat).Set(ibor)})
		case !iborHeld && custHeld:
			breaks = append(breaks, Break{Kind: BreakMissingInIBOR, Key: inst, IBOR: new(big.Rat), Custodian: cust, Diff: new(big.Rat).Neg(cust)})
		default:
			if diff := sub(ibor, cust); beyond(diff, tolerance) {
				breaks = append(breaks, Break{Kind: BreakQuantity, Key: inst, IBOR: ibor, Custodian: cust, Diff: diff})
			}
		}
	}

	// Cash: union of both sides' currencies.
	ccys := map[string]struct{}{}
	for ccy := range b.Cash {
		ccys[ccy] = struct{}{}
	}
	for ccy := range stmt.Cash {
		ccys[ccy] = struct{}{}
	}
	for ccy := range ccys {
		ibor := b.CashBalance(ccy)
		cust := orZero(stmt.Cash[ccy])
		if diff := sub(ibor, cust); beyond(diff, tolerance) {
			breaks = append(breaks, Break{Kind: BreakCash, Key: ccy, IBOR: ibor, Custodian: cust, Diff: diff})
		}
	}

	// THE TRANSACTION PASS, over the same basis and into the same break slice.
	txBreaks, leg := transactionLeg(executions, stmt)
	breaks = append(breaks, txBreaks...)

	sort.SliceStable(breaks, func(i, j int) bool {
		if breaks[i].Kind != breaks[j].Kind {
			return breaks[i].Kind < breaks[j].Kind
		}
		return breaks[i].Key < breaks[j].Key
	})
	return breaks, leg
}

func bookQty(b *ledger.Book, inst string) *big.Rat {
	if p := b.Positions[inst]; p != nil {
		return new(big.Rat).Set(p.Qty)
	}
	return new(big.Rat)
}

func sub(a, b *big.Rat) *big.Rat { return new(big.Rat).Sub(a, b) }

// beyond reports whether |diff| exceeds tolerance.
func beyond(diff, tolerance *big.Rat) bool {
	return new(big.Rat).Abs(diff).Cmp(tolerance) > 0
}

func orZero(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return new(big.Rat).Set(r)
}
