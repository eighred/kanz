package ledger

import (
	"math/big"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

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
	if e.Type == EntryTrade {
		fill, err := executionFromEntry(e)
		if err != nil {
			return nil
		}
		return fill
	}
	if e.Type != EntryFee || len(e.ExecutionEvidence) == 0 || len(e.ExecutionEvidence) > 1<<20 {
		return nil
	}
	var fill orderpb.Fill
	if err := proto.Unmarshal(e.ExecutionEvidence, &fill); err != nil {
		return nil
	}
	if _, ok := dec.InDomainDeep(&fill); !ok {
		return nil
	}
	previous, err := fillfact.FeeRevisionTerms(&fill)
	if err != nil {
		return nil
	}
	delta, err := fillfact.ApprovedFeeDelta(previous, &fill)
	if err != nil {
		return nil
	}
	trade, err := FromFill(e.PortfolioID, &fill, e.CashCurrency, e.Knowledge)
	if err != nil {
		return nil
	}
	want := feeAdjustment(trade, &fill, delta)
	if e.EntryID != want.EntryID || e.SourceRef != want.SourceRef || e.InstrumentID != want.InstrumentID || e.VenueAccountID != want.VenueAccountID ||
		e.Quantity != nil || e.Price != nil || e.Action != nil || !exactRat(e.Cash, want.Cash) ||
		!e.Effective.Equal(want.Effective) || e.SettlementBasis != want.SettlementBasis || !e.SettlementDate.Equal(want.SettlementDate) {
		return nil
	}
	return &fill
}
