package ledger

import (
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// PostgreSQL's timestamptz columns retain whole microseconds. The immutable
// execution evidence also retains the original venue timestamp. Restore the
// source instants only on a cash read, after verifying the stored clock
// projections and binding all posting terms to the trade or its fee correction.
// The fill carries no accounting knowledge timestamp.
func restoreCashExecutionTime(e *Event) bool {
	if e.Type != EntryTrade && e.Type != EntryFee {
		return false
	}
	fill, err := decodeExecutionEvidence(e.ExecutionEvidence)
	if err != nil || fill == nil || fill.GetExecutedAt().CheckValid() != nil {
		return false
	}
	executed := fill.ExecutedAt.AsTime()
	if executed.Nanosecond()%1000 == 0 || !e.Effective.Equal(executed.Truncate(time.Microsecond)) {
		return false
	}
	_, settled := fillSettlement(executed)
	if !e.SettlementDate.Equal(settled.Truncate(time.Microsecond)) {
		return false
	}
	exact := *e
	exact.Effective, exact.SettlementDate = executed, settled
	if !matchesExecutionPosting(&exact, fill) {
		return false
	}
	e.Effective, e.SettlementDate = executed, settled
	return true
}

// executionDebits uses the very entries selected for the balance's economic
// cutoff. It never infers attribution from a timestamp or a venue fill alias.
// Unattributed historical executions leave the handoff incomplete, while the
// ledger can continue recording cash rather than discarding financial facts.
func executionDebits(events []*Event, currency string) (map[string]*big.Rat, uint64) {
	debits := map[string]*big.Rat{}
	seen := map[string]bool{}
	var unknown uint64
	for _, e := range events {
		if seen[e.EntryID] {
			continue
		}
		seen[e.EntryID] = true
		if e.Type != EntryTrade && len(e.ExecutionEvidence) == 0 && !(e.Type == EntryFee && e.InstrumentID != "") {
			continue // ordinary cash movements and account fees are not executions
		}
		if e.CashCurrency != "" && e.CashCurrency != currency {
			continue
		}
		fill := coveredExecution(e)
		if fill == nil || e.Cash == nil || e.CashCurrency != currency {
			unknown++
			continue
		}
		add(debits, fill.OrderId, new(big.Rat).Neg(e.Cash))
	}
	for _, debit := range debits {
		// Sale proceeds are already in the cash total; they are not permission
		// to subtract a negative reservation or to finance another order twice.
		if debit.Sign() < 0 {
			debit.SetInt64(0)
		}
	}
	return debits, unknown
}

func coveredExecution(e *Event) *orderpb.Fill {
	fill, err := decodeExecutionEvidence(e.ExecutionEvidence)
	if err != nil || fill == nil || !matchesExecutionPosting(e, fill) {
		return nil
	}
	return fill
}

func matchesExecutionPosting(e *Event, fill *orderpb.Fill) bool {
	if e.Type == EntryTrade {
		return validateExecutionTrade(e, fill) == nil
	}
	if e.Type != EntryFee {
		return false
	}
	previous, err := fillfact.FeeRevisionTerms(fill)
	if err != nil {
		return false
	}
	delta, err := fillfact.ApprovedFeeDelta(previous, fill)
	if err != nil {
		return false
	}
	trade, err := FromFill(e.PortfolioID, fill, e.CashCurrency, e.Knowledge)
	if err != nil {
		return false
	}
	want := feeAdjustment(trade, fill, delta)
	return e.EntryID == want.EntryID && e.SourceRef == want.SourceRef && e.InstrumentID == want.InstrumentID && e.VenueAccountID == want.VenueAccountID &&
		e.Quantity == nil && e.Price == nil && e.Action == nil && exactRat(e.Cash, want.Cash) &&
		e.Effective.Equal(want.Effective) && e.SettlementBasis == want.SettlementBasis && e.SettlementDate.Equal(want.SettlementDate)
}
