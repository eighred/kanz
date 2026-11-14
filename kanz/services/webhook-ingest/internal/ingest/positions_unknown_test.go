package ingest

import (
	"errors"
	"math/big"
	"testing"

	"github.com/eighred/kanz/internal/signal/translate"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

func unreadablePositionQuantities() map[string]*commonpb.Decimal {
	return map[string]*commonpb.Decimal{"missing": nil, "large_exponent": {Coefficient: 1, Exponent: 2_000_000_000}, "small_exponent": {Coefficient: 1, Exponent: -2_000_000_000}}
}

func invalidatePosition(t *testing.T, cache *PositionCache, venue string, quantity *commonpb.Decimal) {
	t.Helper()
	payload, err := proto.Marshal(&domainpb.PositionState{PortfolioId: "fund-alpha", Venue: venue, InstrumentId: "BTC-USD", Quantity: quantity})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Handle(t.Context(), nil, payload); err == nil {
		t.Fatal("unreadable position was acknowledged")
	}
}

func TestUnreadablePositionIsUnknownUntilValidRepair(t *testing.T) {
	for name, quantity := range unreadablePositionQuantities() {
		t.Run(name, func(t *testing.T) {
			for _, seeded := range []bool{false, true} {
				label := "cold"
				if seeded {
					label = "prior"
				}
				t.Run(label, func(t *testing.T) {
					cache := NewPositionCache()
					if seeded {
						feed(t, cache, "fund-alpha", "BINANCE", "BTC-USD", 4)
					}
					feed(t, cache, "fund-alpha", "OKX", "BTC-USD", 2)
					feed(t, cache, "other-fund", "BINANCE", "BTC-USD", 5)
					feed(t, cache, "fund-alpha", "BINANCE", "ETH-USD", 7)
					invalidatePosition(t, cache, "BINANCE", quantity)
					cache.Arm()
					got, err := cache.Position(t.Context(), "fund-alpha", "BINANCE", "BTC-USD")
					if !errors.Is(err, translate.ErrUnresolvable) || got != nil {
						t.Fatalf("unreadable latest level became readable: %v %v", got, err)
					}
					other, err := cache.Position(t.Context(), "fund-alpha", "OKX", "BTC-USD")
					if err != nil || other.Cmp(big.NewRat(2, 1)) != 0 {
						t.Fatalf("another venue was invalidated: %v %v", other, err)
					}
					for _, key := range []struct {
						fund, instrument string
						quantity         int64
					}{{"other-fund", "BTC-USD", 5}, {"fund-alpha", "ETH-USD", 7}} {
						other, err := cache.Position(t.Context(), key.fund, "BINANCE", key.instrument)
						if err != nil || other.Cmp(big.NewRat(key.quantity, 1)) != 0 {
							t.Fatalf("another fund/instrument was invalidated: %v %v", other, err)
						}
					}
					for _, repair := range []*big.Rat{big.NewRat(7, 10), big.NewRat(-7, 10), new(big.Rat)} {
						feedRat(t, cache, "fund-alpha", "BINANCE", "BTC-USD", repair)
						got, err = cache.Position(t.Context(), "fund-alpha", "BINANCE", "BTC-USD")
						if err != nil || got.Cmp(repair) != 0 {
							t.Fatalf("exact repair failed: got=%v err=%v want=%v", got, err, repair)
						}
					}
				})
			}
		})
	}
}

func TestUnknownCloseReleasesNonceBeforeAnyEffects(t *testing.T) {
	for name, quantity := range unreadablePositionQuantities() {
		t.Run(name, func(t *testing.T) {
			for _, venue := range []string{"BINANCE", "OKX"} {
				t.Run(venue, func(t *testing.T) {
					cache := NewPositionCache()
					cache.Arm()
					feedRat(t, cache, "fund-alpha", "BINANCE", "BTC-USD", big.NewRat(3, 5))
					feedRat(t, cache, "fund-alpha", "OKX", "BTC-USD", big.NewRat(-2, 5))
					invalidatePosition(t, cache, venue, quantity)
					pub := &brokenPublisher{}
					pipeline := pipelineWithPositions(t, cache, pub)
					raw := body("close", "1", "absolute_qty", "unknown-close")
					for range 2 {
						err := post(t, pipeline, raw)
						if err == nil || errors.Is(err, ErrReplayed) || decided(err) || len(pub.events) != 0 {
							t.Fatalf("unknown close burned nonce or emitted effects: %v events=%d", err, len(pub.events))
						}
					}
					if venue == "BINANCE" {
						feedRat(t, cache, "fund-alpha", venue, "BTC-USD", big.NewRat(3, 5))
					} else {
						feedRat(t, cache, "fund-alpha", venue, "BTC-USD", big.NewRat(-2, 5))
					}
					if err := post(t, pipeline, raw); err != nil {
						t.Fatalf("same nonce could not retry after repair: %v", err)
					}
					commands := pub.submitted()
					if len(pub.events) != 3 || len(commands) != 2 {
						t.Fatalf("retry effects=%d commands=%d", len(pub.events), len(commands))
					}
					for _, cmd := range commands {
						wantSide := orderpb.Side_SIDE_SELL
						if cmd.Venue == "OKX" {
							wantSide = orderpb.Side_SIDE_BUY
						}
						if cmd.Side != wantSide || cmd.OrderId != translate.DeterministicID(translate.DeterministicID("momentum", "unknown-close"), cmd.Venue) {
							t.Fatalf("close identity/direction changed: %v", cmd)
						}
					}
					if err := post(t, pipeline, raw); !errors.Is(err, ErrReplayed) {
						t.Fatalf("successful retry did not commit nonce: %v", err)
					}
				})
			}
		})
	}
}
