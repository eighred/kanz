package scenario

import (
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func depthExample() *querypb.SpotDepthScenario {
	at := timestamppb.New(time.Unix(1800000000, 0))
	n := func(c int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: c} }
	v := func(mic, symbol, unit string, bid, ask, bps int64) *querypb.SpotDepthVenue {
		level := func(p int64) *marketpb.PriceLevel { return &marketpb.PriceLevel{Price: n(p), Size: n(1)} }
		return &querypb.SpotDepthVenue{Book: &marketpb.OrderBookSnapshot{InstrumentId: "BTC-USDT", Symbol: symbol, Mic: mic, LastUpdateSequence: 1, EventTime: at,
			Bids: []*marketpb.PriceLevel{level(bid), level(bid - 10)}, Asks: []*marketpb.PriceLevel{level(ask), level(ask + 10)}},
			KnownAt: at, AccountLabel: mic + "-pilot", BaseAssetRef: "btc-asset-id", QuoteAssetRef: "usdt-asset-id", ProductType: "CRYPTO_SPOT",
			AvailableBase: n(2), AvailableQuote: n(200000), QuantityStep: &commonpb.Decimal{Coefficient: 1, Exponent: -4}, MinimumQuantity: &commonpb.Decimal{Coefficient: 1, Exponent: -4},
			MinimumNotional: n(10), FeeBps: n(bps), FeeCurrency: unit}
	}
	return &querypb.SpotDepthScenario{AsOf: at, RequestedBuyQuantity: n(1), RebalanceCost: n(1), AdverseMoveBps: n(100), SellFillBps: n(5000),
		Venues: []*querypb.SpotDepthVenue{v("OKX", "BTC-USDT", "BTC", 49980, 50000, 8), v("BINANCE", "BTCUSDT", "USDT", 50030, 50040, 5)}}
}

func assertDepthAmount(t *testing.T, d *commonpb.Decimal, want string) {
	t.Helper()
	expected, ok := new(big.Rat).SetString(want)
	if !ok || d == nil || dec.FromProto(d).Cmp(expected) != 0 {
		t.Fatalf("amount %v, expected %s", d, want)
	}
}

func TestLocalInventoryRoundTrip(t *testing.T) {
	n := func(i int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: i} }
	s := depthExample()
	a, b := s.Venues[0], s.Venues[1]
	for _, v := range s.Venues {
		v.FeeBps, v.QuantityStep = n(0), n(1)
		v.Book.Bids, v.Book.Asks = v.Book.Bids[:1], v.Book.Asks[:1]
	}
	a.Book.Bids[0].Price, a.Book.Asks[0].Price = n(99), n(100)
	b.Book.Bids[0].Price, b.Book.Asks[0].Price = n(110), n(112)
	b.AvailableQuote = n(2)
	s.RebalanceCost = nil
	run := func(status string) *querypb.SpotLocalUnwind {
		t.Helper()
		reply, err := EvaluateSpotDepth(s)
		if err != nil {
			t.Fatal(err)
		}
		d := reply.Directions[0]
		if d.Status != "SUPPORTED" || d.LocalUnwind.GetStatus() != status {
			t.Fatal(d)
		}
		if status != "SUPPORTED" && (d.LocalUnwind.MarkedPnl != nil || d.LocalUnwind.SellBack != nil || d.LocalUnwind.BuyBack != nil) {
			t.Fatal(d.LocalUnwind)
		}
		return d.LocalUnwind
	}
	u := run("SUPPORTED")
	assertDepthAmount(t, u.MarkedPnl, "-3")
	assertDepthAmount(t, u.NetQuoteCash, "-3")
	assertDepthAmount(t, u.CostVsEntryMark, "13")
	assertDepthAmount(t, u.ExtraQuoteRequired, "2")
	assertDepthAmount(t, u.SellBack.ChangeFromStart.QuoteChange, "-1")
	assertDepthAmount(t, u.BuyBack.ChangeFromStart.QuoteChange, "-2")
	assertDepthAmount(t, u.SellBack.ChangeFromStart.BaseChange, "0")
	assertDepthAmount(t, u.BuyBack.ChangeFromStart.BaseChange, "0")
	b.AvailableQuote = n(1)
	assertDepthAmount(t, run("UNSUPPORTED").ExtraQuoteRequired, "2")
	b.AvailableQuote = nil
	assertDepthAmount(t, run("UNKNOWN").ExtraQuoteRequired, "2")
	b.FeeBps, b.FeeCurrency = nil, ""
	b.SellTakerFee = &commonpb.SpotTakerFee{Bps: n(-200), Currency: "USDT"}
	run("UNKNOWN") // No buy-back fee.
	b.BuyTakerFee = &commonpb.SpotTakerFee{Bps: n(0), Currency: "USDT"}
	u = run("SUPPORTED") // Settled sale with rebate covers the full buy-back.
	assertDepthAmount(t, u.ExtraQuoteRequired, "0")
	assertDepthAmount(t, u.MarkedPnl, "-0.8")
	a.SellPriceRange = &querypb.SpotExecutionPriceRange{Minimum: n(100)}
	run("UNSUPPORTED")
	a.SellPriceRange = nil
	b.Book.Asks[0].Size = &commonpb.Decimal{Coefficient: 9, Exponent: -1}
	run("UNSUPPORTED")
}

func TestLocalInventoryRoundTripNativeFeeAndLot(t *testing.T) {
	s := depthExample()
	r, err := EvaluateSpotDepth(s)
	if err != nil {
		t.Fatal(err)
	}
	u := r.Directions[0].LocalUnwind
	if u.GetStatus() != "SUPPORTED" {
		t.Fatal(u)
	}
	// 0.9992 BTC acquired. Selling 0.9984 plus 8 bps leaves 0.00000128 on OKX.
	assertDepthAmount(t, u.SellBack.Quantity, "0.9984")
	assertDepthAmount(t, u.SellBack.Fee.Amount, "0.00079872")
	assertDepthAmount(t, u.SellBack.ChangeFromStart.BaseChange, "0.00000128")
	assertDepthAmount(t, u.BuyBack.Quantity, "0.9992")
	assertDepthAmount(t, u.BuyBack.Fee.Amount, "24.999984")
	assertDepthAmount(t, u.BuyBack.ChangeFromStart.BaseChange, "0")
	if u.SellBack.Fee.CurrencyCode != "BTC" || u.BuyBack.Fee.CurrencyCode != "USDT" {
		t.Fatal(u)
	}
	v := r.Directions[1].LocalUnwind
	if v.GetStatus() != "SUPPORTED" {
		t.Fatal(v)
	}
	// Buy back enough to replace the opening sale and both base fees.
	assertDepthAmount(t, v.BuyBack.Quantity, "1.0008")
}

func TestDepthTakerPriceRangeExpiresAtFirstViolation(t *testing.T) {
	n := func(c int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: c} }
	s := depthExample()
	s.RequestedBuyQuantity = n(2)
	for _, venue := range s.Venues {
		venue.FeeBps = n(0)
	}
	binance := s.Venues[1]
	// Both first levels are on the inclusive bounds. Second levels are outside.
	binance.BuyPriceRange = &querypb.SpotExecutionPriceRange{Maximum: n(50040)}
	binance.SellPriceRange = &querypb.SpotExecutionPriceRange{Minimum: n(50030)}
	run := func() []*querypb.SpotDepthDirection {
		t.Helper()
		r, err := EvaluateSpotDepth(s)
		if err != nil {
			t.Fatal(err)
		}
		return r.Directions
	}
	for _, d := range run() {
		if d.Status != "SUPPORTED" {
			t.Fatal(d)
		}
		assertDepthAmount(t, d.BuyQuantity, "1")
		assertDepthAmount(t, d.SellQuantity, "1")
		assertDepthAmount(t, d.Shortfall, "1")
	}
	// An in-range second bid cannot be reached through a rejected first bid.
	binance.SellPriceRange = &querypb.SpotExecutionPriceRange{Maximum: n(50020)}
	got := run()
	if got[0].Status != "UNSUPPORTED" || got[0].Funding != nil || got[1].Status != "SUPPORTED" {
		t.Fatal(got)
	}
	assertDepthAmount(t, got[0].Shortfall, "2")
	for _, invalid := range []*querypb.SpotExecutionPriceRange{
		{}, {Minimum: n(0)}, {Maximum: n(-1)}, {Minimum: n(50040), Maximum: n(50030)},
	} {
		binance.BuyPriceRange = invalid
		if got := run()[1]; got.Status != "UNKNOWN" || got.MarkedNetAfterRebalance != nil {
			t.Fatal(got)
		}
	}
}

func TestDepthScenarioNativeFeesAndOneLegLoss(t *testing.T) {
	s := depthExample()
	r, err := EvaluateSpotDepth(s)
	if err != nil {
		t.Fatal(err)
	}
	a, b := r.Directions[0], r.Directions[1]
	if a.Status != "SUPPORTED" || b.Status != "SUPPORTED" || a.BuyFee.CurrencyCode != "BTC" || a.SellFee.CurrencyCode != "USDT" {
		t.Fatal(r)
	}
	assertDepthAmount(t, a.SellQuantity, "0.9992")
	assertDepthAmount(t, a.SellNotional, "49989.976")
	assertDepthAmount(t, a.SellFee.Amount, "24.994988")
	assertDepthAmount(t, a.TotalFeesInQuote, "64.994988")
	assertDepthAmount(t, a.CashChange, "-35.018988")
	assertDepthAmount(t, a.BuyVenueChange.BaseChange, "0.9992")
	assertDepthAmount(t, a.BuyVenueChange.QuoteChange, "-50000")
	assertDepthAmount(t, a.SellVenueChange.BaseChange, "-0.9992")
	assertDepthAmount(t, a.SellVenueChange.QuoteChange, "49964.981012")
	assertDepthAmount(t, a.ResidualBase, "0")
	assertDepthAmount(t, a.MarkedNetAfterRebalance, "-36.018988")
	assertDepthAmount(t, a.BuyOnly.MarkPrice, "49480.2")
	assertDepthAmount(t, a.SellOnly.MarkPrice, "50540.4")
	assertDepthAmount(t, a.BuyOnly.MarkedPnl, "-559.384160")
	assertDepthAmount(t, a.SellOnly.MarkedPnl, "-534.986668")
	assertDepthAmount(t, a.PartialSell.InventoryChange, "0.4996")
	assertDepthAmount(t, a.PartialSell.MarkedPnl, "-297.201574")
	assertDepthAmount(t, b.ResidualBase, "0.00000064")
	assertDepthAmount(t, b.MarkedNetAfterRebalance, "-125.9720128")
	again, err := EvaluateSpotDepth(proto.Clone(s).(*querypb.SpotDepthScenario))
	if err != nil || !proto.Equal(r, again) {
		t.Fatal("unstable scenario")
	}
	s.RebalanceCost = nil
	s.SellFillBps = nil
	again, err = EvaluateSpotDepth(s)
	if err != nil {
		t.Fatal(err)
	}
	a = again.Directions[0]
	if a.MarkedNetAfterRebalance != nil || a.PartialSell != nil || a.BuyOnly == nil || again.InputSha256 == r.InputSha256 {
		t.Fatal(again)
	}
}

func TestSpotDepthFundingMinimumsAndCoverage(t *testing.T) {
	s := depthExample()
	a, b := s.Venues[0], s.Venues[1]
	// Cross into a second ask, with an explicitly funded quote fee.
	s.RequestedBuyQuantity = &commonpb.Decimal{Coefficient: 2}
	a.FeeCurrency = "USDT"
	a.FeeBps = &commonpb.Decimal{Coefficient: 10}
	b.FeeBps = &commonpb.Decimal{}
	a.AvailableQuote = &commonpb.Decimal{Coefficient: 75080005, Exponent: -3} // (50000+0.5*50010)*1.001
	r, err := EvaluateSpotDepth(s)
	if err != nil {
		t.Fatal(err)
	}
	d := r.Directions[0]
	assertDepthAmount(t, d.BuyQuantity, "1.5")
	assertDepthAmount(t, d.Shortfall, "0.5")
	assertDepthAmount(t, d.SellNotional, "75040")
	assertDepthAmount(t, d.BuyFee.Amount, "75.005")
	b.AvailableBase = &commonpb.Decimal{Coefficient: 75, Exponent: -2}
	b.FeeCurrency = "BTC"
	b.FeeBps = &commonpb.Decimal{Coefficient: -10}
	r, err = EvaluateSpotDepth(s)
	if err != nil {
		t.Fatal(err)
	}
	assertDepthAmount(t, r.Directions[0].SellFee.Amount, "-0.00075")
	assertDepthAmount(t, r.Directions[0].SellQuantity, "0.75")
	b.MinimumNotional = &commonpb.Decimal{Coefficient: 100000}
	r, err = EvaluateSpotDepth(s)
	if err != nil || r.Directions[0].Status != "UNSUPPORTED" || r.Directions[0].CashChange != nil {
		t.Fatal(r, err)
	}
	b.AvailableBase = nil
	r, err = EvaluateSpotDepth(s)
	if err != nil || r.Directions[0].Status != "UNKNOWN" {
		t.Fatal(r, err)
	}
}

func TestSpotFundedCapitalFixedFillBreakEvenAndLossBudget(t *testing.T) {
	s := depthExample()
	d := func(text string) *commonpb.Decimal {
		t.Helper()
		v, ok := dec.ParseProtoExact(text)
		if !ok {
			t.Fatal(text)
		}
		return v
	}
	level := func(price, size string) *marketpb.PriceLevel {
		return &marketpb.PriceLevel{Price: d(price), Size: d(size)}
	}
	buy, sell := s.Venues[0], s.Venues[1]
	buy.Book.Asks, buy.Book.Bids = []*marketpb.PriceLevel{level("100", "2")}, []*marketpb.PriceLevel{level("99", "2")}
	sell.Book.Bids = []*marketpb.PriceLevel{level("120", "0.5"), level("100", "1.5")}
	sell.Book.Asks = []*marketpb.PriceLevel{level("121", "2")}
	buy.FeeBps, buy.FeeCurrency = d("100"), "USDT"
	sell.FeeBps, sell.FeeCurrency = d("100"), "BTC"
	s.RequestedBuyQuantity, s.AdverseMoveBps, s.OneLegLossLimit = d("1.01"), d("3000"), d("48.873")
	evaluate := func() *querypb.SpotDepthDirection {
		t.Helper()
		r, err := EvaluateSpotDepth(s)
		if err != nil || r.Directions[0].Status != "SUPPORTED" {
			t.Fatal(r, err)
		}
		return r.Directions[0]
	}
	r := evaluate()
	// The purchase costs 101 plus 1.01 in quote. Selling 1 BTC at an
	// average of 110 consumes 1.01 BTC. Cash net of restoration is 6.99.
	assertDepthAmount(t, r.Funding.BuyQuoteRequired, "102.01")
	assertDepthAmount(t, r.Funding.SellBaseRequired, "1.01")
	assertDepthAmount(t, r.Funding.SellBaseValueQuote, "111.1")
	assertDepthAmount(t, r.Funding.FundedCapitalQuote, "213.11")
	assertDepthAmount(t, r.Funding.GrossTurnoverQuote, "211")
	assertDepthAmount(t, r.Funding.ReturnOnCapitalBps, "327.999625")
	assertDepthAmount(t, r.Funding.BreakEvenCostPerLegBps, "331.279621")
	assertDepthAmount(t, r.WorstOneLegLoss, "48.873")
	if r.OneLegLimitStatus != "WITHIN" {
		t.Fatal(r)
	}
	s.OneLegLossLimit = d("48.8729999999999999")
	if evaluate().OneLegLimitStatus != "EXCEEDED" {
		t.Fatal("rounded away a limit breach")
	}
	s.RebalanceCost = d("7.99")
	r = evaluate()
	assertDepthAmount(t, r.Funding.ReturnOnCapitalBps, "0")
	assertDepthAmount(t, r.Funding.BreakEvenCostPerLegBps, "0")
	s.RebalanceCost = d("14.98")
	assertDepthAmount(t, evaluate().Funding.BreakEvenCostPerLegBps, "-331.279621")
	s.RebalanceCost, s.AdverseMoveBps = nil, nil
	r = evaluate()
	if r.Funding.ReturnOnCapitalBps != nil || r.Funding.BreakEvenCostPerLegBps != nil ||
		r.WorstOneLegLoss != nil || r.OneLegLimitStatus != "UNKNOWN" {
		t.Fatal(r)
	}
	assertDepthAmount(t, r.MarkedNetBeforeRebalance, "7.99")
	if r.MarkedNetAfterRebalance != nil {
		t.Fatal("missing restoration cost produced a final net")
	}
	// No planned fee rebate can pre-fund either venue. Both must supply
	// the gross size and the full purchase price before proceeds arrive.
	buy.FeeBps, sell.FeeBps = d("-100"), d("-100")
	r = evaluate()
	assertDepthAmount(t, r.Funding.BuyQuoteRequired, "101")
	assertDepthAmount(t, r.Funding.SellBaseRequired, "1.0202")
	assertDepthAmount(t, r.Funding.SellBaseValueQuote, "112.02")
	s.OneLegLossLimit = d("-0.01")
	if _, err := EvaluateSpotDepth(s); err == nil {
		t.Fatal("negative loss limit accepted")
	}
}

func TestSpotDepthDirectionalTakerFees(t *testing.T) {
	s := depthExample()
	n := func(c int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: c} }
	fee := func(bps int64, currency string) *commonpb.SpotTakerFee {
		return &commonpb.SpotTakerFee{Bps: n(bps), Currency: currency}
	}
	a, b := s.Venues[0], s.Venues[1]
	for _, v := range s.Venues {
		v.FeeBps, v.FeeCurrency = nil, ""
		v.Book.Bids, v.Book.Asks = v.Book.Bids[:1], v.Book.Asks[:1]
	}
	a.Book.Bids[0].Price, a.Book.Asks[0].Price = n(9999), n(10000)
	b.Book.Bids[0].Price, b.Book.Asks[0].Price = n(10010), n(10011)
	a.BuyTakerFee, a.SellTakerFee = fee(0, "BTC"), fee(-1, "USDT")
	b.BuyTakerFee, b.SellTakerFee = fee(5, "BTC"), fee(20, "USDT")
	s.RebalanceCost = n(0)
	evaluate := func() []*querypb.SpotDepthDirection {
		t.Helper()
		out, err := EvaluateSpotDepth(s)
		if err != nil {
			t.Fatal(err)
		}
		return out.Directions
	}
	d := evaluate()
	// One BTC costs 10000, sells for 10010 and incurs 20.02 USDT on sale.
	assertDepthAmount(t, d[0].GrossQuoteCashChange, "10")
	assertDepthAmount(t, d[0].MarkedNetAfterRebalance, "-10.02")
	// The reverse buy delivers 0.9995 BTC, earning 9994.0005 plus a
	// 0.99940005 USDT rebate on sale, against the 10011 USDT purchase.
	assertDepthAmount(t, d[1].BuyFee.Amount, "0.0005")
	assertDepthAmount(t, d[1].SellFee.Amount, "-0.99940005")
	assertDepthAmount(t, d[1].ResidualBase, "0")
	assertDepthAmount(t, d[1].MarkedNetAfterRebalance, "-16.00009995")
	if d[1].BuyFee.CurrencyCode != "BTC" || d[1].SellFee.CurrencyCode != "USDT" {
		t.Fatal(d[1])
	}
	b.SellTakerFee = nil
	d = evaluate()
	if d[0].Status != "UNKNOWN" || d[0].MarkedNetAfterRebalance != nil || d[1].Status != "SUPPORTED" {
		t.Fatal(d)
	}
	b.FeeBps = n(0)
	d = evaluate()
	if d[0].Status != "UNKNOWN" || d[1].Status != "UNKNOWN" {
		t.Fatal("conflicting shared and side-specific terms accepted")
	}
}

func TestSpotFundingKeepsNativeAmountsWhenTheirSumExceedsWireRange(t *testing.T) {
	one := big.NewRat(1, 1)
	f := spotFunding(big.NewRat(9000000000000000000, 1), big.NewRat(9223372036854775807, 1),
		one, one, one, &commonpb.Decimal{Coefficient: 1})
	assertDepthAmount(t, f.BuyQuoteRequired, "9000000000000000000")
	assertDepthAmount(t, f.SellBaseValueQuote, "9223372036854775807")
	if f.FundedCapitalQuote != nil || f.GrossTurnoverQuote != nil || f.ReturnOnCapitalBps != nil || f.BreakEvenCostPerLegBps != nil {
		t.Fatal("unrepresentable denominator was reported as usable", f)
	}
}

func TestSpotDepthRefusesInvalidReferences(t *testing.T) {
	for name, change := range map[string]func(*querypb.SpotDepthScenario){
		"stale": func(s *querypb.SpotDepthScenario) { s.AsOf = timestamppb.New(s.AsOf.AsTime().Add(3 * time.Second)) },
		"future receipt": func(s *querypb.SpotDepthScenario) {
			s.Venues[0].KnownAt = timestamppb.New(s.AsOf.AsTime().Add(time.Nanosecond))
		},
		"clock skew": func(s *querypb.SpotDepthScenario) {
			s.Venues[0].Book.EventTime = timestamppb.New(s.AsOf.AsTime().Add(-1100 * time.Millisecond))
		},
		"wrong token":      func(s *querypb.SpotDepthScenario) { s.Venues[0].BaseAssetRef = "wrapped-btc" },
		"derivative":       func(s *querypb.SpotDepthScenario) { s.Venues[0].ProductType = "PERPETUAL" },
		"unknown sequence": func(s *querypb.SpotDepthScenario) { s.Venues[0].Book.LastUpdateSequence = 0 },
		"crossed":          func(s *querypb.SpotDepthScenario) { s.Venues[0].Book.Bids[0].Price = s.Venues[0].Book.Asks[0].Price },
		"unordered":        func(s *querypb.SpotDepthScenario) { s.Venues[0].Book.Asks[1] = s.Venues[0].Book.Asks[0] },
		"invalid move":     func(s *querypb.SpotDepthScenario) { s.AdverseMoveBps = &commonpb.Decimal{Coefficient: 10001} },
	} {
		t.Run(name, func(t *testing.T) {
			s := depthExample()
			change(s)
			if _, err := EvaluateSpotDepth(s); err == nil {
				t.Fatal("scenario accepted")
			}
		})
	}
}

func TestMarketSizeLimitsUseTheirOwnCurrencyAndReferencePrice(t *testing.T) {
	n := func(s string) *commonpb.Decimal {
		t.Helper()
		d, ok := dec.ParseProtoExact(s)
		if !ok {
			t.Fatal(s)
		}
		return d
	}
	s := depthExample()
	for _, v := range s.Venues {
		v.FeeBps, v.MinimumNotional = n("0"), n("0")
	}
	// Orders are sized in BTC. A 50-USD cap at 60000 USD/BTC permits
	// 1/1200 BTC, or 0.0008 BTC after rounding down to 0.0001 BTC lots.
	okx, binance := s.Venues[0], s.Venues[1]
	okx.MarketValueLimit = &querypb.SpotMarketValueLimit{Currency: "USD", ReferencePrice: n("60000"), MaximumValue: n("50")}
	binance.MarketValueLimit = &querypb.SpotMarketValueLimit{Currency: "USDT", ReferencePrice: n("10000"), MinimumValue: n("8")}
	run := func() []*querypb.SpotDepthDirection {
		t.Helper()
		out, err := EvaluateSpotDepth(s)
		if err != nil {
			t.Fatal(err)
		}
		return out.Directions
	}
	for _, d := range run() {
		if d.Status != "SUPPORTED" {
			t.Fatal(d)
		}
		assertDepthAmount(t, d.BuyQuantity, "0.0008")
		assertDepthAmount(t, d.SellQuantity, "0.0008")
		assertDepthAmount(t, d.Shortfall, "0.9992")
	}
	// Both book notionals still exceed 8 USDT. The exchange reference
	// price moves below the minimum by one unit of its reported precision.
	binance.MarketValueLimit.ReferencePrice = n("9999.99999999")
	for _, d := range run() {
		if d.Status != "UNSUPPORTED" || d.CashChange != nil {
			t.Fatal(d)
		}
	}
	binance.MarketValueLimit = nil
	binance.MaximumQuantity = n("0.00059999")
	for _, d := range run() {
		assertDepthAmount(t, d.SellQuantity, "0.0005")
	}
	okx.MarketValueLimit.ReferencePrice = nil
	for _, d := range run() {
		if d.Status != "UNKNOWN" || d.CashChange != nil {
			t.Fatal("missing USD reference became quote price", d)
		}
	}
}

func TestSpotRuleEvidencePreservesCutoffAndReplay(t *testing.T) {
	s := depthExample()
	plain, err := EvaluateSpotDepth(s)
	if err != nil {
		t.Fatal(err)
	}
	ref := &querypb.SpotRuleEvidence{Source: "public venue rules", KnownAt: s.AsOf, ResponseSha256: plain.InputSha256}
	s.Venues[0].RuleEvidence = []*querypb.SpotRuleEvidence{ref}
	withSource, err := EvaluateSpotDepth(s)
	if err != nil || plain.InputSha256 == withSource.InputSha256 || !proto.Equal(plain.Directions[0], withSource.Directions[0]) {
		t.Fatal("source evidence was lost or changed the arithmetic", err)
	}
	ref.KnownAt = timestamppb.New(s.AsOf.AsTime().Add(time.Nanosecond))
	if _, err := EvaluateSpotDepth(s); err == nil {
		t.Fatal("future rule evidence accepted")
	}
}
