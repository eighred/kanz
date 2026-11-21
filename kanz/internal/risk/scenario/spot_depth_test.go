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
	assertDepthAmount(t, a.BuyOnly.MarkedPnl, "-509.923760")
	assertDepthAmount(t, a.SellOnly.MarkedPnl, "-494.618988")
	assertDepthAmount(t, a.PartialSell.InventoryChange, "0.4996")
	assertDepthAmount(t, a.PartialSell.MarkedPnl, "-272.471374")
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
