package scenario

import (
	"math/big"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/instrument"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
)

// Model one sell and one buy after both opening fills have settled. Keep the
// legs locally funded and preserve each venue's remaining inventory separately.
func localUnwind(pair instrument.Pair, buy, sell *querypb.SpotDepthVenue, acquired, buyCash, sold, sellCash, entryMark *big.Rat) *querypb.SpotLocalUnwind {
	out := &querypb.SpotLocalUnwind{Status: "UNKNOWN"}
	backSell, err := venueTerms(buy, pair, false)
	if err != nil {
		out.Reason = err.Error()
		return out
	}
	backBuy, err := venueTerms(sell, pair, true)
	if err != nil {
		out.Reason = err.Error()
		return out
	}
	bids, err := executableDepth(buy.Book.Bids, buy.SellPriceRange)
	if err != nil {
		out.Reason = buy.Book.Mic + ": " + err.Error()
		return out
	}
	asks, err := executableDepth(sell.Book.Asks, sell.BuyPriceRange)
	if err != nil {
		out.Reason = sell.Book.Mic + ": " + err.Error()
		return out
	}

	// Never borrow initial base to sell a larger lot or spend a future rebate.
	sellQty := spotFloor(spotDiv(acquired, backSell.fundingFactor), backSell.step)
	// Buy the smallest whole number of lots that replaces the sold inventory
	// including the original sell fee and the new buy fee in native units.
	target := spotDiv(sold, backBuy.baseFactor)
	buyQty := spotFloor(target, backBuy.step)
	if buyQty.Cmp(target) < 0 {
		buyQty = spotAdd(buyQty, backBuy.step)
	}
	sellFilled, proceeds := spotWalk(bids, sellQty, nil)
	buyFilled, cost := spotWalk(asks, buyQty, nil)
	if sellFilled.Cmp(sellQty) != 0 || !backSell.permits(sellQty, proceeds) {
		out.Status, out.Reason = "UNSUPPORTED", buy.Book.Mic+" sell-back size exceeds available depth or violates order limits"
		return out
	}
	if buyFilled.Cmp(buyQty) != 0 || !backBuy.permits(buyQty, cost) {
		out.Status, out.Reason = "UNSUPPORTED", sell.Book.Mic+" buy-back size exceeds available depth or violates order limits"
		return out
	}
	extra := spotSub(spotMul(cost, backBuy.fundingFactor), sellCash)
	if extra.Sign() < 0 {
		extra = new(big.Rat)
	}
	e := spotEncoder{}
	out.ExtraQuoteRequired = e.value(extra)
	if e.failed {
		out.Reason = "buy-back funding exceeds exact decimal range"
		return out
	}
	if extra.Sign() > 0 {
		if sell.AvailableQuote == nil || sell.AvailableQuote.Coefficient < 0 {
			out.Reason = sell.Book.Mic + " initial quote balance unavailable for buy-back"
			return out
		}
		if dec.FromProto(sell.AvailableQuote).Cmp(extra) < 0 {
			out.Status, out.Reason = "UNSUPPORTED", sell.Book.Mic+" has insufficient quote for buy-back after sale proceeds"
			return out
		}
	}

	leftBuy := spotSub(acquired, spotMul(sellQty, backSell.baseFactor))
	leftSell := spotSub(spotMul(buyQty, backBuy.baseFactor), sold)
	cashBuy := spotSub(spotMul(proceeds, backSell.quoteFactor), buyCash)
	cashSell := spotSub(sellCash, spotMul(cost, backBuy.quoteFactor))
	cash := spotAdd(cashBuy, cashSell)
	marked := spotAdd(cash, spotAdd(spotMul(leftBuy, dec.FromProto(buy.Book.Bids[0].Price)),
		spotMul(leftSell, dec.FromProto(sell.Book.Bids[0].Price))))
	leg := func(t spotTerms, quantity, notional, left, cash *big.Rat) *querypb.SpotUnwindLeg {
		return &querypb.SpotUnwindLeg{
			Quantity: e.value(quantity), Notional: e.value(notional), Fee: e.fee(t, pair, quantity, notional),
			ChangeFromStart: &querypb.SpotVenueChange{BaseChange: e.value(left), QuoteChange: e.value(cash)},
		}
	}
	out.SellBack = leg(backSell, sellQty, proceeds, leftBuy, cashBuy)
	out.BuyBack = leg(backBuy, buyQty, cost, leftSell, cashSell)
	out.NetQuoteCash, out.MarkedPnl, out.CostVsEntryMark = e.value(cash), e.value(marked), e.value(spotSub(entryMark, marked))
	if e.failed {
		return &querypb.SpotLocalUnwind{Status: "UNKNOWN", Reason: "local unwind exceeds exact decimal range"}
	}
	out.Status = "SUPPORTED"
	return out
}
