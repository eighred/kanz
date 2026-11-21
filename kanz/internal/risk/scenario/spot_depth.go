package scenario

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/instrument"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const SpotDepthName = "OPERATOR_SPOT_DEPTH"
const SpotBookSkew = time.Second
const SpotBookMaxAge = 2 * time.Second

// This historical what-if uses the caller's inputs only. No balance, fee or
// depth here establishes an entitlement, actionable quote or order approval.
func EvaluateSpotDepth(s *querypb.SpotDepthScenario) (*querypb.SpotDepthResult, error) {
	if len(s.GetVenues()) != 2 || s.GetAsOf().CheckValid() != nil {
		return nil, errors.New("two venue books and a scenario cutoff are required")
	}
	if _, ok := dec.InDomainDeep(s); !ok {
		return nil, errors.New("spot scenario decimal outside supported domain")
	}
	requested := dec.FromProto(s.RequestedBuyQuantity)
	if requested.Sign() <= 0 {
		return nil, errors.New("requested buy quantity must be positive")
	}
	if s.GetRebalanceCost().GetCoefficient() < 0 {
		return nil, errors.New("rebalance cost cannot be negative")
	}
	if s.GetOneLegLossLimit().GetCoefficient() < 0 {
		return nil, errors.New("one-leg loss limit cannot be negative")
	}
	for _, d := range []*commonpb.Decimal{s.SellFillBps, s.AdverseMoveBps} {
		if d != nil && (d.Coefficient < 0 || dec.FromProto(d).Cmp(big.NewRat(10000, 1)) > 0) {
			return nil, errors.New("stress and fill must be between 0 and 10000 basis points")
		}
	}
	a, b := s.Venues[0], s.Venues[1]
	for _, v := range s.Venues {
		if err := validateSpotBook(v, s.AsOf.AsTime()); err != nil {
			return nil, err
		}
	}
	book := a.Book
	if book.Mic == b.Book.Mic || book.InstrumentId != b.Book.InstrumentId {
		return nil, errors.New("books must be from different venues for the same spot instrument")
	}
	if a.BaseAssetRef != b.BaseAssetRef || a.QuoteAssetRef != b.QuoteAssetRef {
		return nil, errors.New("settlement asset references differ across venues")
	}
	if delta := book.EventTime.AsTime().Sub(b.Book.EventTime.AsTime()); delta.Abs() > SpotBookSkew {
		return nil, errors.New("venue timestamps differ by more than one second")
	}
	pair, _ := instrument.SpotPair(book.InstrumentId, book.Symbol)
	otherPair, _ := instrument.SpotPair(b.Book.InstrumentId, b.Book.Symbol)
	if pair != otherPair {
		return nil, errors.New("venue symbols resolve to different spot pairs")
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(s)
	if err != nil {
		return nil, err
	}
	out := &querypb.SpotDepthResult{InstrumentId: book.InstrumentId, BaseCurrency: pair.Base, QuoteCurrency: pair.Quote,
		AsOf: s.AsOf, InputSha256: fmt.Sprintf("%x", sha256.Sum256(wire))}
	for _, venues := range [][2]*querypb.SpotDepthVenue{{a, b}, {b, a}} {
		out.Directions = append(out.Directions, spotDirection(s, venues[0], venues[1], pair))
	}
	return out, nil
}

func validateSpotBook(v *querypb.SpotDepthVenue, cutoff time.Time) error {
	if strings.TrimSpace(v.GetAccountLabel()) == "" || len(v.GetAccountLabel()) > 128 {
		return errors.New("invalid scenario account label")
	}
	if v.ProductType != "CRYPTO_SPOT" || strings.TrimSpace(v.BaseAssetRef) == "" || strings.TrimSpace(v.QuoteAssetRef) == "" ||
		v.BaseAssetRef == v.QuoteAssetRef || len(v.BaseAssetRef) > 256 || len(v.QuoteAssetRef) > 256 {
		return errors.New("both books require crypto spot terms and distinct settlement asset references")
	}
	if len(v.RuleEvidence) > 8 {
		return errors.New("too many order-rule sources")
	}
	for _, ref := range v.RuleEvidence {
		if ref.GetKnownAt().CheckValid() != nil || ref.KnownAt.AsTime().After(cutoff) ||
			strings.TrimSpace(ref.Source) == "" || len(ref.Source) > 256 || len(ref.ResponseSha256) != 64 {
			return errors.New("invalid or future order-rule reference")
		}
		if _, err := hex.DecodeString(ref.ResponseSha256); err != nil {
			return errors.New("invalid order-rule response digest")
		}
	}
	return ValidateObservedBook(v.Book, v.KnownAt, cutoff)
}

// ValidateObservedBook checks the dated levels independently of account terms.
func ValidateObservedBook(b *marketpb.OrderBookSnapshot, knownAt *timestamppb.Timestamp, cutoff time.Time) error {
	if b.GetMic() != "BINANCE" && b.GetMic() != "OKX" {
		return errors.New("expected Binance or OKX spot book")
	}
	pair, ok := instrument.SpotPair(b.InstrumentId, b.Symbol)
	if !ok || (b.Mic == "OKX" && b.Symbol != pair.Base+"-"+pair.Quote) || (b.Mic == "BINANCE" && b.Symbol != pair.Base+pair.Quote) {
		return errors.New("spot pair and venue symbol disagree")
	}
	if len(b.InstrumentId) > 128 || len(b.Symbol) > 128 {
		return errors.New("invalid instrument label")
	}
	if b.EventTime.CheckValid() != nil || knownAt.CheckValid() != nil || b.LastUpdateSequence == 0 {
		return errors.New("venue time, receipt time and sequence are required")
	}
	event, known := b.EventTime.AsTime(), knownAt.AsTime()
	if event.Unix() <= 0 || known.Before(event) || known.After(cutoff) || cutoff.Sub(event) > SpotBookMaxAge {
		return errors.New("book is unavailable at cutoff or more than two seconds old")
	}
	for side, levels := range [][]*marketpb.PriceLevel{b.Bids, b.Asks} {
		if len(levels) == 0 || len(levels) > 100 {
			return errors.New("each book side needs 1–100 levels")
		}
		for i, l := range levels {
			if !dec.IsPositive(l.GetPrice()) || !dec.IsPositive(l.GetSize()) {
				return errors.New("book levels need positive price and size")
			}
			if i > 0 {
				order := dec.Cmp(levels[i-1].Price, l.Price)
				if (side == 0 && order <= 0) || (side == 1 && order >= 0) {
					return errors.New("book prices are not strictly ordered")
				}
			}
		}
	}
	if dec.Cmp(b.Bids[0].Price, b.Asks[0].Price) >= 0 {
		return errors.New("locked or crossed spot book")
	}
	return nil
}

func spotAdd(a, b *big.Rat) *big.Rat { return new(big.Rat).Add(a, b) }
func spotSub(a, b *big.Rat) *big.Rat { return new(big.Rat).Sub(a, b) }
func spotMul(a, b *big.Rat) *big.Rat { return new(big.Rat).Mul(a, b) }
func spotDiv(a, b *big.Rat) *big.Rat { return new(big.Rat).Quo(a, b) }
func spotMin(a, b *big.Rat) *big.Rat {
	if a.Cmp(b) < 0 {
		return a
	}
	return b
}

// Decimal step alignment applies to order quantity, not each individual fill.
func spotFloor(q, step *big.Rat) *big.Rat {
	count := spotDiv(q, step)
	return spotMul(new(big.Rat).SetInt(new(big.Int).Quo(count.Num(), count.Denom())), step)
}

// The taker expires at the first out-of-range match. A later price can
// be inside the band and still be unreachable at this cutoff.
func executableDepth(levels []*marketpb.PriceLevel, band *querypb.SpotExecutionPriceRange) ([]*marketpb.PriceLevel, error) {
	if band == nil {
		return levels, nil
	}
	lo, hi := band.Minimum, band.Maximum
	if (lo == nil && hi == nil) || (lo != nil && !dec.IsPositive(lo)) || (hi != nil && !dec.IsPositive(hi)) ||
		(lo != nil && hi != nil && dec.Cmp(lo, hi) > 0) {
		return nil, errors.New("invalid taker execution price range")
	}
	for i, level := range levels {
		if (lo != nil && dec.Cmp(level.Price, lo) < 0) || (hi != nil && dec.Cmp(level.Price, hi) > 0) {
			return levels[:i], nil
		}
	}
	return levels, nil
}

// Consume successive levels until the quantity or a quote funding limit is
// exhausted. Do not charge a future rebate against the funding requirement.
func spotWalk(levels []*marketpb.PriceLevel, wanted, budget *big.Rat) (quantity, notional *big.Rat) {
	quantity, notional = new(big.Rat), new(big.Rat)
	for _, l := range levels {
		price := dec.FromProto(l.Price)
		take := spotMin(spotSub(wanted, quantity), dec.FromProto(l.Size))
		if budget != nil {
			take = spotMin(take, spotDiv(spotSub(budget, notional), price))
		}
		if take.Sign() <= 0 {
			break
		}
		quantity.Add(quantity, take)
		notional.Add(notional, spotMul(take, price))
	}
	return
}

type spotTerms struct {
	step, minQty, minCash, rate, baseFactor, quoteFactor, fundingFactor *big.Rat
	maxQty                                                              *big.Rat
	feeCurrency                                                         string
}

func venueTerms(v *querypb.SpotDepthVenue, pair instrument.Pair, buy bool) (spotTerms, error) {
	t := spotTerms{}
	for _, d := range []*commonpb.Decimal{v.QuantityStep, v.MinimumQuantity, v.MinimumNotional} {
		if d == nil || d.Coefficient < 0 {
			return t, fmt.Errorf("%s order limits unavailable", v.Book.Mic)
		}
	}
	if !dec.IsPositive(v.QuantityStep) {
		return t, errors.New("quantity step must be positive")
	}
	bps, currency := v.FeeBps, v.FeeCurrency
	if v.BuyTakerFee != nil || v.SellTakerFee != nil {
		if bps != nil || currency != "" {
			return t, fmt.Errorf("%s has conflicting shared and side-specific fee inputs", v.Book.Mic)
		}
		fee := v.SellTakerFee
		if buy {
			fee = v.BuyTakerFee
		}
		bps, currency = fee.GetBps(), fee.GetCurrency()
	}
	if bps == nil || (currency != pair.Base && currency != pair.Quote) {
		return t, fmt.Errorf("%s fee amount or native denomination unavailable", v.Book.Mic)
	}
	t.step, t.minQty, t.minCash = dec.FromProto(v.QuantityStep), dec.FromProto(v.MinimumQuantity), dec.FromProto(v.MinimumNotional)
	if err := t.marketSizeLimits(v, pair); err != nil {
		return t, err
	}
	t.feeCurrency = currency
	t.rate = spotDiv(dec.FromProto(bps), big.NewRat(10000, 1))
	one := big.NewRat(1, 1)
	if new(big.Rat).Abs(t.rate).Cmp(one) >= 0 {
		return t, errors.New("absolute fee must be below 10000 basis points")
	}
	t.baseFactor, t.quoteFactor = one, one
	if currency == pair.Base {
		if buy {
			t.baseFactor = spotSub(one, t.rate)
		} else {
			t.baseFactor = spotAdd(one, t.rate)
		}
	} else if buy {
		t.quoteFactor = spotAdd(one, t.rate)
	} else {
		t.quoteFactor = spotSub(one, t.rate)
	}
	t.fundingFactor = one
	if buy && t.quoteFactor.Cmp(one) > 0 {
		t.fundingFactor = t.quoteFactor
	}
	if !buy && t.baseFactor.Cmp(one) > 0 {
		t.fundingFactor = t.baseFactor
	}
	return t, nil
}

func (t *spotTerms) marketSizeLimits(v *querypb.SpotDepthVenue, pair instrument.Pair) error {
	if v.MaximumQuantity != nil {
		if !dec.IsPositive(v.MaximumQuantity) {
			return fmt.Errorf("%s maximum quantity must be positive", v.Book.Mic)
		}
		t.maxQty = dec.FromProto(v.MaximumQuantity)
	}
	limit := v.MarketValueLimit
	if limit == nil {
		return nil
	}
	if (limit.Currency != pair.Quote && limit.Currency != "USD") || !dec.IsPositive(limit.ReferencePrice) ||
		(limit.MinimumValue == nil && limit.MaximumValue == nil) ||
		(limit.MinimumValue != nil && !dec.IsPositive(limit.MinimumValue)) ||
		(limit.MaximumValue != nil && !dec.IsPositive(limit.MaximumValue)) ||
		(limit.MinimumValue != nil && limit.MaximumValue != nil && dec.Cmp(limit.MinimumValue, limit.MaximumValue) > 0) {
		return fmt.Errorf("%s market value limit or reference price unavailable", v.Book.Mic)
	}
	price := dec.FromProto(limit.ReferencePrice)
	if limit.MinimumValue != nil {
		floor := spotDiv(dec.FromProto(limit.MinimumValue), price)
		if floor.Cmp(t.minQty) > 0 {
			t.minQty = floor
		}
	}
	if limit.MaximumValue != nil {
		ceiling := spotDiv(dec.FromProto(limit.MaximumValue), price)
		if t.maxQty == nil || ceiling.Cmp(t.maxQty) < 0 {
			t.maxQty = ceiling
		}
	}
	return nil
}

// A failed exact wire conversion invalidates the entire direction. A partially
// populated result could otherwise turn an unrepresentable charge into zero.
type spotEncoder struct{ failed bool }

func (e *spotEncoder) value(r *big.Rat) *commonpb.Decimal {
	d, ok := dec.ToProtoExact(r)
	if !ok {
		e.failed = true
	}
	return d
}
func (e *spotEncoder) stress(base, cash, mark *big.Rat) *querypb.SpotLegStress {
	return &querypb.SpotLegStress{InventoryChange: e.value(base), CashChange: e.value(cash), MarkPrice: e.value(mark), MarkedPnl: e.value(spotAdd(cash, spotMul(base, mark)))}
}

func (e *spotEncoder) fee(t spotTerms, pair instrument.Pair, quantity, notional *big.Rat) *commonpb.Money {
	basis := notional
	if t.feeCurrency == pair.Base {
		basis = quantity
	}
	return &commonpb.Money{Amount: e.value(spotMul(basis, t.rate)), CurrencyCode: t.feeCurrency}
}

func (t spotTerms) permits(quantity, notional *big.Rat) bool {
	return quantity.Sign() > 0 && quantity.Cmp(t.minQty) >= 0 && notional.Cmp(t.minCash) >= 0 &&
		(t.maxQty == nil || quantity.Cmp(t.maxQty) <= 0)
}

func spotFunding(cost, proceeds, sellQuantity, buyFundingFactor, sellFundingFactor *big.Rat, net *commonpb.Decimal) *querypb.SpotFunding {
	buyQuote := spotMul(cost, buyFundingFactor)
	sellBase := spotMul(sellQuantity, sellFundingFactor)
	// sellBase * (proceeds / sellQuantity). Cancel the quantity so a
	// repeating VWAP never needs to be rounded before valuing inventory.
	sellValue := spotMul(proceeds, sellFundingFactor)
	capital, turnover := spotAdd(buyQuote, sellValue), spotAdd(cost, proceeds)
	f := &querypb.SpotFunding{}
	// Expanded totals may exceed the wire range even for two valid legs.
	// Preserve independently representable amounts and the existing leg results.
	f.BuyQuoteRequired, _ = dec.ToProtoExact(buyQuote)
	f.SellBaseRequired, _ = dec.ToProtoExact(sellBase)
	f.SellBaseValueQuote, _ = dec.ToProtoExact(sellValue)
	f.FundedCapitalQuote, _ = dec.ToProtoExact(capital)
	f.GrossTurnoverQuote, _ = dec.ToProtoExact(turnover)
	if net != nil {
		netBps := spotMul(dec.FromProto(net), big.NewRat(10000, 1))
		rate := func(denominator *commonpb.Decimal) *commonpb.Decimal {
			if !dec.IsPositive(denominator) {
				return nil
			}
			value, _ := dec.ParseProtoExact(spotDiv(netBps, dec.FromProto(denominator)).FloatString(6))
			return value
		}
		f.ReturnOnCapitalBps = rate(f.FundedCapitalQuote)
		f.BreakEvenCostPerLegBps = rate(f.GrossTurnoverQuote)
	}
	return f
}

func spotDirection(s *querypb.SpotDepthScenario, buy, sell *querypb.SpotDepthVenue, pair instrument.Pair) *querypb.SpotDepthDirection {
	result := &querypb.SpotDepthDirection{BuyVenue: buy.Book.Mic, SellVenue: sell.Book.Mic, Status: "UNKNOWN"}
	for _, leg := range []struct {
		venue   string
		balance *commonpb.Decimal
	}{{buy.Book.Mic, buy.AvailableQuote}, {sell.Book.Mic, sell.AvailableBase}} {
		if leg.balance == nil || leg.balance.Coefficient < 0 {
			result.Reason = leg.venue + " balance or order limits unavailable"
			return result
		}
	}
	b, err := venueTerms(buy, pair, true)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	v, err := venueTerms(sell, pair, false)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	want := dec.FromProto(s.RequestedBuyQuantity)
	asks, err := executableDepth(buy.Book.Asks, buy.BuyPriceRange)
	if err != nil {
		result.Reason = buy.Book.Mic + ": " + err.Error()
		return result
	}
	bids, err := executableDepth(sell.Book.Bids, sell.SellPriceRange)
	if err != nil {
		result.Reason = sell.Book.Mic + ": " + err.Error()
		return result
	}
	if len(asks) == 0 || len(bids) == 0 {
		result.Status = "UNSUPPORTED"
		result.Reason = "first match outside the taker execution price range"
		result.Shortfall = s.RequestedBuyQuantity
		return result
	}
	buyBudget := spotDiv(dec.FromProto(buy.AvailableQuote), b.fundingFactor)
	buyLimit := want
	if b.maxQty != nil {
		buyLimit = spotMin(buyLimit, b.maxQty)
	}
	buyCap, _ := spotWalk(asks, buyLimit, buyBudget)
	sellBudget := spotDiv(dec.FromProto(sell.AvailableBase), v.fundingFactor)
	if v.maxQty != nil {
		sellBudget = spotMin(sellBudget, v.maxQty)
	}
	sellCap, _ := spotWalk(bids, sellBudget, nil)
	sellCap = spotFloor(sellCap, v.step)
	buyQty := spotFloor(buyCap, b.step)
	// Size the buy to fund the largest available hedge. When the exact
	// purchase falls between buy lots, a rounded-up purchase is acceptable
	// only if its remaining inventory is less than one sell lot.
	target := spotDiv(spotMul(sellCap, v.baseFactor), b.baseFactor)
	if buyQty.Cmp(target) > 0 {
		lower := spotFloor(target, b.step)
		upper := spotAdd(lower, b.step)
		buyQty = lower
		if lower.Cmp(target) < 0 && upper.Cmp(buyCap) <= 0 &&
			spotFloor(spotDiv(spotMul(upper, b.baseFactor), v.baseFactor), v.step).Cmp(sellCap) == 0 {
			buyQty = upper
		}
	}
	acquired := spotMul(buyQty, b.baseFactor)
	sellQty := spotFloor(spotDiv(acquired, v.baseFactor), v.step)
	_, cost := spotWalk(asks, buyQty, nil)
	_, proceeds := spotWalk(bids, sellQty, nil)
	if !b.permits(buyQty, cost) || !v.permits(sellQty, proceeds) {
		result.Status = "UNSUPPORTED"
		result.Reason = "paired size falls below venue funding, depth or order minimums"
		result.Shortfall = s.RequestedBuyQuantity
		return result
	}
	buyCash := spotMul(cost, b.quoteFactor)
	sellCash := spotMul(proceeds, v.quoteFactor)
	sold := spotMul(sellQty, v.baseFactor)
	residual, cash := spotSub(acquired, sold), spotSub(sellCash, buyCash)
	e := spotEncoder{}
	result.BuyQuantity, result.SellQuantity, result.Shortfall = e.value(buyQty), e.value(sellQty), e.value(spotSub(want, buyQty))
	result.BuyNotional, result.SellNotional = e.value(cost), e.value(proceeds)
	result.BuyFee, result.SellFee = e.fee(b, pair, buyQty, cost), e.fee(v, pair, sellQty, proceeds)
	result.CashChange, result.ResidualBase = e.value(cash), e.value(residual)
	result.BuyVenueChange = &querypb.SpotVenueChange{BaseChange: e.value(acquired), QuoteChange: e.value(new(big.Rat).Neg(buyCash))}
	result.SellVenueChange = &querypb.SpotVenueChange{BaseChange: e.value(new(big.Rat).Neg(sold)), QuoteChange: e.value(sellCash)}
	result.GrossQuoteCashChange = e.value(spotSub(proceeds, cost))
	result.TotalFeesInQuote = e.value(spotAdd(spotMul(cost, b.rate), spotMul(proceeds, v.rate)))
	// The unmatched portion of the acquisition can be sold locally at the buy venue.
	buyBid := dec.FromProto(buy.Book.Bids[0].Price)
	beforeRebalance := spotAdd(cash, spotMul(residual, buyBid))
	result.MarkedNetBeforeRebalance, _ = dec.ToProtoExact(beforeRebalance)
	if s.RebalanceCost != nil && s.RebalanceCost.Coefficient >= 0 {
		result.MarkedNetAfterRebalance = e.value(spotSub(beforeRebalance, dec.FromProto(s.RebalanceCost)))
	}
	bps := big.NewRat(10000, 1)
	if s.AdverseMoveBps != nil {
		move := dec.FromProto(s.AdverseMoveBps)
		// The failed hedge leaves the inventory at the venue of the fill.
		// Mark the unhedged purchase at its local bid and the sale at its local ask.
		longMark := spotMul(buyBid, spotDiv(spotSub(bps, move), bps))
		shortMark := spotMul(dec.FromProto(sell.Book.Asks[0].Price), spotDiv(spotAdd(bps, move), bps))
		result.BuyOnly = e.stress(acquired, new(big.Rat).Neg(buyCash), longMark)
		result.SellOnly = e.stress(new(big.Rat).Neg(sold), sellCash, shortMark)
		if s.SellFillBps != nil {
			part := spotFloor(spotMul(sellQty, spotDiv(dec.FromProto(s.SellFillBps), bps)), v.step)
			_, partProceeds := spotWalk(bids, part, nil)
			result.PartialSell = e.stress(spotSub(acquired, spotMul(part, v.baseFactor)), spotSub(spotMul(partProceeds, v.quoteFactor), buyCash), longMark)
		}
	}
	if e.failed {
		return &querypb.SpotDepthDirection{BuyVenue: result.BuyVenue, SellVenue: result.SellVenue, Status: "UNKNOWN", Reason: "result exceeds exact decimal range"}
	}
	result.Funding = spotFunding(cost, proceeds, sellQty, b.fundingFactor, v.fundingFactor, result.MarkedNetAfterRebalance)
	result.LocalUnwind = localUnwind(pair, buy, sell, acquired, buyCash, sold, sellCash, beforeRebalance)
	result.OneLegLimitStatus = "UNKNOWN"
	if result.BuyOnly != nil && result.SellOnly != nil {
		worst := spotMin(new(big.Rat), spotMin(dec.FromProto(result.BuyOnly.MarkedPnl), dec.FromProto(result.SellOnly.MarkedPnl)))
		loss := new(big.Rat).Neg(worst)
		result.WorstOneLegLoss, _ = dec.ToProtoExact(loss)
		if result.WorstOneLegLoss != nil && s.OneLegLossLimit != nil {
			result.OneLegLimitStatus = "WITHIN"
			if loss.Cmp(dec.FromProto(s.OneLegLossLimit)) > 0 {
				result.OneLegLimitStatus = "EXCEEDED"
			}
		}
	}
	result.Status = "SUPPORTED"
	if result.MarkedNetAfterRebalance == nil {
		result.Reason = "net unknown until rebalance cost is supplied"
	}
	return result
}
