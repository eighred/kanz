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
func recordExecution(ctx context.Context, tx pgx.Tx, portfolio string, fill *orderpb.Fill, asOf time.Time, current *lot) (*executionFold, error) {
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
	price := dec.FromProto(fill.GetPrice())
	if backdated {
		rows, err := tx.Query(ctx, `SELECT fill,time_proven FROM position_execution_history
			WHERE portfolio_id=$1 AND venue=$2 AND instrument_id=$3 ORDER BY execution_time,execution_key LIMIT $4`, portfolio, fill.GetVenue(), fill.GetInstrumentId(), maxPositionReplay+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		result = zeroLot()
		count := 0
		for rows.Next() {
			count++
			if count > maxPositionReplay {
				return nil, ErrPositionHistoryUnknown
			}
			var blob []byte
			var timeProven bool
			if err := rows.Scan(&blob, &timeProven); err != nil {
				return nil, err
			}
			if !timeProven {
				return nil, ErrPositionHistoryUnknown
			}
			var execution orderpb.Fill
			if err := proto.Unmarshal(blob, &execution); err != nil {
				return nil, err
			}
			if _, ok := dec.InDomainDeep(&execution); !ok {
				return nil, ErrPositionHistoryUnknown
			}
			foldExecution(result, &execution)
			price = dec.FromProto(execution.GetPrice())
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	} else {
		foldExecution(result, fill)
		lastTime, lastKey = when, key
	}
	_, err = tx.Exec(ctx, `INSERT INTO position_execution_basis (portfolio_id,venue,instrument_id,complete,last_execution_time,last_execution_key)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (tenant_id,portfolio_id,venue,instrument_id) DO UPDATE
		SET complete=EXCLUDED.complete,last_execution_time=EXCLUDED.last_execution_time,last_execution_key=EXCLUDED.last_execution_key`, portfolio, fill.GetVenue(), fill.GetInstrumentId(), complete, lastTime, lastKey)
	return &executionFold{lot: result, price: price, asOf: lastTime}, err
}

func foldExecution(l *lot, fill *orderpb.Fill) {
	quantity := dec.FromProto(fill.GetQuantity())
	if fill.GetSide() == orderpb.Side_SIDE_SELL {
		quantity = new(big.Rat).Neg(quantity)
	}
	costbasis.Fold(l, quantity, dec.FromProto(fill.GetPrice()))
}

func verifyPositionDuplicate(ctx context.Context, tx pgx.Tx, portfolio string, fill *orderpb.Fill) error {
	var data []byte
	var originalPortfolio string
	err := tx.QueryRow(ctx, `SELECT portfolio_id,fill FROM position_execution_history WHERE execution_key=$1`, fillfact.ExecutionKey(fill)).Scan(&originalPortfolio, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		if fill.GetRecovery() != nil {
			return ErrPositionHistoryUnknown
		}
		return nil
	}
	if err != nil {
		return err
	}
	var original orderpb.Fill
	if err := proto.Unmarshal(data, &original); err != nil {
		return err
	}
	if originalPortfolio != portfolio {
		return fillfact.ErrExecutionIdentityConflict
	}
	if fill.GetRecovery().GetFeeApproval() != nil {
		_, _, err := fillfact.RecordFeeRevision(ctx, tx, "position", &original, fill)
		return err
	}
	head, err := fillfact.FeeHead(ctx, tx, "position", &original)
	if err != nil {
		return err
	}
	if !fillfact.SameExecution(head, fill) && !(fill.Recovery == nil && fillfact.SameExecution(&original, fill)) {
		return fillfact.ErrExecutionIdentityConflict
	}
	return nil
}

func latestExecutionMark(ctx context.Context, tx pgx.Tx, portfolio string, fill *orderpb.Fill) (*big.Rat, time.Time, error) {
	var data []byte
	var at time.Time
	err := tx.QueryRow(ctx, `SELECT execution_time,fill FROM position_execution_history
		WHERE portfolio_id=$1 AND venue=$2 AND instrument_id=$3 ORDER BY execution_time DESC,execution_key DESC LIMIT 1`,
		portfolio, fill.GetVenue(), fill.GetInstrumentId()).Scan(&at, &data)
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
