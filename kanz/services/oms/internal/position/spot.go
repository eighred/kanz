package position

import (
	"context"
	"fmt"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func (t positionTrade) currency(fallback string) string {
	if t.quote != "" {
		return t.quote
	}
	return fallback
}

func missingPositionFX(quote, reporting string) error {
	return fmt.Errorf("position: cannot value %s holdings in %s without an exchange rate", quote, reporting)
}

func saveLot(ctx context.Context, tx pgx.Tx, portfolio, venue, instrument, quote string, l *lot) error {
	_, err := tx.Exec(ctx, `INSERT INTO positions
		(portfolio_id,venue,instrument_id,quantity,average_price,realized_pnl,spot_quote)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (tenant_id,portfolio_id,venue,instrument_id) DO UPDATE SET
		quantity=EXCLUDED.quantity,average_price=EXCLUDED.average_price,
		realized_pnl=EXCLUDED.realized_pnl,spot_quote=EXCLUDED.spot_quote,updated_at=now()`,
		portfolio, venue, instrument, l.Qty.RatString(), l.AvgCost.RatString(), l.Realized.RatString(), quote)
	return err
}

// A lot posted by an earlier writer can be classified only by replaying its
// complete venue evidence. Rebuild every affected venue under the portfolio's
// instrument lock so the next aggregate includes all corrections together.
func alignSpotLots(ctx context.Context, tx pgx.Tx, portfolio, currentVenue, instrument, quote string) ([]*domainpb.PositionState, error) {
	rows, err := tx.Query(ctx, `SELECT venue,COALESCE(spot_quote,'') FROM positions WHERE
		portfolio_id=$1 AND instrument_id=$2 AND spot_quote IS DISTINCT FROM $3 ORDER BY venue`, portfolio, instrument, quote)
	if err != nil {
		return nil, err
	}
	var venues []string
	for rows.Next() {
		var venue, oldQuote string
		if err := rows.Scan(&venue, &oldQuote); err != nil {
			rows.Close()
			return nil, err
		}
		if oldQuote != "" {
			rows.Close()
			return nil, ErrPositionHistoryUnknown
		}
		venues = append(venues, venue)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if quote == "" && len(venues) > 0 {
		if err := checkUnclassified(ctx, tx, portfolio, instrument); err != nil {
			return nil, err
		}
		_, err := tx.Exec(ctx, `UPDATE positions SET spot_quote='' WHERE portfolio_id=$1 AND instrument_id=$2 AND spot_quote IS NULL`, portfolio, instrument)
		return nil, err
	}
	var restated []*domainpb.PositionState
	for _, venue := range venues {
		fold, err := replayExecutions(ctx, tx, portfolio, venue, instrument, quote)
		if err != nil {
			return nil, err
		}
		if err := saveLot(ctx, tx, portfolio, venue, instrument, quote, fold.lot); err != nil {
			return nil, err
		}
		if venue != currentVenue {
			state, err := positionState(portfolio, venue, instrument, fold.lot, fold.price, fold.asOf, quote)
			if err != nil {
				return nil, err
			}
			restated = append(restated, state)
		}
	}
	return restated, nil
}

// An earlier candidate could have received spot evidence while folding gross
// size. Never publish or spend such a stored lot before its first native replay.
func checkUnclassified(ctx context.Context, tx pgx.Tx, portfolio, instrument string) error {
	rows, err := tx.Query(ctx, `SELECT h.fill FROM positions p
		JOIN position_execution_history h USING (tenant_id,portfolio_id,venue,instrument_id)
		WHERE p.portfolio_id=$1 AND p.spot_quote IS NULL AND ($2='' OR p.instrument_id=$2)`, portfolio, instrument)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return err
		}
		var f orderpb.Fill
		if proto.Unmarshal(data, &f) != nil || f.GetSpotVenueSymbol() != "" {
			return ErrPositionHistoryUnknown
		}
	}
	return rows.Err()
}
