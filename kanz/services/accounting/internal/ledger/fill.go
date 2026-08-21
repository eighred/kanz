package ledger

import (
	"fmt"
	"github.com/eighred/kanz/internal/fillfact"
	"math/big"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// FromFill builds the TRADE journal entry for an OMS-01 Fill FACT — the seam
// where the execution book feeds the accounting book. It computes the double
// entry: the signed position leg (BUY +, SELL −) and the offsetting cash leg
// (BUY pays cash out, SELL takes cash in), net of the fee.
//
// The fill's PRICE carries no currency, so cashCurrency stamps the cash leg (the
// portfolio reporting currency until a per-instrument reference-data join lands).
// The FEE does carry one — order.v1.Fill.fee is a common.v1.Money "in the
// execution currency", and both crypto venue adapters populate it faithfully
// (OKX fillFeeCcy, Binance commission ASSET) — so it is read, not assumed.
//
// A FEE IN ANY OTHER CURRENCY IS REFUSED, NOT NETTED (#221). OKX charges a spot
// BUY's fee in the BASE asset: 1 BTC @ 50000 with fee 0.0008 BTC actually
// delivered 0.9992 BTC for exactly 50000 USD, but subtracting 0.0008 from the USD
// cash leg books +1.0 BTC and −50000.0008 USD — phantom BTC in NAV plus a USD
// debit that never happened, permanently, because the journal is append-only.
// Booking the fee against the asset it was charged in is the complete fix and
// needs two things this package does not have: an Event that can carry a second
// leg, and an instrument_id → base/quote-asset join (datamaster carries ONE
// currency_code per instrument, which is the quote currency, not the pair legs).
// Until both land, the fill DLQs and an operator sees it.
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
	qty := dec.FromProto(fill.GetQuantity())
	price := dec.FromProto(fill.GetPrice())
	fee, err := dec.MoneyIn(fill.GetFee(), cashCurrency)
	if err != nil {
		return nil, fmt.Errorf("ledger: fill %s fee is not in the cash currency, so it cannot be netted against the cash leg: %w",
			fill.GetFillId(), err)
	}

	signedQty := new(big.Rat).Set(qty)
	gross := new(big.Rat).Mul(qty, price) // |qty|·price, the cash notional
	cash := new(big.Rat)
	if fill.GetSide() == orderpb.Side_SIDE_SELL {
		signedQty.Neg(signedQty)
		cash.Set(gross) // sell: cash in
	} else {
		cash.Neg(gross) // buy: cash out
	}
	cash.Sub(cash, fee) // the fee reduces cash on both sides (checked above to be IN cashCurrency)

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
	}, nil
}
