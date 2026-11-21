package spotbooks

import (
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostPricingBookCheck(t *testing.T) {
	event := time.Unix(1800000000, 0)
	current := &querypb.SpotBookObservation{Status: "SUPPORTED", AsOf: timestamppb.New(event.Add(250 * time.Millisecond)),
		ProductType: "CRYPTO_SPOT", BaseAssetRef: "bitcoin", QuoteAssetRef: "tether"}
	priced := &querypb.SpotDepthScenario{AsOf: timestamppb.New(event.Add(time.Millisecond))}
	check := &querypb.SpotBookCheckRequest{MaxAgeMs: 250}
	for _, venue := range []string{"BINANCE", "OKX"} {
		level := func(p int64) *marketpb.PriceLevel {
			return &marketpb.PriceLevel{Price: &commonpb.Decimal{Coefficient: p}, Size: &commonpb.Decimal{Coefficient: 1}}
		}
		b := &marketpb.OrderBookSnapshot{InstrumentId: "BTC-USDT", Mic: venue, Symbol: map[string]string{"BINANCE": "BTCUSDT", "OKX": "BTC-USDT"}[venue],
			EventTime: timestamppb.New(event), LastUpdateSequence: 20, Bids: []*marketpb.PriceLevel{level(100)}, Asks: []*marketpb.PriceLevel{level(101)}}
		observed := &querypb.ObservedVenueBook{Book: b, EventId: venue + "-20", KnownAt: priced.AsOf}
		current.Venues = append(current.Venues, observed)
		// Exercise caller order independently of the cache's Binance, OKX order.
		priced.Venues = append([]*querypb.SpotDepthVenue{{Book: proto.CloneOf(b), KnownAt: observed.KnownAt,
			ProductType: current.ProductType, BaseAssetRef: current.BaseAssetRef, QuoteAssetRef: current.QuoteAssetRef}}, priced.Venues...)
		check.EventIds = append([]string{observed.EventId}, check.EventIds...)
	}
	run := func(want querypb.SpotBookCheckState) {
		t.Helper()
		got := Check(current, priced, check)
		if got.State != want || !proto.Equal(got.CheckedAt, current.AsOf) || got.MaxAgeMs != check.MaxAgeMs {
			t.Fatal(got)
		}
	}
	bid := priced.Venues[0].Book.Bids[0]
	bid.Price = &commonpb.Decimal{Coefficient: 1, Exponent: 2}
	run(querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_CURRENT)
	current.AsOf = timestamppb.New(current.AsOf.AsTime().Add(time.Nanosecond))
	run(querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_STALE)
	check.MaxAgeMs = 251
	bid.Size.Coefficient = 2
	run(querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_UNVERIFIED)
	bid.Size.Coefficient = 1
	check.EventIds[0] = "different"
	run(querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_UNVERIFIED)
	check.EventIds[0] = "OKX-20"
	current.Venues[0].Book.LastUpdateSequence++
	run(querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_SUPERSEDED)
	current.Status = "UNKNOWN"
	run(querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_UNVERIFIED)
	current.Refusal = querypb.SpotBookRefusal_SPOT_BOOK_REFUSAL_STALE
	run(querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_STALE)
	current.AsOf = nil // Unbound or expired grant.
	run(querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_UNVERIFIED)
}
