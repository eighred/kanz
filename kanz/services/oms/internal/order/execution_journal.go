package order

import (
	"context"
	"errors"
	"fmt"

	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// journalExecution captures the same execution that the order transaction
// announces. Older claims without a fill FACT remain unproven; recovery must
// never manufacture their price, fee, or execution time from an aggregate.
func journalExecution(ctx context.Context, tx pgx.Tx, orderID, fillID string, records []outbox.Record) error {
	fill, err := announcedExecution(orderID, fillID, records)
	if err != nil || fill == nil {
		return err
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(fill)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO order_executions
		(order_id, fill_id, venue, venue_account_id, instrument_id, venue_execution_id, fill)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, orderID, fillID, fill.GetVenue(),
		fill.GetVenueAccountId(), fill.GetInstrumentId(), fill.GetVenueExecutionId(), payload)
	if err != nil {
		return fmt.Errorf("journal execution: %w", err)
	}
	return nil
}

func announcedExecution(orderID, fillID string, records []outbox.Record) (*orderpb.Fill, error) {
	var found *orderpb.Fill
	for _, record := range records {
		var fill *orderpb.Fill
		switch record.EventType {
		case fillfact.SubjectRecovered:
			var err error
			fill, _, err = fillfact.DecodeRecovery(record.Payload)
			if err != nil {
				return nil, err
			}
		case fillfact.SubjectFilled:
			var event orderpb.OrderFilled
			if err := proto.Unmarshal(record.Payload, &event); err != nil {
				return nil, err
			}
			fill = event.GetFill()
		case fillfact.SubjectPartiallyFilled:
			var event orderpb.OrderPartiallyFilled
			if err := proto.Unmarshal(record.Payload, &event); err != nil {
				return nil, err
			}
			fill = event.GetFill()
		default:
			continue
		}
		if fill.GetFillId() != fillID || fill.GetOrderId() != orderID {
			return nil, errors.New("oms: execution journal and claimed fill disagree")
		}
		if found != nil {
			return nil, errors.New("oms: one fill claim cannot announce multiple executions")
		}
		found = fill
	}
	return found, nil
}

func verifyDuplicateExecution(ctx context.Context, tx pgx.Tx, fill *orderpb.Fill) error {
	if fill == nil {
		return nil
	}
	var data []byte
	var err error
	if fillfact.ExecutionKey(fill) != fill.GetFillId() {
		err = tx.QueryRow(ctx, `SELECT fill FROM order_executions WHERE venue=$1 AND venue_account_id=$2
			AND instrument_id=$3 AND venue_execution_id=$4`, fill.GetVenue(), fill.GetVenueAccountId(),
			fill.GetInstrumentId(), fill.GetVenueExecutionId()).Scan(&data)
	} else {
		err = tx.QueryRow(ctx, `SELECT fill FROM order_executions WHERE order_id=$1 AND fill_id=$2`, fill.GetOrderId(), fill.GetFillId()).Scan(&data)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} // Legacy claim: recovery still requires independent evidence.
	if err != nil {
		return err
	}
	var original orderpb.Fill
	if err := proto.Unmarshal(data, &original); err != nil {
		return err
	}
	if !fillfact.SameExecution(&original, fill) {
		head, err := fillfact.FeeHead(ctx, tx, "oms", &original)
		if err != nil {
			return err
		}
		if !fillfact.SameExecution(head, fill) {
			return fillfact.ErrExecutionIdentityConflict
		}
	}
	return nil
}
