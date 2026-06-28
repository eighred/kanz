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

	"github.com/kanz-eng/kanz/services/accounting/internal/ledger"
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
)

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
	default:
		return "unknown"
	}
}

// Break is one detected discrepancy between the IBOR and the custodian.
type Break struct {
	Kind BreakKind
	// Key is the instrument id (position break) or the currency code (cash break).
	Key       string
	IBOR      *big.Rat // the IBOR figure
	Custodian *big.Rat // the custodian figure
	Diff      *big.Rat // IBOR − Custodian
}

// Statement is a custodian / administrator position-and-cash statement — the
// independent view the IBOR is reconciled against.
type Statement struct {
	PortfolioID string
	Positions   map[string]*big.Rat // instrument -> quantity
	Cash        map[string]*big.Rat // currency -> balance
}

// Reconcile compares the book against the statement and returns the breaks,
// deterministically ordered. A difference within tolerance (inclusive) matches;
// a non-positive/nil tolerance means an exact match is required. A zero quantity
// on one side is treated as not-held, so a flat IBOR position the custodian also
// does not hold is not a break.
func Reconcile(b *ledger.Book, stmt Statement, tolerance *big.Rat) []Break {
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

	sort.SliceStable(breaks, func(i, j int) bool {
		if breaks[i].Kind != breaks[j].Kind {
			return breaks[i].Kind < breaks[j].Kind
		}
		return breaks[i].Key < breaks[j].Key
	})
	return breaks
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
