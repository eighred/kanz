package orderview

import (
	"context"
	"errors"
	"fmt"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Placement identifies the exchange observation within its account and symbol.
type Placement struct {
	AccountUID string
	Symbol     string
	OrderID    int64
}

type PlacementSource string

const (
	PlacementResponse PlacementSource = "placement_response"
	RecoveryQuery     PlacementSource = "recovery_query"
	ExecutionReport   PlacementSource = "execution_report"
)

type PlacementReceipt struct {
	Placement
	Source     PlacementSource
	RecordedAt time.Time
}

var ErrPlacementConflict = errors.New("orderview: conflicting placement identity")

// VerifyPlacementStorage checks the deployed migration and the runtime role
// before the connector can dispatch an order that it may have to acknowledge.
func (p *Postgres) VerifyPlacementStorage(ctx context.Context) error {
	var ready bool
	err := p.pool.QueryRow(ctx, `SELECT bool_and(has_column_privilege('venue_orders', col, 'SELECT')
    AND has_column_privilege('venue_orders', col, 'UPDATE'))
    FROM unnest(ARRAY['placement_account_uid','placement_symbol','placement_order_id',
        'placement_source','placement_ack_at']) AS cols(col)`).Scan(&ready)
	if err != nil {
		return fmt.Errorf("verify placement storage: %w", err)
	}
	if !ready {
		return errors.New("orderview: runtime role cannot retain placement acknowledgements")
	}
	return nil
}

// PlacementFor returns only positively recorded acknowledgements. The legacy
// dispatch claim, including one left by a lost POST, does not populate these columns.
func (p *Postgres) PlacementFor(ctx context.Context, clientID string) (PlacementReceipt, bool, error) {
	var r PlacementReceipt
	err := p.pool.QueryRow(ctx, `SELECT placement_account_uid, placement_symbol, placement_order_id, placement_source, placement_ack_at
        FROM venue_orders WHERE order_id=$1 AND placement_ack_at IS NOT NULL`, clientID).
		Scan(&r.AccountUID, &r.Symbol, &r.OrderID, &r.Source, &r.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlacementReceipt{}, false, nil
	}
	return r, err == nil, err
}

// CheckPlacement checks both directions. An unacknowledged dispatch cannot
// adopt a venue order already bound to a different local client ID.
func (p *Postgres) CheckPlacement(ctx context.Context, clientID string, r Placement) error {
	if r.AccountUID == "" || r.Symbol == "" || r.OrderID <= 0 {
		return ErrPlacementConflict
	}
	var conflict bool
	err := p.pool.QueryRow(ctx, `SELECT
    EXISTS(SELECT 1 FROM venue_orders WHERE order_id=$1 AND placement_ack_at IS NOT NULL
        AND (placement_account_uid,placement_symbol,placement_order_id) IS DISTINCT FROM ($2::text,$3::text,$4::bigint))
    OR EXISTS(SELECT 1 FROM venue_orders WHERE placement_ack_at IS NOT NULL AND placement_account_uid=$2 AND placement_symbol=$3
        AND placement_order_id=$4 AND order_id<>$1)`, clientID, r.AccountUID, r.Symbol, r.OrderID).Scan(&conflict)
	if err != nil {
		return err
	}
	if conflict {
		return ErrPlacementConflict
	}
	return nil
}

// AcknowledgePlacement binds the admitted order to the first accepted exchange
// identity. It does not change economic state or the local order's revision.
func (p *Postgres) AcknowledgePlacement(ctx context.Context, st *orderpb.OrderState, r Placement, source PlacementSource) error {
	if r.AccountUID == "" || r.Symbol == "" || r.OrderID <= 0 {
		return ErrPlacementConflict
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var blob []byte
	var priorID *int64
	var priorUID, priorSymbol *string
	err = tx.QueryRow(ctx, `SELECT state,placement_account_uid,placement_symbol,placement_order_id
        FROM venue_orders WHERE order_id=$1 FOR UPDATE`, st.GetOrderId()).Scan(&blob, &priorUID, &priorSymbol, &priorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPlacementConflict
	}
	if err != nil {
		return err
	}
	current, err := decode(blob, st.GetOrderId())
	if err != nil {
		return err
	}
	if !SamePlacementScope(current, st) {
		return ErrPlacementConflict
	}

	if priorID != nil {
		if priorUID == nil || priorSymbol == nil || *priorUID != r.AccountUID || *priorSymbol != r.Symbol || *priorID != r.OrderID {
			return ErrPlacementConflict
		}
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE venue_orders SET placement_account_uid=$2, placement_symbol=$3,
        placement_order_id=$4, placement_source=$5, placement_ack_at=clock_timestamp() WHERE order_id=$1`,
		st.GetOrderId(), r.AccountUID, r.Symbol, r.OrderID, source)
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" && pgerr.ConstraintName == "venue_placement_source_identity" {
			return ErrPlacementConflict
		}
		return fmt.Errorf("record placement acknowledgement: %w", err)
	}
	return tx.Commit(ctx)
}

// SamePlacementScope checks the admission identity carried by the typed OMS
// request against the tenant's own durable order.
func SamePlacementScope(current, requested *orderpb.OrderState) bool {
	return current.GetOrderId() != "" && current.GetOrderId() == requested.GetOrderId() &&
		current.GetPortfolioId() != "" && current.GetPortfolioId() == requested.GetPortfolioId() &&
		current.GetVenue() != "" && current.GetVenue() == requested.GetVenue() &&
		current.GetVenueAccountId() != "" && current.GetVenueAccountId() == requested.GetVenueAccountId() &&
		current.GetInstrumentId() != "" && current.GetInstrumentId() == requested.GetInstrumentId() &&
		current.GetSide() != orderpb.Side_SIDE_UNSPECIFIED && current.GetSide() == requested.GetSide()
}
