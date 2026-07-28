package ledger

import (
	"math/big"
	"time"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// FromFill builds the TRADE journal entry for an OMS-01 Fill FACT — the seam
// where the execution book feeds the accounting book. It computes the double
// entry: the signed position leg (BUY +, SELL −) and the offsetting cash leg
// (BUY pays cash out, SELL takes cash in), net of fees which always reduce cash.
//
// A fill carries no currency, so cashCurrency stamps the cash leg (the portfolio
// reporting currency until a per-instrument reference-data join lands — the same
// carried-forward seam the OMS position projector notes). The effective time is
// the venue execution time; knowledge is when the book ingests the fill.
func FromFill(portfolioID string, fill *orderpb.Fill, cashCurrency string, knowledge time.Time) *Event {
	qty := dec.FromProto(fill.GetQuantity())
	price := dec.FromProto(fill.GetPrice())
	fee := dec.FromProto(fill.GetFee().GetAmount())

	signedQty := new(big.Rat).Set(qty)
	gross := new(big.Rat).Mul(qty, price) // |qty|·price, the cash notional
	cash := new(big.Rat)
	if fill.GetSide() == orderpb.Side_SIDE_SELL {
		signedQty.Neg(signedQty)
		cash.Set(gross) // sell: cash in
	} else {
		cash.Neg(gross) // buy: cash out
	}
	cash.Sub(cash, fee) // fees always reduce cash

	eff := fill.GetExecutedAt().AsTime()
	if eff.IsZero() {
		eff = knowledge
	}
	return &Event{
		EntryID:     "fill:" + fill.GetFillId(),
		PortfolioID: portfolioID,
		// The account the fill SETTLED against — reported by the venue that executed
		// it, not inferred from the order's intent. Where the cash actually went is
		// the only thing a book of record may say about where the cash went.
		VenueAccountID: fill.GetVenueAccountId(),
		Type:           EntryTrade,
		InstrumentID:   fill.GetInstrumentId(),
		Quantity:       signedQty,
		Price:          price,
		Cash:           cash,
		CashCurrency:   cashCurrency,
		Effective:      eff,
		Knowledge:      knowledge,
		SourceRef:      fill.GetFillId(),
	}
}
