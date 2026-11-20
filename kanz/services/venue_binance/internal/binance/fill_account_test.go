package binance

import (
	"context"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// Low-level response conversion tests supply the storage seam. Restart and
// acknowledgement identity are qualified against deployed PostgreSQL.
type fixturePlacements struct{}

func (fixturePlacements) AcknowledgePlacement(context.Context, *orderpb.OrderState, orderview.Placement, orderview.PlacementSource) error {
	return nil
}
func (fixturePlacements) CheckPlacement(context.Context, string, orderview.Placement) error {
	return nil
}

const fixtureAccount = "fixture-account"

// Isolated conversion tests supply the already verified account. The startup
// and signed-stream tests exercise the exchange proof on a loopback server.
func provenBinanceFixture(cfg BinanceConfig) *BinanceVenue {
	cfg.Account = fixtureAccount
	v := NewBinanceVenue(cfg)
	uid := "12345678"
	v.verifiedUID.Store(&uid)
	v.placements = fixturePlacements{}
	return v
}

func fixtureUserDataIngester(cfg UserDataConfig) *UserDataIngester {
	cfg.provenAccount = fixtureAccount
	return newUserDataIngester(cfg)
}
