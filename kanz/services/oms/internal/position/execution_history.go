package position

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/costbasis"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

var ErrPositionHistoryUnknown = errors.New("position: chronological history is incomplete or exceeds the bounded replay limit")

const maxPositionReplay = 10000

type executionFold struct {
	lot   *lot
	price *big.Rat
	asOf  time.Time
}

// recordExecution runs under the existing instrument lock and fill transaction.
// Normal chronological fills retain the constant-work fold. A backdated fill
// replays the complete journal; an unknown legacy prefix is never guessed away.
func recordExecution(ctx context.Context, tx pgx.Tx, portfolio string, fill *orderpb.Fill, asOf time.Time, current *lot, trade positionTrade) (*executionFold, error) {
	key := fillfact.ExecutionKey(fill)
	when := asOf.UTC()
	proven := fill.GetExecutedAt() != nil && fill.GetExecutedAt().CheckValid() == nil && fill.GetExecutedAt().AsTime().Unix() > 0
	if proven {
		when = fill.GetExecutedAt().AsTime()
	}
	var complete bool
	var lastTime time.Time
	var lastKey string
	err := tx.QueryRow(ctx, `SELECT complete,last_execution_time,last_execution_key FROM position_execution_basis
		WHERE portfolio_id=$1 AND venue=$2 AND instrument_id=$3`, portfolio, fill.GetVenue(), fill.GetInstrumentId()).Scan(&complete, &lastTime, &lastKey)
	if errors.Is(err, pgx.ErrNoRows) {
		var existed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM positions WHERE portfolio_id=$1 AND venue=$2 AND instrument_id=$3)`, portfolio, fill.GetVenue(), fill.GetInstrumentId()).Scan(&existed); err != nil {
			return nil, err
		}
		complete = !existed
	} else if err != nil {
		return nil, err
	}
	if !proven {
		complete = false
	}
	backdated := when.Before(lastTime) || (when.Equal(lastTime) && key < lastKey)
	if (backdated || fill.GetRecovery() != nil) && !complete {
		return nil, ErrPositionHistoryUnknown
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(fill)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO position_execution_history
		(execution_key,raw_fill_id,portfolio_id,venue,instrument_id,execution_time,time_proven,fill)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, key, fill.GetFillId(), portfolio, fill.GetVenue(), fill.GetInstrumentId(), when, proven, data); err != nil {
		return nil, err
	}
	if fill.GetRecovery().GetFeeApproval() != nil {
		previous, err := fillfact.FeeRevisionTerms(fill)
		if err != nil {
			return nil, err
		}
		if _, _, err := fillfact.RecordFeeRevision(ctx, tx, "position", previous, fill); err != nil {
			return nil, err
		}
	}
	result := current
	price := trade.price
	if backdated {
		replayed, err := replayExecutions(ctx, tx, portfolio, fill.GetVenue(), fill.GetInstrumentId(), trade.quote)
		if err != nil {
			return nil, err
		}
		result, price = replayed.lot, replayed.price
	} else {
		costbasis.Fold(result, trade.quantity, trade.price)
		lastTime, lastKey = when, key
	}
	_, err = tx.Exec(ctx, `INSERT INTO position_execution_basis (portfolio_id,venue,instrument_id,complete,last_execution_time,last_execution_key)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (tenant_id,portfolio_id,venue,instrument_id) DO UPDATE
		SET complete=EXCLUDED.complete,last_execution_time=EXCLUDED.last_execution_time,last_execution_key=EXCLUDED.last_execution_key`, portfolio, fill.GetVenue(), fill.GetInstrumentId(), complete, lastTime, lastKey)
	return &executionFold{lot: result, price: price, asOf: lastTime}, err
}

type positionTrade struct {
	quantity, price *big.Rat
	quote           string // empty for the historical configured-currency fold
}

func tradeOf(fill *orderpb.Fill) (positionTrade, error) {
	if fill.GetSpotVenueSymbol() != "" {
		if fill.GetExecutedAt() == nil || fill.ExecutedAt.CheckValid() != nil || fill.ExecutedAt.AsTime().Unix() <= 0 {
			return positionTrade{}, ErrPositionHistoryUnknown
		}
		legs, err := fillfact.SettlementLegs(fill, "")
		if err != nil {
			return positionTrade{}, err
		}
		return positionTrade{quantity: legs.Quantity, price: legs.Price, quote: legs.Currency}, nil
	}
	quantity, qOK := dec.FromProtoChecked(fill.GetQuantity())
	price, pOK := dec.FromProtoChecked(fill.GetPrice())
	if !qOK || !pOK {
		return positionTrade{}, ErrPositionHistoryUnknown
	}
	if fill.GetSide() == orderpb.Side_SIDE_SELL {
		quantity.Neg(quantity)
	}
	return positionTrade{quantity: quantity, price: price}, nil
}

// Rebuild in exchange-time order, using the latest authorized fee for each
// execution. One ordered query observes the same locked transaction as the claim.
func replayExecutions(ctx context.Context, tx pgx.Tx, portfolio, venue, instrumentID, quote string) (*executionFold, error) {
	var complete bool
	err := tx.QueryRow(ctx, `SELECT complete FROM position_execution_basis
		WHERE portfolio_id=$1 AND venue=$2 AND instrument_id=$3`, portfolio, venue, instrumentID).Scan(&complete)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !complete) {
		return nil, ErrPositionHistoryUnknown
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT h.fill,h.time_proven,h.execution_time,r.revised_fill
		FROM position_execution_history h LEFT JOIN LATERAL (
			SELECT revised_fill FROM execution_fee_revisions
			WHERE book='position' AND execution_key=h.execution_key ORDER BY revision_sequence DESC LIMIT 1
		) r ON true WHERE h.portfolio_id=$1 AND h.venue=$2 AND h.instrument_id=$3
		ORDER BY h.execution_time,h.execution_key LIMIT $4`, portfolio, venue, instrumentID, maxPositionReplay+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := &executionFold{lot: zeroLot()}
	count := 0
	for rows.Next() {
		count++
		if count > maxPositionReplay {
			return nil, ErrPositionHistoryUnknown
		}
		var data, revision []byte
		var proven bool
		if err := rows.Scan(&data, &proven, &result.asOf, &revision); err != nil {
			return nil, err
		}
		var original orderpb.Fill
		if !proven || proto.Unmarshal(data, &original) != nil || original.GetVenue() != venue || original.GetInstrumentId() != instrumentID {
			return nil, ErrPositionHistoryUnknown
		}
		head := &original
		if revision != nil {
			var revised orderpb.Fill
			if proto.Unmarshal(revision, &revised) != nil || !fillfact.SameExecutionExceptFee(&original, &revised) {
				return nil, fillfact.ErrFeeRevision
			}
			head = &revised
		}
		trade, err := tradeOf(head)
		if err != nil {
			return nil, err
		}
		if trade.quote != quote {
			return nil, ErrPositionHistoryUnknown
		}
		costbasis.Fold(result.lot, trade.quantity, trade.price)
		result.price = trade.price
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrPositionHistoryUnknown
	}
	return result, nil
}

func verifyPositionDuplicate(ctx context.Context, tx pgx.Tx, portfolio string, fill *orderpb.Fill) (bool, error) {
	var data []byte
	var originalPortfolio string
	err := tx.QueryRow(ctx, `SELECT portfolio_id,fill FROM position_execution_history WHERE execution_key=$1`, fillfact.ExecutionKey(fill)).Scan(&originalPortfolio, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		if fill.GetRecovery() != nil || fill.GetSpotVenueSymbol() != "" {
			return false, ErrPositionHistoryUnknown
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var original orderpb.Fill
	if err := proto.Unmarshal(data, &original); err != nil {
		return false, err
	}
	if originalPortfolio != portfolio || original.GetSpotVenueSymbol() != fill.GetSpotVenueSymbol() {
		return false, fillfact.ErrExecutionIdentityConflict
	}
	if fill.GetRecovery().GetFeeApproval() != nil {
		_, fresh, err := fillfact.RecordFeeRevision(ctx, tx, "position", &original, fill)
		return fresh, err
	}
	head, err := fillfact.FeeHead(ctx, tx, "position", &original)
	if err != nil {
		return false, err
	}
	if !fillfact.SameExecution(head, fill) && !(fill.Recovery == nil && fillfact.SameExecution(&original, fill)) {
		return false, fillfact.ErrExecutionIdentityConflict
	}
	return false, nil
}

func latestExecutionMark(ctx context.Context, tx pgx.Tx, portfolio, instrument, venue string) (*big.Rat, time.Time, error) {
	var data []byte
	var at time.Time
	err := tx.QueryRow(ctx, `SELECT execution_time,fill FROM position_execution_history
		WHERE portfolio_id=$1 AND ($2='' OR venue=$2) AND instrument_id=$3 ORDER BY execution_time DESC,execution_key DESC LIMIT 1`,
		portfolio, venue, instrument).Scan(&at, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var latest orderpb.Fill
	if err := proto.Unmarshal(data, &latest); err != nil {
		return nil, time.Time{}, err
	}
	if _, ok := dec.InDomainDeep(&latest); !ok {
		return nil, time.Time{}, ErrPositionHistoryUnknown
	}
	return dec.FromProto(latest.GetPrice()), at, nil
}
