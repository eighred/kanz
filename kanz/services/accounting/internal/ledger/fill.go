package ledger

import (
	"fmt"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	"google.golang.org/protobuf/proto"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// FromFill builds the TRADE journal entry for an OMS-01 Fill FACT — the seam
// where the execution book feeds the accounting book. It computes the double
// entry: the signed position leg (BUY +, SELL −) and the offsetting cash leg
// (BUY pays cash out, SELL takes cash in), net of the fee.
//
// A verified spot execution settles in its quote currency. Position quantity
// includes fees or rebates in the base asset. The original fill retains the
// gross execution size for transaction-level custody comparison. Older fills
// use the supplied cash currency and must have a representable cash fee.
//
// The effective time is the venue execution time; knowledge is when the book
// ingests the fill.
func FromFill(portfolioID string, fill *orderpb.Fill, cashCurrency string, knowledge time.Time) (*Event, error) {
	// THE IBOR REFUSES WHAT THE POSITION BOOK REFUSES (#631). It refused none of
	// them until now, and the fill_id case did not merely mis-book — it lost
	// money silently: EntryID is "fill:" + FillId and Store.Append is ON CONFLICT
	// DO NOTHING, so with an empty id the FIRST unidentified fill was journalled
	// and every later one in that tenant was discarded as a duplicate, forever,
	// with no error and no counter. The store's own empty-entry-id refusal sits
	// two lines above that INSERT and could never fire, because "fill:" is not
	// empty.
	//
	// Returning the error rather than skipping is what makes the delivery nack
	// and park. A skip here would put the two books back into the state this
	// fixes, with the ledger quietly dropping what the position book stops on.
	if err := fillfact.Validate(fill); err != nil {
		return nil, fmt.Errorf("ledger: refusing to book fill: %w", err)
	}
	legs, err := fillfact.SettlementLegs(fill, cashCurrency)
	if err != nil {
		return nil, fmt.Errorf("ledger: fill %s: %w", fill.GetFillId(), err)
	}

	eff := fill.GetExecutedAt().AsTime()
	if eff.IsZero() {
		eff = knowledge
	}
	basis, settlementDate := fillSettlement(eff)
	evidence, err := proto.MarshalOptions{Deterministic: true}.Marshal(fill)
	if err != nil {
		return nil, err
	}
	return &Event{
		ExecutionEvidence: evidence,
		EntryID:           "fill:" + fillfact.ExecutionKey(fill),
		PortfolioID:       portfolioID,
		// The account the fill SETTLED against — reported by the venue that executed
		// it, not inferred from the order's intent. Where the cash actually went is
		// the only thing a book of record may say about where the cash went.
		VenueAccountID:  fill.GetVenueAccountId(),
		Type:            EntryTrade,
		InstrumentID:    fill.GetInstrumentId(),
		Quantity:        legs.Quantity,
		tradedQuantity:  signedTradeSize(fill),
		Price:           legs.Price,
		Cash:            legs.Cash,
		CashCurrency:    legs.Currency,
		Effective:       eff,
		Knowledge:       knowledge,
		SettlementBasis: basis,
		SettlementDate:  settlementDate,
		SourceRef:       fill.GetFillId(),
	}, nil
}

// The venue confirms the gross trade even when a fee reduces the delivery.
func signedTradeSize(f *orderpb.Fill) *big.Rat {
	q := dec.FromProto(f.GetQuantity())
	if f.GetSide() == orderpb.Side_SIDE_SELL {
		q.Neg(q)
	}
	return q
}

// fillSettlement decides which of a fill's legs are immediate and which are
// pending — THE ONE PLACE THE T+0 ASSUMPTION LIVES (#1043).
//
// Before this existed the assumption was not written down anywhere: FromFill
// emitted the position leg and the FULL cash leg at the execution time, so "the
// fund owns it and can spend the proceeds the instant it matched" was implicit in
// every consumer of the book rather than stated once by the component that knows
// the venue.
//
// # Why T+0 is the right answer today, and what would change it
//
// Both venue adapters in this estate are crypto SPOT, and they say so through the
// capability surface rather than in prose: BinanceVenue.MarginModes and
// OKXVenue.MarginModes each return MARGIN_MODE_UNSPECIFIED alone, which is the
// cash/spot regime, and venueadapter/server reports it as venue.v1's
// supported_margin_modes. Spot on those venues settles atomically with the match —
// base asset and quote cash move in the same exchange transaction — so the
// position leg and the cash leg are both FINAL at ExecutedAt. Asserting
// SettlementSettled here is therefore a statement about the venue, not a default.
//
// WHEN A T+n VENUE ARRIVES THIS FUNCTION IS WHAT CHANGES, and only this function.
// A prime-brokered leg, a security settling at a CSD, or an adapter declaring a
// non-CASH margin mode returns SettlementPending with the contractual settlement
// date; the legs stay in the traded book, drop out of the settled one, and every
// caller that asked for the settled basis gets the right answer without being
// edited. test/arch/settlement_basis_absence_is_stated_test.go derives the
// justification above from those two MarginModes declarations and fails the moment
// one of them stops being spot-only, so this comment cannot quietly expire.
func fillSettlement(executedAt time.Time) (SettlementBasis, time.Time) {
	return SettlementSettled, executedAt
}
