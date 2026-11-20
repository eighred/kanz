package binance

import (
	"context"
	"errors"
	"fmt"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

type placementStore interface {
	AcknowledgePlacement(context.Context, *orderpb.OrderState, orderview.Placement, orderview.PlacementSource) error
	CheckPlacement(context.Context, string, orderview.Placement) error
}

func (v *BinanceVenue) placementIdentity(symbol string, orderID int64) orderview.Placement {
	uid := v.verifiedUID.Load()
	if uid == nil {
		return orderview.Placement{}
	}
	return orderview.Placement{AccountUID: *uid, Symbol: symbol, OrderID: orderID}
}

// The client order ID can be reused at the venue. All signed order lookups,
// including background reconciliation, must respect a previously observed exchange ID.
func (v *BinanceVenue) checkPlacement(ctx context.Context, clientID, symbol string, orderID int64) error {
	if v.placements == nil {
		return fmt.Errorf("%w: placement evidence unavailable", execution.ErrReconcileEvidence)
	}
	err := v.placements.CheckPlacement(ctx, clientID, v.placementIdentity(symbol, orderID))
	if errors.Is(err, orderview.ErrPlacementConflict) {
		return fmt.Errorf("%w: %w", execution.ErrReconcileEvidence, err)
	}
	return err
}

func (v *BinanceVenue) acknowledge(ctx context.Context, st *orderpb.OrderState, symbol string, orderID int64, source orderview.PlacementSource) error {
	if v.placements == nil {
		return fmt.Errorf("%w: placement evidence unavailable", execution.ErrReconcileEvidence)
	}
	return v.placements.AcknowledgePlacement(ctx, st, v.placementIdentity(symbol, orderID), source)
}
